package smartroute

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/protocol/group"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json/badoption"
	N "github.com/sagernet/sing/common/network"
)

const (
	AlgorithmComposite = "composite"
	AlgorithmLCBRobust = "lcb_robust"
	AlgorithmStickySLA = "sticky_sla"
	AlgorithmMOSITU    = "mos_itu"
	AlgorithmLowestRTT = "lowest_rtt"

	defaultJitterWeight      = 1.0
	defaultLossWeight        = 300.0
	defaultCostWeight        = 25.0
	defaultBandwidthWeight   = 40.0
	defaultMaxLatency        = uint16(500)
	defaultMaxLoss           = 0.20
	defaultHoldTime          = 30 * time.Second
	congestionWarnRatio      = 0.60
	congestionOverflowRatio  = 0.95
	hysteresisRelativeFactor = 0.10
)

type SmartRouteConfig struct {
	Algorithm        string
	RawPolicies      []URLTestPolicy
	Policies         []compiledPolicy
	JitterWeight     float64
	LossWeight       float64
	CostWeight       float64
	BandwidthWeight  float64
	MaxLatency       uint16
	MaxLoss          float64
	FailureThreshold uint8
	HoldTime         time.Duration
	StatWindow       uint8
	PenaltyHalfLife  time.Duration
	BandwidthWindow  time.Duration
}

type compiledPolicy struct {
	matcher         *regexp.Regexp
	tier            int
	preference      float64
	maxBandwidthBps uint64
	cost            float64
}

type outboundPolicy struct {
	tier            int
	preference      float64
	maxBandwidthBps uint64
	cost            float64
}

var defaultOutboundPolicy = outboundPolicy{
	tier:            1,
	preference:      0,
	maxBandwidthBps: 0,
	cost:            1.0,
}

func NewSmartRouteConfig(options URLTestOptions) (SmartRouteConfig, error) {
	cfg := SmartRouteConfig{
		Algorithm:        options.Algorithm,
		JitterWeight:     options.JitterWeight,
		LossWeight:       options.LossWeight,
		CostWeight:       options.CostWeight,
		BandwidthWeight:  options.BandwidthWeight,
		MaxLatency:       options.MaxLatency,
		MaxLoss:          options.MaxLoss,
		FailureThreshold: options.FailureThreshold,
		HoldTime:         time.Duration(options.HoldTime),
		StatWindow:       options.StatWindow,
		PenaltyHalfLife:  time.Duration(options.PenaltyHalfLife),
		BandwidthWindow:  time.Duration(options.BandwidthWindow),
	}
	cfg.ensureDefaults()

	if len(options.Policies) > 0 {
		cfg.RawPolicies = make([]URLTestPolicy, len(options.Policies))
		copy(cfg.RawPolicies, options.Policies)
		cfg.Policies = make([]compiledPolicy, 0, len(options.Policies))
		for i, p := range options.Policies {
			pattern := p.Match
			if pattern == "" {
				pattern = ".*"
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return SmartRouteConfig{}, E.Cause(err, "compile policy[", i, "] match regex")
			}
			tier := p.Tier
			if tier <= 0 {
				tier = 1
			}
			cost := p.Cost
			if cost <= 0 {
				cost = 1.0
			}
			cfg.Policies = append(cfg.Policies, compiledPolicy{
				matcher:         re,
				tier:            tier,
				preference:      float64(p.Preference),
				maxBandwidthBps: uint64(p.MaxBandwidthMbps) * 1_000_000,
				cost:            cost,
			})
		}
	}
	return cfg, nil
}

func (c *SmartRouteConfig) ensureDefaults() {
	switch c.Algorithm {
	case AlgorithmComposite, AlgorithmLCBRobust, AlgorithmStickySLA, AlgorithmMOSITU, AlgorithmLowestRTT:
	default:
		c.Algorithm = AlgorithmComposite
	}
	if c.JitterWeight <= 0 {
		c.JitterWeight = defaultJitterWeight
	}
	if c.LossWeight <= 0 {
		c.LossWeight = defaultLossWeight
	}
	if c.CostWeight <= 0 {
		c.CostWeight = defaultCostWeight
	}
	if c.BandwidthWeight <= 0 {
		c.BandwidthWeight = defaultBandwidthWeight
	}
	if c.MaxLatency == 0 {
		c.MaxLatency = defaultMaxLatency
	}
	if c.MaxLoss <= 0 {
		c.MaxLoss = defaultMaxLoss
	}
	if c.FailureThreshold == 0 {
		c.FailureThreshold = DefaultFailureThreshold
	}
	if c.HoldTime <= 0 {
		c.HoldTime = defaultHoldTime
	}
	if c.StatWindow == 0 {
		c.StatWindow = DefaultStatWindow
	}
	if c.PenaltyHalfLife <= 0 {
		c.PenaltyHalfLife = DefaultPenaltyHalfLife
	}
	if c.BandwidthWindow <= 0 {
		c.BandwidthWindow = DefaultBandwidthWindow
	}
}

func (c *SmartRouteConfig) matchPolicy(tag string) outboundPolicy {
	for _, p := range c.Policies {
		if p.matcher != nil && p.matcher.MatchString(tag) {
			return outboundPolicy{
				tier:            p.tier,
				preference:      p.preference,
				maxBandwidthBps: p.maxBandwidthBps,
				cost:            p.cost,
			}
		}
	}
	return defaultOutboundPolicy
}

type outboundEvaluation struct {
	outbound    adapter.Outbound
	policy      outboundPolicy
	healthy     bool
	withinSLA   bool
	congested   bool
	circuitOpen bool
	rtt         float64
	jitter      float64
	lossRate    float64
	penalty     float64
	slaExtra    float64
	utilization float64
	bwPenalty   float64
	costPenalty float64
	score       float64
}

func (e outboundEvaluation) formatBreakdown() string {
	if e.outbound == nil {
		return "none"
	}
	if !e.healthy {
		state := "down"
		if e.circuitOpen {
			state = "circuit_open"
		}
		return fmt.Sprintf("%s[T%d %s]", e.outbound.Tag(), e.policy.tier, state)
	}
	return fmt.Sprintf("%s[T%d cost=%.1f = rtt:%.0fms + sla:+%.1f(jit:%.1fms,loss:%.0f%%,pen:%.0f) + bw:+%.1f(util:%.0f%%) + cost_pen:%+.1f(%.1fx) + pref:%+.0f]",
		e.outbound.Tag(),
		e.policy.tier,
		e.score,
		e.rtt,
		e.slaExtra,
		e.jitter,
		e.lossRate*100,
		e.penalty,
		e.bwPenalty,
		e.utilization*100,
		e.costPenalty,
		e.policy.cost,
		e.policy.preference,
	)
}

func (g *URLTestGroup) evaluateOutbound(detour adapter.Outbound, network string) outboundEvaluation {
	g.smartRoute.ensureDefaults()
	policy := g.smartRoute.matchPolicy(detour.Tag())
	realTag := group.RealTag(detour, network)
	if realTag == "" {
		return outboundEvaluation{outbound: detour, policy: policy}
	}
	snap, loaded := g.metrics.LoadLinkMetrics(realTag)
	if !loaded || !snap.Available {
		return outboundEvaluation{outbound: detour, policy: policy, circuitOpen: snap.CircuitOpen}
	}

	rtt := snap.SmoothedRTT
	if rtt <= 0 {
		rtt = float64(snap.LastDelay)
	}
	var slaExtra float64
	switch g.smartRoute.Algorithm {
	case AlgorithmLCBRobust:
		// P95 Upper Confidence Bound (UCB) with sample uncertainty & non-linear loss damping
		samples := float64(snap.SampleCount)
		if samples < 1 {
			samples = 1
		}
		sampleFactor := 1.0 + 1.0/math.Sqrt(samples)
		tailJitter := 1.65 * g.smartRoute.JitterWeight * snap.Jitter * sampleFactor
		lossDenom := math.Max(0.25, (1.0-snap.LossRate)*(1.0-snap.LossRate))
		effectiveRTT := (rtt + tailJitter) / lossDenom
		slaExtra = (effectiveRTT - rtt) + g.smartRoute.LossWeight*snap.LossRate + snap.Penalty
	case AlgorithmMOSITU:
		// ITU-T G.107 E-Model: 177.3ms knee-point nonlinear delay impairment & burst loss model
		dEff := rtt + 2.0*g.smartRoute.JitterWeight*snap.Jitter
		var kneePenalty float64
		if dEff > 177.3 {
			kneePenalty = (dEff - 177.3) * 1.8
		}
		lossImpairment := g.smartRoute.LossWeight * (snap.LossRate / (snap.LossRate + 0.08)) * 1.5
		slaExtra = (dEff - rtt) + kneePenalty + lossImpairment + snap.Penalty
	case AlgorithmLowestRTT:
		// Pure Smoothed RTT priority (ignores jitter and financial cost multiplier)
		slaExtra = g.smartRoute.LossWeight*snap.LossRate*0.5 + snap.Penalty
	default: // AlgorithmComposite & AlgorithmStickySLA
		slaExtra = g.smartRoute.JitterWeight*snap.Jitter + g.smartRoute.LossWeight*snap.LossRate + snap.Penalty
	}
	slaRTT := rtt + slaExtra

	var utilization, bwPenalty float64
	var congested bool
	if policy.maxBandwidthBps > 0 && snap.ThroughputBps > 0 {
		utilization = float64(snap.ThroughputBps) / float64(policy.maxBandwidthBps)
		if utilization >= congestionOverflowRatio {
			congested = true
			bwPenalty = g.smartRoute.BandwidthWeight * 10.0
		} else if utilization > congestionWarnRatio {
			bwPenalty = g.smartRoute.BandwidthWeight * ((utilization - congestionWarnRatio) / (1.0 - utilization))
		}
	}

	costPenalty := g.smartRoute.CostWeight * (policy.cost - 1.0)
	if g.smartRoute.Algorithm == AlgorithmLowestRTT {
		costPenalty = 0
	}
	score := slaRTT + policy.preference + bwPenalty + costPenalty
	if score < 1.0 {
		score = 1.0
	}

	withinSLA := !congested &&
		rtt <= float64(g.smartRoute.MaxLatency) &&
		snap.LossRate <= g.smartRoute.MaxLoss

	return outboundEvaluation{
		outbound:    detour,
		policy:      policy,
		healthy:     true,
		withinSLA:   withinSLA,
		congested:   congested,
		circuitOpen: snap.CircuitOpen,
		rtt:         rtt,
		jitter:      snap.Jitter,
		lossRate:    snap.LossRate,
		penalty:     snap.Penalty,
		slaExtra:    slaExtra,
		utilization: utilization,
		bwPenalty:   bwPenalty,
		costPenalty: costPenalty,
		score:       score,
	}
}

func (g *URLTestGroup) isCurrentOutboundCongested(current adapter.Outbound, network string) bool {
	if current == nil {
		return false
	}
	eval := g.evaluateOutbound(current, network)
	return !eval.healthy || eval.congested
}

type smartRouteDecision struct {
	selected           adapter.Outbound
	exists             bool
	overflow           bool
	reason             string
	currentEval        outboundEvaluation
	bestEval           outboundEvaluation
	deltaScore         float64
	requiredHysteresis float64
	holdRemaining      time.Duration
}

func (g *URLTestGroup) selectSmartRoute(network string, current adapter.Outbound, lastSwitchTime time.Time, ignoreHoldTime bool) (adapter.Outbound, bool, bool) {
	d := g.selectSmartRouteWithDecision(network, current, lastSwitchTime, ignoreHoldTime)
	return d.selected, d.exists, d.overflow
}

func (g *URLTestGroup) selectAlternateOutbound(network string, excludeTag string) adapter.Outbound {
	g.smartRoute.ensureDefaults()
	var evaluations []outboundEvaluation
	for _, detour := range g.outbounds {
		if !common.Contains(detour.Network(), network) {
			continue
		}
		if detour.Tag() == excludeTag || group.RealTag(detour, network) == excludeTag {
			continue
		}
		eval := g.evaluateOutbound(detour, network)
		if eval.healthy {
			evaluations = append(evaluations, eval)
		}
	}
	if len(evaluations) == 0 {
		for _, detour := range g.outbounds {
			if !common.Contains(detour.Network(), network) {
				continue
			}
			if detour.Tag() == excludeTag || group.RealTag(detour, network) == excludeTag {
				continue
			}
			return detour
		}
		return nil
	}
	bestSLATier := math.MaxInt
	for _, eval := range evaluations {
		if eval.withinSLA && eval.policy.tier < bestSLATier {
			bestSLATier = eval.policy.tier
		}
	}
	var bestCandidate outboundEvaluation
	var foundBest bool
	if bestSLATier != math.MaxInt {
		for _, eval := range evaluations {
			if eval.withinSLA && eval.policy.tier == bestSLATier {
				if !foundBest || eval.score < bestCandidate.score {
					bestCandidate = eval
					foundBest = true
				}
			}
		}
	} else {
		for _, eval := range evaluations {
			if !eval.congested {
				if !foundBest || eval.policy.tier < bestCandidate.policy.tier || (eval.policy.tier == bestCandidate.policy.tier && eval.score < bestCandidate.score) {
					bestCandidate = eval
					foundBest = true
				}
			}
		}
		if !foundBest {
			for _, eval := range evaluations {
				if !foundBest || eval.score < bestCandidate.score {
					bestCandidate = eval
					foundBest = true
				}
			}
		}
	}
	return bestCandidate.outbound
}

func (g *URLTestGroup) selectSmartRouteWithDecision(network string, current adapter.Outbound, lastSwitchTime time.Time, ignoreHoldTime bool) smartRouteDecision {
	g.smartRoute.ensureDefaults()
	var evaluations []outboundEvaluation
	var currentEval outboundEvaluation
	for _, detour := range g.outbounds {
		if !common.Contains(detour.Network(), network) {
			continue
		}
		eval := g.evaluateOutbound(detour, network)
		if eval.healthy {
			evaluations = append(evaluations, eval)
		}
		if current != nil && detour == current {
			currentEval = eval
		}
	}

	if len(evaluations) == 0 {
		for _, detour := range g.outbounds {
			if !common.Contains(detour.Network(), network) {
				continue
			}
			return smartRouteDecision{selected: detour, exists: false, overflow: false, reason: "no_healthy_fallback"}
		}
		return smartRouteDecision{reason: "no_supported_outbound"}
	}

	// Stage 1: Find the highest-priority Tier (smallest tier number) that has at least one within-SLA candidate.
	bestSLATier := math.MaxInt
	for _, eval := range evaluations {
		if eval.withinSLA && eval.policy.tier < bestSLATier {
			bestSLATier = eval.policy.tier
		}
	}

	var bestCandidate outboundEvaluation
	var foundBest bool
	if bestSLATier != math.MaxInt {
		for _, eval := range evaluations {
			if eval.withinSLA && eval.policy.tier == bestSLATier {
				if !foundBest || eval.score < bestCandidate.score {
					bestCandidate = eval
					foundBest = true
				}
			}
		}
	} else {
		// Spillover: no candidate strictly within SLA + uncongested; pick best uncongested first, then lowest score overall.
		for _, eval := range evaluations {
			if !eval.congested {
				if !foundBest || eval.policy.tier < bestCandidate.policy.tier || (eval.policy.tier == bestCandidate.policy.tier && eval.score < bestCandidate.score) {
					bestCandidate = eval
					foundBest = true
				}
			}
		}
		if !foundBest {
			for _, eval := range evaluations {
				if !foundBest || eval.score < bestCandidate.score {
					bestCandidate = eval
					foundBest = true
				}
			}
		}
	}

	if currentEval.outbound == nil {
		return smartRouteDecision{
			selected:    bestCandidate.outbound,
			exists:      true,
			overflow:    false,
			reason:      "initial_select",
			currentEval: currentEval,
			bestEval:    bestCandidate,
		}
	}
	if !currentEval.healthy {
		return smartRouteDecision{
			selected:    bestCandidate.outbound,
			exists:      true,
			overflow:    false,
			reason:      "failover_down",
			currentEval: currentEval,
			bestEval:    bestCandidate,
		}
	}
	if bestCandidate.outbound == currentEval.outbound {
		return smartRouteDecision{
			selected:    currentEval.outbound,
			exists:      true,
			overflow:    false,
			reason:      "keep_optimal",
			currentEval: currentEval,
			bestEval:    bestCandidate,
		}
	}

	delta := currentEval.score - bestCandidate.score

	// If current outbound is congested while bestCandidate is not congested, spill over without interrupting existing connections.
	if currentEval.congested && !bestCandidate.congested {
		return smartRouteDecision{
			selected:    bestCandidate.outbound,
			exists:      true,
			overflow:    true,
			reason:      "bandwidth_overflow",
			currentEval: currentEval,
			bestEval:    bestCandidate,
			deltaScore:  delta,
		}
	}

	// If current outbound violated SLA while bestCandidate is in a backup tier (cross-tier spillover) and within SLA:
	if !currentEval.withinSLA && bestCandidate.withinSLA && bestCandidate.policy.tier > currentEval.policy.tier {
		if ignoreHoldTime || currentEval.lossRate > g.smartRoute.MaxLoss || lastSwitchTime.IsZero() || time.Since(lastSwitchTime) >= g.smartRoute.HoldTime {
			return smartRouteDecision{
				selected:    bestCandidate.outbound,
				exists:      true,
				overflow:    false,
				reason:      "sla_violation",
				currentEval: currentEval,
				bestEval:    bestCandidate,
				deltaScore:  delta,
			}
		}
		return smartRouteDecision{
			selected:      currentEval.outbound,
			exists:        true,
			overflow:      false,
			reason:        "suppressed_by_hold_time",
			currentEval:   currentEval,
			bestEval:      bestCandidate,
			deltaScore:    delta,
			holdRemaining: g.smartRoute.HoldTime - time.Since(lastSwitchTime),
		}
	}

	// If bestCandidate is in a strictly higher priority tier (smaller number) and within SLA, preempt if hold time passed.
	if bestCandidate.withinSLA && bestCandidate.policy.tier < currentEval.policy.tier {
		if ignoreHoldTime || lastSwitchTime.IsZero() || time.Since(lastSwitchTime) >= g.smartRoute.HoldTime {
			return smartRouteDecision{
				selected:    bestCandidate.outbound,
				exists:      true,
				overflow:    false,
				reason:      "tier_preempt",
				currentEval: currentEval,
				bestEval:    bestCandidate,
				deltaScore:  delta,
			}
		}
		return smartRouteDecision{
			selected:      currentEval.outbound,
			exists:        true,
			overflow:      false,
			reason:        "suppressed_by_hold_time",
			currentEval:   currentEval,
			bestEval:      bestCandidate,
			deltaScore:    delta,
			holdRemaining: g.smartRoute.HoldTime - time.Since(lastSwitchTime),
		}
	}

	// If bestCandidate is in a worse tier while currentEval is still within SLA, stay on currentEval.
	if currentEval.withinSLA && bestCandidate.policy.tier > currentEval.policy.tier {
		return smartRouteDecision{
			selected:    currentEval.outbound,
			exists:      true,
			overflow:    false,
			reason:      "keep_higher_tier",
			currentEval: currentEval,
			bestEval:    bestCandidate,
			deltaScore:  delta,
		}
	}

	// Same tier comparison:
	// 1. If AlgorithmStickySLA is active and current route is still within SLA, lock onto current route.
	if g.smartRoute.Algorithm == AlgorithmStickySLA && currentEval.withinSLA && !currentEval.congested {
		return smartRouteDecision{
			selected:    currentEval.outbound,
			exists:      true,
			overflow:    false,
			reason:      "keep_sticky_sla",
			currentEval: currentEval,
			bestEval:    bestCandidate,
			deltaScore:  delta,
		}
	}

	// 2. Apply hysteresis (max of tolerance, 10% relative score, and for lcb_robust the bilateral jitter noise envelope) and hold-down timer.
	hysteresis := math.Max(float64(g.tolerance), currentEval.score*hysteresisRelativeFactor)
	if g.smartRoute.Algorithm == AlgorithmLCBRobust {
		jitterEnvelope := (currentEval.jitter + bestCandidate.jitter) * 1.25
		if jitterEnvelope > hysteresis {
			hysteresis = jitterEnvelope
		}
	}
	if bestCandidate.score+hysteresis >= currentEval.score {
		reason := "keep_current"
		if delta > 1.0 {
			reason = "suppressed_by_hysteresis"
		}
		return smartRouteDecision{
			selected:           currentEval.outbound,
			exists:             true,
			overflow:           false,
			reason:             reason,
			currentEval:        currentEval,
			bestEval:           bestCandidate,
			deltaScore:         delta,
			requiredHysteresis: hysteresis,
		}
	}
	if !ignoreHoldTime && !lastSwitchTime.IsZero() && time.Since(lastSwitchTime) < g.smartRoute.HoldTime {
		return smartRouteDecision{
			selected:           currentEval.outbound,
			exists:             true,
			overflow:           false,
			reason:             "suppressed_by_hold_time",
			currentEval:        currentEval,
			bestEval:           bestCandidate,
			deltaScore:         delta,
			requiredHysteresis: hysteresis,
			holdRemaining:      g.smartRoute.HoldTime - time.Since(lastSwitchTime),
		}
	}

	switchReason := "score_better"
	if !currentEval.withinSLA && bestCandidate.withinSLA {
		switchReason = "sla_violation"
	}
	return smartRouteDecision{
		selected:           bestCandidate.outbound,
		exists:             true,
		overflow:           false,
		reason:             switchReason,
		currentEval:        currentEval,
		bestEval:           bestCandidate,
		deltaScore:         delta,
		requiredHysteresis: hysteresis,
	}
}

const maxSmartRouteEvents = 100

type SmartRouteEvent struct {
	Timestamp          time.Time `json:"timestamp"`
	Network            string    `json:"network"`
	Action             string    `json:"action"` // switch, suppressed, overflow, circuit_breaker, config_update
	Reason             string    `json:"reason"`
	FromTag            string    `json:"from_tag,omitempty"`
	ToTag              string    `json:"to_tag,omitempty"`
	FromTier           int       `json:"from_tier,omitempty"`
	ToTier             int       `json:"to_tier,omitempty"`
	FromCost           float64   `json:"from_cost,omitempty"`
	ToCost             float64   `json:"to_cost,omitempty"`
	DeltaCost          float64   `json:"delta_cost,omitempty"`
	RequiredHysteresis float64   `json:"required_hysteresis,omitempty"`
	DwellMs            int64     `json:"dwell_ms,omitempty"`
	HoldRemainingMs    int64     `json:"hold_remaining_ms,omitempty"`
	Summary            string    `json:"summary"`
	Detail             string    `json:"detail"`
}

type smartRouteDecisionStats struct {
	TotalSwitches        uint32
	FlapSwitches         uint32
	SLAViolationSwitches uint32
	FailoverSwitches     uint32
	TierPreemptSwitches  uint32
	ScoreBetterSwitches  uint32
	SuppressedHysteresis uint32
	SuppressedHoldTime   uint32
	OverflowCount        uint32
	CircuitBreakerTrips  uint32
	TotalDwellMs         int64
	DwellSamples         uint32
}

type SmartRouteConfigAssessment struct {
	Parameter      string `json:"parameter"`
	Status         string `json:"status"` // ok, warn, info
	Finding        string `json:"finding"`
	Recommendation string `json:"recommendation"`
}

type SmartRouteDiagnostics struct {
	TotalSwitches        uint32                  `json:"total_switches"`
	FlapSwitches         uint32                  `json:"flap_switches"`
	SuppressedHysteresis uint32                  `json:"suppressed_hysteresis"`
	SuppressedHoldTime   uint32                  `json:"suppressed_hold_time"`
	SLAViolationSwitches uint32                  `json:"sla_violation_switches"`
	CircuitBreakerTrips  uint32                  `json:"circuit_breaker_trips"`
	OverflowCount        uint32                  `json:"overflow_count"`
	AvgDwellSeconds      float64                 `json:"avg_dwell_seconds"`
	Assessments          []SmartRouteConfigAssessment `json:"assessments"`
}

func (g *URLTestGroup) appendEventLocked(ev SmartRouteEvent) {
	if len(g.events) >= maxSmartRouteEvents {
		copy(g.events, g.events[1:])
		g.events[len(g.events)-1] = ev
	} else {
		g.events = append(g.events, ev)
	}
}

func (g *URLTestGroup) observeDecision(network string, d smartRouteDecision, prevSelected adapter.Outbound, lastSwitch time.Time, now time.Time) {
	if d.selected == nil {
		return
	}
	g.eventsMu.Lock()
	defer g.eventsMu.Unlock()

	// Case 1: Bandwidth overflow state transition
	if d.overflow {
		targetTag := d.selected.Tag()
		if g.lastOverflowTag != targetTag {
			g.lastOverflowTag = targetTag
			g.stats.OverflowCount++
			fromTag := ""
			if prevSelected != nil {
				fromTag = prevSelected.Tag()
			}
			summary := fmt.Sprintf("主路 %s 带宽满载 (水位 %.0f%% ≥ 95%%)，新连接无损溢出至 %s", fromTag, d.currentEval.utilization*100, targetTag)
			detail := fmt.Sprintf("from=%s -> spillover=%s", d.currentEval.formatBreakdown(), d.bestEval.formatBreakdown())
			if g.logger != nil {
				g.logger.Warn("[SmartRoute][", network, "] bandwidth_overflow: ", summary, " | ", detail)
			}
			g.appendEventLocked(SmartRouteEvent{
				Timestamp: now,
				Network:   network,
				Action:    "overflow",
				Reason:    d.reason,
				FromTag:   fromTag,
				ToTag:     targetTag,
				FromTier:  d.currentEval.policy.tier,
				ToTier:    d.bestEval.policy.tier,
				FromCost:  d.currentEval.score,
				ToCost:    d.bestEval.score,
				DeltaCost: d.deltaScore,
				Summary:   summary,
				Detail:    detail,
			})
		}
		return
	}
	g.lastOverflowTag = ""

	// Case 2: Actual primary outbound switch
	if prevSelected == nil || (d.exists && d.selected != prevSelected) {
		var dwellMs int64
		if prevSelected != nil && !lastSwitch.IsZero() {
			dwellMs = now.Sub(lastSwitch).Milliseconds()
			g.stats.TotalSwitches++
			g.stats.TotalDwellMs += dwellMs
			g.stats.DwellSamples++
			if now.Sub(lastSwitch) < 2*g.smartRoute.HoldTime {
				g.stats.FlapSwitches++
			}
		}
		switch d.reason {
		case "sla_violation":
			g.stats.SLAViolationSwitches++
		case "failover_down":
			g.stats.FailoverSwitches++
		case "tier_preempt":
			g.stats.TierPreemptSwitches++
		case "score_better":
			g.stats.ScoreBetterSwitches++
		}

		fromTag := ""
		if prevSelected != nil {
			fromTag = prevSelected.Tag()
		}
		toTag := d.selected.Tag()

		var summary string
		switch d.reason {
		case "initial_select":
			summary = fmt.Sprintf("初始选路锁定主路 %s (T%d, 综合代价 %.1f)", toTag, d.bestEval.policy.tier, d.bestEval.score)
		case "failover_down":
			summary = fmt.Sprintf("原主路 %s 故障/熔断离线，故障切换至 %s (T%d, 代价 %.1f)", fromTag, toTag, d.bestEval.policy.tier, d.bestEval.score)
		case "sla_violation":
			summary = fmt.Sprintf("原主路 %s 违反 SLA 门限 (RTT %.0fms / 丢包 %.0f%%)，降级切换至 %s (代价 %.1f)", fromTag, d.currentEval.rtt, d.currentEval.lossRate*100, toTag, d.bestEval.score)
		case "tier_preempt":
			summary = fmt.Sprintf("高优先级线路 %s (T%d) 恢复 SLA，抢占回切替下 %s (T%d)，驻留 %.1fs", toTag, d.bestEval.policy.tier, fromTag, d.currentEval.policy.tier, float64(dwellMs)/1000)
		case "score_better":
			summary = fmt.Sprintf("同级更优切换 %s -> %s：代价降低 %.1f ≥ 容差门限 %.1f，原路驻留 %.1fs", fromTag, toTag, d.deltaScore, d.requiredHysteresis, float64(dwellMs)/1000)
		default:
			summary = fmt.Sprintf("主路切换 %s -> %s (%s)", fromTag, toTag, d.reason)
		}
		detail := fmt.Sprintf("from=%s -> to=%s | delta=%.1f req_tol=%.1f dwell=%dms",
			d.currentEval.formatBreakdown(), d.bestEval.formatBreakdown(), d.deltaScore, d.requiredHysteresis, dwellMs)

		if g.logger != nil {
			g.logger.Info("[SmartRoute][", network, "] switch(", d.reason, "): ", summary, " | ", detail)
		}
		g.appendEventLocked(SmartRouteEvent{
			Timestamp:          now,
			Network:            network,
			Action:             "switch",
			Reason:             d.reason,
			FromTag:            fromTag,
			ToTag:              toTag,
			FromTier:           d.currentEval.policy.tier,
			ToTier:             d.bestEval.policy.tier,
			FromCost:           d.currentEval.score,
			ToCost:             d.bestEval.score,
			DeltaCost:          d.deltaScore,
			RequiredHysteresis: d.requiredHysteresis,
			DwellMs:            dwellMs,
			Summary:            summary,
			Detail:             detail,
		})
		return
	}

	// Case 3: Suppressed switch (key diagnostic signal for evaluating tolerance & hold_time)
	if d.reason == "suppressed_by_hysteresis" || d.reason == "suppressed_by_hold_time" {
		if d.reason == "suppressed_by_hysteresis" {
			g.stats.SuppressedHysteresis++
		} else {
			g.stats.SuppressedHoldTime++
		}
		fromTag := d.currentEval.outbound.Tag()
		toTag := d.bestEval.outbound.Tag()
		dedupKey := fmt.Sprintf("%s:%s->%s", d.reason, fromTag, toTag)
		if g.lastSuppressedKey == dedupKey && now.Sub(g.lastSuppressedAt) < 15*time.Second {
			return
		}
		g.lastSuppressedKey = dedupKey
		g.lastSuppressedAt = now

		var summary string
		if d.reason == "suppressed_by_hysteresis" {
			summary = fmt.Sprintf("容差拦截：候选 %s 代价(%.1f) 低于当前 %s(%.1f)，但差值 %.1f < 容差门限 %.1f，保持不切",
				toTag, d.bestEval.score, fromTag, d.currentEval.score, d.deltaScore, d.requiredHysteresis)
		} else {
			summary = fmt.Sprintf("驻留期拦截：候选 %s 代价(%.1f) 已满足切换条件(差值 %.1f)，但距上次切换尚余 %.1fs 驻留保护期",
				toTag, d.bestEval.score, d.deltaScore, d.holdRemaining.Seconds())
		}
		detail := fmt.Sprintf("current=%s vs candidate=%s | delta=%.1f req_tol=%.1f hold_rem=%dms",
			d.currentEval.formatBreakdown(), d.bestEval.formatBreakdown(), d.deltaScore, d.requiredHysteresis, d.holdRemaining.Milliseconds())

		if g.logger != nil {
			g.logger.Debug("[SmartRoute][", network, "] ", d.reason, ": ", summary, " | ", detail)
		}
		g.appendEventLocked(SmartRouteEvent{
			Timestamp:          now,
			Network:            network,
			Action:             "suppressed",
			Reason:             d.reason,
			FromTag:            fromTag,
			ToTag:              toTag,
			FromTier:           d.currentEval.policy.tier,
			ToTier:             d.bestEval.policy.tier,
			FromCost:           d.currentEval.score,
			ToCost:             d.bestEval.score,
			DeltaCost:          d.deltaScore,
			RequiredHysteresis: d.requiredHysteresis,
			HoldRemainingMs:    d.holdRemaining.Milliseconds(),
			Summary:            summary,
			Detail:             detail,
		})
	}
}

func (g *URLTestGroup) recordCircuitBreakerEvent(network string, failedTag string, failoverTag string, err error, probeErr error) {
	g.eventsMu.Lock()
	defer g.eventsMu.Unlock()
	g.stats.CircuitBreakerTrips++
	summary := fmt.Sprintf("被动熔断触发：节点 %s 连续建连失败达阈值 (%d次) 且主动探测失败，立即剔除并快切至 %s",
		failedTag, g.smartRoute.FailureThreshold, failoverTag)
	detail := fmt.Sprintf("failed=%s failover=%s threshold=%d dial_err=%v probe_err=%v", failedTag, failoverTag, g.smartRoute.FailureThreshold, err, probeErr)
	if g.logger != nil {
		g.logger.Warn("[SmartRoute][", network, "] passive_circuit_trip: ", summary, " | ", detail)
	}
	g.appendEventLocked(SmartRouteEvent{
		Timestamp: time.Now(),
		Network:   network,
		Action:    "circuit_breaker",
		Reason:    "passive_circuit_trip",
		FromTag:   failedTag,
		ToTag:     failoverTag,
		Summary:   summary,
		Detail:    detail,
	})
}

func (g *URLTestGroup) recordProbeVerifiedEvent(network string, nodeTag string, probeDelay uint16, smoothedRTT uint16, triggerErr error) {
	g.eventsMu.Lock()
	defer g.eventsMu.Unlock()
	summary := fmt.Sprintf("主动探测拦截误熔断：节点 %s 连续被动失败达阈值 (%d次)，但主动探测成功 (%dms)，判定为目标站点问题，保持主路不变",
		nodeTag, g.smartRoute.FailureThreshold, probeDelay)
	detail := fmt.Sprintf("node=%s threshold=%d probe_rtt=%dms smoothed_rtt=%dms trigger_err=%v", nodeTag, g.smartRoute.FailureThreshold, probeDelay, smoothedRTT, triggerErr)
	if g.logger != nil {
		g.logger.Info("[SmartRoute][", network, "] probe_verified_healthy: ", summary, " | ", detail)
	}
	g.appendEventLocked(SmartRouteEvent{
		Timestamp: time.Now(),
		Network:   network,
		Action:    "suppressed",
		Reason:    "probe_verified_healthy",
		FromTag:   nodeTag,
		ToTag:     nodeTag,
		Summary:   summary,
		Detail:    detail,
	})
}

func (g *URLTestGroup) recordConfigUpdateEvent(dto SmartRouteGroupConfigDTO) {
	g.eventsMu.Lock()
	defer g.eventsMu.Unlock()
	summary := fmt.Sprintf("更新选路配置：容差=%d(代价) 驻留期=%dms 统计窗口(探测=%ds,平滑=%d次,惩罚半衰=%ds,带宽=%ds) SLA(≤%dms,丢包≤%.0f%%) 权重(抖动=%.1f,丢包=%.0f,带宽=%.0f,成本=%.0f) 策略=%d条",
		dto.Tolerance, dto.HoldTimeMs,
		g.interval/time.Second, g.smartRoute.StatWindow, g.smartRoute.PenaltyHalfLife/time.Second, g.smartRoute.BandwidthWindow/time.Second,
		dto.MaxLatency, dto.MaxLoss*100,
		dto.JitterWeight, dto.LossWeight, dto.BandwidthWeight, dto.CostWeight, len(dto.Policies))
	detail := fmt.Sprintf("tolerance=%d hold_ms=%d interval=%ds stat_window=%d penalty_hl=%ds bw_win=%ds max_lat=%dms max_loss=%.0f%% weights={jit:%.1f,loss:%.0f,bw:%.0f,cost:%.0f} policies=%d",
		dto.Tolerance, dto.HoldTimeMs,
		g.interval/time.Second, g.smartRoute.StatWindow, g.smartRoute.PenaltyHalfLife/time.Second, g.smartRoute.BandwidthWindow/time.Second,
		dto.MaxLatency, dto.MaxLoss*100,
		dto.JitterWeight, dto.LossWeight, dto.BandwidthWeight, dto.CostWeight, len(dto.Policies))
	if g.logger != nil {
		g.logger.Info("[SmartRoute] config_update: ", summary)
	}
	g.appendEventLocked(SmartRouteEvent{
		Timestamp: time.Now(),
		Network:   "all",
		Action:    "config_update",
		Reason:    "manual_config_update",
		Summary:   summary,
		Detail:    detail,
	})
}

func (g *URLTestGroup) buildDiagnosticsLocked(nodes []SmartRouteNodeStatus) ([]SmartRouteEvent, SmartRouteDiagnostics) {
	eventsCopy := make([]SmartRouteEvent, len(g.events))
	for i := range g.events {
		// Reverse chronological order (newest first) for UI convenience
		eventsCopy[i] = g.events[len(g.events)-1-i]
	}

	var avgDwellSec float64
	if g.stats.DwellSamples > 0 {
		avgDwellSec = (float64(g.stats.TotalDwellMs) / float64(g.stats.DwellSamples)) / 1000.0
	}

	var assessments []SmartRouteConfigAssessment

	// 1. Evaluate Tolerance & Anti-Flapping Hysteresis
	if g.stats.FlapSwitches >= 2 {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "防震荡容差与驻留期",
			Status:         "warn",
			Finding:        fmt.Sprintf("检测到 %d 次短驻留往复切换（平均驻留 %.1fs），存在路由乒乓震荡", g.stats.FlapSwitches, avgDwellSec),
			Recommendation: fmt.Sprintf("建议将容差从 %d 调大至 %d，或将驻留期从 %dms 延长至 %dms，也可通过偏好补贴拉开相近线路代价梯度", g.tolerance, g.tolerance+30, g.smartRoute.HoldTime.Milliseconds(), g.smartRoute.HoldTime.Milliseconds()*2),
		})
	} else if g.stats.SuppressedHysteresis >= 4 && g.stats.ScoreBetterSwitches == 0 {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "防震荡容差与驻留期",
			Status:         "info",
			Finding:        fmt.Sprintf("容差门限 (%d) 已连续拦截 %d 次潜在更优切换，且未发生过同级择优切换", g.tolerance, g.stats.SuppressedHysteresis),
			Recommendation: "若当前主路稳定性优先则配置合理；若希望更积极地跟随低延迟线路，可将容差适当调小（如 25~35）",
		})
	} else {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "防震荡容差与驻留期",
			Status:         "ok",
			Finding:        fmt.Sprintf("切换 %d 次（短震荡 %d 次），容差拦截 %d 次，驻留期拦截 %d 次", g.stats.TotalSwitches, g.stats.FlapSwitches, g.stats.SuppressedHysteresis, g.stats.SuppressedHoldTime),
			Recommendation: "当前容差与驻留时间窗口配置均衡，既能有效过滤延迟毛刺，又未出现乒乓震荡",
		})
	}

	// 2. Evaluate SLA Gate & Jitter/Loss Weights
	var slaDegradedNodes, highExtraCostNodes int
	for _, n := range nodes {
		if n.Healthy && !n.WithinSLA {
			slaDegradedNodes++
		}
		if n.Healthy && (n.SLARTT-n.SmoothedRTT) > n.SmoothedRTT*0.8 && (n.SLARTT-n.SmoothedRTT) > 40 {
			highExtraCostNodes++
		}
	}
	if len(nodes) > 0 && slaDegradedNodes == len(nodes) {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "分层溢出门限与代价权重",
			Status:         "warn",
			Finding:        fmt.Sprintf("当前组内全部 %d 个节点均超出 SLA 门限 (≤%dms, 丢包≤%.0f%%)，导致分层退化为全局溢出选路", len(nodes), g.smartRoute.MaxLatency, g.smartRoute.MaxLoss*100),
			Recommendation: "建议根据实际物理链路基线放宽最大延迟或最大丢包率，以恢复层级硬约束",
		})
	} else if highExtraCostNodes > len(nodes)/2 && len(nodes) > 1 {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "分层溢出门限与代价权重",
			Status:         "info",
			Finding:        fmt.Sprintf("超过半数节点 (%d/%d) 的抖动/丢包附加代价超过基础延迟的 80%%", highExtraCostNodes, len(nodes)),
			Recommendation: "当前选路主要由抖动与丢包主导；若探针本身存在偶发抖动，可适当下调抖动权重或丢包权重防止过度惩罚",
		})
	} else {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "分层溢出门限与代价权重",
			Status:         "ok",
			Finding:        fmt.Sprintf("SLA 违约切换 %d 次，当前 %d/%d 节点处于达标水位内", g.stats.SLAViolationSwitches, len(nodes)-slaDegradedNodes, len(nodes)),
			Recommendation: "溢出门限与抖动/丢包代价权重与当前线路物理质量匹配良好",
		})
	}

	// 3. Evaluate Passive Circuit Breaker Threshold
	if g.stats.CircuitBreakerTrips >= 3 {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "被动熔断连续失败阈值",
			Status:         "warn",
			Finding:        fmt.Sprintf("已触发 %d 次被动建连熔断 (当前阈值: 连续 %d 次失败)", g.stats.CircuitBreakerTrips, g.smartRoute.FailureThreshold),
			Recommendation: "若由于用户访问不可达目标站点导致误熔断，建议将连续失败数调高至 5~8；若确为代理节点抖动则当前快切保护有效",
		})
	} else {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "被动熔断连续失败阈值",
			Status:         "ok",
			Finding:        fmt.Sprintf("连续 %d 次被动失败触发熔断（累计触发 %d 次）", g.smartRoute.FailureThreshold, g.stats.CircuitBreakerTrips),
			Recommendation: "被动熔断阈值合理，可容忍偶发目标站点拒绝连接，同时在链路真断时快速切走",
		})
	}

	// 4. Evaluate Statistical & Smoothing Windows
	var highJitterNodes, healthyNodes int
	for _, n := range nodes {
		if n.Healthy {
			healthyNodes++
			if n.Jitter >= 80 {
				highJitterNodes++
			}
		}
	}
	if healthyNodes > 0 && highJitterNodes == healthyNodes && (g.smartRoute.StatWindow < 12 || g.tolerance < 150) {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "统计采样与平滑窗口",
			Status:         "warn",
			Finding:        fmt.Sprintf("检测到全部 %d 个可用节点均呈大范围抖动 (±80ms+，如 100~1000ms 波动)，当前平滑窗口 (%d 次) 或容差 (%d) 易受波峰波谷相位差诱导切换", healthyNodes, g.smartRoute.StatWindow, g.tolerance),
			Recommendation: "全线路剧烈波动场景建议：将平滑窗口调大至 12~16 次、容差调至 200~300、驻留期延至 60~90s、抖动权重降至 0.5、最大延迟放宽至 800~900ms",
		})
	} else if g.smartRoute.StatWindow <= 2 && g.stats.FlapSwitches >= 1 {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "统计采样与平滑窗口",
			Status:         "warn",
			Finding:        fmt.Sprintf("当前质量平滑窗口过小 (%d 次采样)，单次探测毛刺权重过高，加剧了路由震荡", g.smartRoute.StatWindow),
			Recommendation: "建议将质量平滑窗口调高至 6~10 次，以充分过滤瞬时延迟毛刺",
		})
	} else if g.interval > 120*time.Second {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "统计采样与平滑窗口",
			Status:         "info",
			Finding:        fmt.Sprintf("主动探测周期较长 (%.0fs)，平滑窗口 %d 次采样需较长时间收敛", g.interval.Seconds(), g.smartRoute.StatWindow),
			Recommendation: "若需及时感知线路劣化或恢复，可将探测周期缩短至 15~60s",
		})
	} else {
		assessments = append(assessments, SmartRouteConfigAssessment{
			Parameter:      "统计采样与平滑窗口",
			Status:         "ok",
			Finding:        fmt.Sprintf("探测周期 %.0fs · 平滑窗口 %d 次 · 惩罚半衰期 %.0fs · 带宽窗口 %.0fs", g.interval.Seconds(), g.smartRoute.StatWindow, g.smartRoute.PenaltyHalfLife.Seconds(), g.smartRoute.BandwidthWindow.Seconds()),
			Recommendation: "多维采样周期与平滑衰减时间窗口配合协调，兼顾收敛灵敏度与抗毛刺能力",
		})
	}

	return eventsCopy, SmartRouteDiagnostics{
		TotalSwitches:        g.stats.TotalSwitches,
		FlapSwitches:         g.stats.FlapSwitches,
		SuppressedHysteresis: g.stats.SuppressedHysteresis,
		SuppressedHoldTime:   g.stats.SuppressedHoldTime,
		SLAViolationSwitches: g.stats.SLAViolationSwitches,
		CircuitBreakerTrips:  g.stats.CircuitBreakerTrips,
		OverflowCount:        g.stats.OverflowCount,
		AvgDwellSeconds:      avgDwellSec,
		Assessments:          assessments,
	}
}

type SmartRouteNodeStatus struct {
	Tag                        string    `json:"tag"`
	Type                       string    `json:"type"`
	Tier                       int       `json:"tier"`
	Preference                 float64   `json:"preference"`
	MaxBandwidthMbps           uint32    `json:"max_bandwidth_mbps"`
	Cost                       float64   `json:"cost"`
	Healthy                    bool      `json:"healthy"`
	WithinSLA                  bool      `json:"within_sla"`
	Congested                  bool      `json:"congested"`
	CircuitOpen                bool      `json:"circuit_open"`
	SelectedTCP                bool      `json:"selected_tcp"`
	SelectedUDP                bool      `json:"selected_udp"`
	ActiveTCP                  bool      `json:"active_tcp"`
	SampleCount                uint32    `json:"sample_count"`
	LastDelay                  uint16    `json:"last_delay"`
	SmoothedRTT                float64   `json:"smoothed_rtt"`
	Jitter                     float64   `json:"jitter"`
	LossRate                   float64   `json:"loss_rate"`
	Penalty                    float64   `json:"penalty"`
	SLARTT                     float64   `json:"sla_rtt"`
	BandwidthPenalty           float64   `json:"bandwidth_penalty"`
	CostPenalty                float64   `json:"cost_penalty"`
	EffectiveScore             float64   `json:"effective_score"`
	ScoreHistory               []float64 `json:"score_history,omitempty"`
	ThroughputBps              uint64    `json:"throughput_bps"`
	PeakThroughputBps          uint64    `json:"peak_throughput_bps"`
	Utilization                float64   `json:"utilization"`
	TotalBytes                 uint64    `json:"total_bytes"`
	ConsecutiveProbeFailures   uint32    `json:"consecutive_probe_failures"`
	ConsecutivePassiveFailures uint32    `json:"consecutive_passive_failures"`
	LastUpdated                time.Time `json:"last_updated"`
	LastPassiveSuccess         time.Time `json:"last_passive_success"`
}

type SmartRouteGroupConfigDTO struct {
	Algorithm         string          `json:"algorithm"`
	IntervalMs        int64           `json:"interval_ms"`
	Tolerance         uint16          `json:"tolerance"`
	JitterWeight      float64         `json:"jitter_weight"`
	LossWeight        float64         `json:"loss_weight"`
	CostWeight        float64         `json:"cost_weight"`
	BandwidthWeight   float64         `json:"bandwidth_weight"`
	MaxLatency        uint16          `json:"max_latency"`
	MaxLoss           float64         `json:"max_loss"`
	FailureThreshold  uint8           `json:"failure_threshold"`
	HoldTimeMs        int64           `json:"hold_time_ms"`
	StatWindow        uint8           `json:"stat_window"`
	PenaltyHalfLifeMs int64           `json:"penalty_half_life_ms"`
	BandwidthWindowMs int64           `json:"bandwidth_window_ms"`
	Policies          []URLTestPolicy `json:"policies"`
}

type SmartRouteGroupStatus struct {
	Tag          string                   `json:"tag"`
	URL          string                   `json:"url"`
	IntervalMs   int64                    `json:"interval_ms"`
	SelectedTCP  string                   `json:"selected_tcp"`
	SelectedUDP  string                   `json:"selected_udp"`
	ActiveTCP    string                   `json:"active_tcp"`
	Overflowing  bool                     `json:"overflowing"`
	LastSwitchAt time.Time                `json:"last_switch_at"`
	Config       SmartRouteGroupConfigDTO `json:"config"`
	Nodes        []SmartRouteNodeStatus   `json:"nodes"`
	Events       []SmartRouteEvent        `json:"events"`
	Diagnostics  SmartRouteDiagnostics    `json:"diagnostics"`
}

func (s *URLTest) SmartRouteStatus() SmartRouteGroupStatus {
	grp := s.group
	if grp == nil {
		return SmartRouteGroupStatus{Tag: s.Tag()}
	}
	grp.updateAccess.Lock()
	defer grp.updateAccess.Unlock()

	grp.smartRoute.ensureDefaults()
	var selectedTCPTag, selectedUDPTag, activeTCPTag string
	if grp.selectedOutboundTCP != nil {
		selectedTCPTag = grp.selectedOutboundTCP.Tag()
	}
	if grp.selectedOutboundUDP != nil {
		selectedUDPTag = grp.selectedOutboundUDP.Tag()
	}
	activeTCP, _, overflow := grp.selectSmartRoute(N.NetworkTCP, grp.selectedOutboundTCP, grp.lastSwitchTCP, false)
	if activeTCP != nil {
		activeTCPTag = activeTCP.Tag()
	}

	grp.scoreHistoryMu.Lock()
	if time.Since(grp.lastHistoryAt) >= 2*time.Second {
		grp.scoreHistoryMu.Unlock()
		// Read-only status query only updates in-memory trajectory, never writes to cache.db
		grp.recordGroupScoreSnapshotLocked(false, true)
	} else {
		grp.scoreHistoryMu.Unlock()
	}

	nodes := make([]SmartRouteNodeStatus, 0, len(grp.outbounds))
	for _, detour := range grp.outbounds {
		policy := grp.smartRoute.matchPolicy(detour.Tag())
		eval := grp.evaluateOutbound(detour, N.NetworkTCP)
		realTag := group.RealTag(detour, N.NetworkTCP)
		snap, _ := grp.metrics.LoadLinkMetrics(realTag)

		rtt := snap.SmoothedRTT
		if rtt <= 0 {
			rtt = float64(snap.LastDelay)
		}
		var slaRTT, bwPenalty, utilization float64
		if eval.healthy {
			slaRTT = rtt + eval.slaExtra
		}
		bwPenalty = eval.bwPenalty
		utilization = eval.utilization
		costPenalty := eval.costPenalty

		var histCopy []float64
		grp.scoreHistoryMu.Lock()
		if h := grp.scoreHistory[detour.Tag()]; len(h) > 0 {
			histCopy = make([]float64, len(h))
			copy(histCopy, h)
		}
		grp.scoreHistoryMu.Unlock()

		nodes = append(nodes, SmartRouteNodeStatus{
			Tag:                        detour.Tag(),
			Type:                       detour.Type(),
			Tier:                       policy.tier,
			Preference:                 policy.preference,
			MaxBandwidthMbps:           uint32(policy.maxBandwidthBps / 1_000_000),
			Cost:                       policy.cost,
			Healthy:                    eval.healthy,
			WithinSLA:                  eval.withinSLA,
			Congested:                  eval.congested,
			CircuitOpen:                snap.CircuitOpen,
			SelectedTCP:                detour.Tag() == selectedTCPTag,
			SelectedUDP:                detour.Tag() == selectedUDPTag,
			ActiveTCP:                  detour.Tag() == activeTCPTag,
			SampleCount:                snap.SampleCount,
			LastDelay:                  snap.LastDelay,
			SmoothedRTT:                snap.SmoothedRTT,
			Jitter:                     snap.Jitter,
			LossRate:                   snap.LossRate,
			Penalty:                    snap.Penalty,
			SLARTT:                     slaRTT,
			BandwidthPenalty:           bwPenalty,
			CostPenalty:                costPenalty,
			EffectiveScore:             eval.score,
			ScoreHistory:               histCopy,
			ThroughputBps:              snap.ThroughputBps,
			PeakThroughputBps:          snap.PeakThroughputBps,
			Utilization:                utilization,
			TotalBytes:                 snap.TotalBytes,
			ConsecutiveProbeFailures:   snap.ConsecutiveProbeFailures,
			ConsecutivePassiveFailures: snap.ConsecutivePassiveFailures,
			LastUpdated:                snap.LastUpdated,
			LastPassiveSuccess:         snap.LastPassiveSuccess,
		})
	}

	rawPolicies := make([]URLTestPolicy, len(grp.smartRoute.RawPolicies))
	copy(rawPolicies, grp.smartRoute.RawPolicies)

	grp.eventsMu.Lock()
	events, diagnostics := grp.buildDiagnosticsLocked(nodes)
	grp.eventsMu.Unlock()

	return SmartRouteGroupStatus{
		Tag:          s.Tag(),
		URL:          grp.link,
		IntervalMs:   grp.interval.Milliseconds(),
		SelectedTCP:  selectedTCPTag,
		SelectedUDP:  selectedUDPTag,
		ActiveTCP:    activeTCPTag,
		Overflowing:  overflow || (selectedTCPTag != "" && activeTCPTag != "" && selectedTCPTag != activeTCPTag),
		LastSwitchAt: grp.lastSwitchTCP,
		Config: SmartRouteGroupConfigDTO{
			Algorithm:         grp.smartRoute.Algorithm,
			IntervalMs:        grp.interval.Milliseconds(),
			Tolerance:         grp.tolerance,
			JitterWeight:      grp.smartRoute.JitterWeight,
			LossWeight:        grp.smartRoute.LossWeight,
			CostWeight:        grp.smartRoute.CostWeight,
			BandwidthWeight:   grp.smartRoute.BandwidthWeight,
			MaxLatency:        grp.smartRoute.MaxLatency,
			MaxLoss:           grp.smartRoute.MaxLoss,
			FailureThreshold:  grp.smartRoute.FailureThreshold,
			HoldTimeMs:        grp.smartRoute.HoldTime.Milliseconds(),
			StatWindow:        grp.smartRoute.StatWindow,
			PenaltyHalfLifeMs: grp.smartRoute.PenaltyHalfLife.Milliseconds(),
			BandwidthWindowMs: grp.smartRoute.BandwidthWindow.Milliseconds(),
			Policies:          rawPolicies,
		},
		Nodes:       nodes,
		Events:      events,
		Diagnostics: diagnostics,
	}
}

func (s *URLTest) UpdateSmartRouteConfig(dto SmartRouteGroupConfigDTO) error {
	newCfg, err := NewSmartRouteConfig(URLTestOptions{
		Algorithm:        dto.Algorithm,
		Tolerance:        dto.Tolerance,
		Policies:         dto.Policies,
		JitterWeight:     dto.JitterWeight,
		LossWeight:       dto.LossWeight,
		CostWeight:       dto.CostWeight,
		BandwidthWeight:  dto.BandwidthWeight,
		MaxLatency:       dto.MaxLatency,
		MaxLoss:          dto.MaxLoss,
		FailureThreshold: dto.FailureThreshold,
		HoldTime:         badoption.Duration(time.Duration(dto.HoldTimeMs) * time.Millisecond),
		StatWindow:       dto.StatWindow,
		PenaltyHalfLife:  badoption.Duration(time.Duration(dto.PenaltyHalfLifeMs) * time.Millisecond),
		BandwidthWindow:  badoption.Duration(time.Duration(dto.BandwidthWindowMs) * time.Millisecond),
	})
	if err != nil {
		return err
	}
	s.smartRoute = newCfg
	if dto.Tolerance > 0 {
		s.tolerance = dto.Tolerance
	}
	if dto.IntervalMs >= 1000 {
		s.interval = time.Duration(dto.IntervalMs) * time.Millisecond
	}
	if s.group != nil {
		s.group.updateAccess.Lock()
		s.group.smartRoute = newCfg
		s.group.configModifiedByAPI = true
		if dto.Tolerance > 0 {
			s.group.tolerance = dto.Tolerance
		}
		if s.group.metrics != nil {
			s.group.metrics.SetWindowConfig(MetricsWindowConfig{
				StatWindow:      newCfg.StatWindow,
				PenaltyHalfLife: newCfg.PenaltyHalfLife,
				BandwidthWindow: newCfg.BandwidthWindow,
			})
		}
		s.group.updateAccess.Unlock()
		if dto.IntervalMs >= 1000 {
			s.group.SetInterval(time.Duration(dto.IntervalMs) * time.Millisecond)
		}
		s.group.recordConfigUpdateEvent(dto)
		s.group.performUpdateCheck()
		s.group.FlushHotCache()
	}
	return nil
}

const maxScoreHistoryPoints = 50

// persistedGroupState is stored as a single compact key (__sr__:<groupTag>) in cache.db.
// Ephemeral UI time-series (score_history) and cold/expired nodes (>24h) are omitted to prevent bbolt bloat.
type persistedGroupState struct {
	SelectedTCP string                       `json:"tcp,omitempty"`
	SelectedUDP string                       `json:"udp,omitempty"`
	SwitchTCP   int64                        `json:"st,omitempty"`
	SwitchUDP   int64                        `json:"su,omitempty"`
	HotNodes    map[string]hotNodeCacheEntry `json:"nodes,omitempty"`
	Config      *SmartRouteGroupConfigDTO    `json:"cfg,omitempty"`
	UpdatedAt   int64                        `json:"at"`
}

func (g *URLTestGroup) AttachCacheFile(groupTag string, cf adapter.CacheFile) {
	if g == nil || cf == nil {
		return
	}
	now := time.Now()
	g.updateAccess.Lock()
	g.groupTag = groupTag
	g.cacheFile = cf

	if groupTag != "" {
		if raw := cf.LoadSelected(cacheKeyGroupPrefix + groupTag); raw != "" {
			var state persistedGroupState
			if err := json.Unmarshal([]byte(raw), &state); err == nil {
				if state.Config != nil {
					if restoredCfg, cfgErr := NewSmartRouteConfig(URLTestOptions{
						Algorithm:        state.Config.Algorithm,
						Tolerance:        state.Config.Tolerance,
						Policies:         state.Config.Policies,
						JitterWeight:     state.Config.JitterWeight,
						LossWeight:       state.Config.LossWeight,
						CostWeight:       state.Config.CostWeight,
						BandwidthWeight:  state.Config.BandwidthWeight,
						MaxLatency:       state.Config.MaxLatency,
						MaxLoss:          state.Config.MaxLoss,
						FailureThreshold: state.Config.FailureThreshold,
						HoldTime:         badoption.Duration(time.Duration(state.Config.HoldTimeMs) * time.Millisecond),
						StatWindow:       state.Config.StatWindow,
						PenaltyHalfLife:  badoption.Duration(time.Duration(state.Config.PenaltyHalfLifeMs) * time.Millisecond),
						BandwidthWindow:  badoption.Duration(time.Duration(state.Config.BandwidthWindowMs) * time.Millisecond),
					}); cfgErr == nil {
						g.smartRoute = restoredCfg
						g.configModifiedByAPI = true
						if state.Config.Tolerance > 0 {
							g.tolerance = state.Config.Tolerance
						}
						if g.metrics != nil {
							g.metrics.SetWindowConfig(MetricsWindowConfig{
								StatWindow:      restoredCfg.StatWindow,
								PenaltyHalfLife: restoredCfg.PenaltyHalfLife,
								BandwidthWindow: restoredCfg.BandwidthWindow,
							})
						}
					}
				}
				// Restore only active member nodes that exist in current group outbounds
				if len(state.HotNodes) > 0 && g.metrics != nil {
					for _, detour := range g.outbounds {
						realTag := group.RealTag(detour, N.NetworkTCP)
						if realTag == "" {
							realTag = detour.Tag()
						}
						if entry, ok := state.HotNodes[realTag]; ok {
							g.metrics.RestoreHotNodeEntry(realTag, entry, now)
						} else if entry, ok := state.HotNodes[detour.Tag()]; ok {
							g.metrics.RestoreHotNodeEntry(realTag, entry, now)
						}
					}
				}
				if state.SelectedTCP != "" {
					for _, detour := range g.outbounds {
						if detour.Tag() == state.SelectedTCP {
							g.selectedOutboundTCP = detour
							break
						}
					}
				}
				if state.SelectedUDP != "" {
					for _, detour := range g.outbounds {
						if detour.Tag() == state.SelectedUDP {
							g.selectedOutboundUDP = detour
							break
						}
					}
				}
				if state.SwitchTCP > 0 {
					g.lastSwitchTCP = time.Unix(state.SwitchTCP, 0)
				}
				if state.SwitchUDP > 0 {
					g.lastSwitchUDP = time.Unix(state.SwitchUDP, 0)
				}
			}
		} else if sel := cf.LoadSelected(groupTag); sel != "" {
			for _, detour := range g.outbounds {
				if detour.Tag() == sel {
					g.selectedOutboundTCP = detour
					break
				}
			}
		}
	}
	g.updateAccess.Unlock()
	g.performUpdateCheck()
}

func (g *URLTestGroup) FlushHotCache() {
	if g == nil {
		return
	}
	g.updateAccess.Lock()
	defer g.updateAccess.Unlock()
	if !g.cacheDirty && !g.lastCacheFlushAt.IsZero() {
		return
	}
	g.persistGroupStateLocked(time.Now())
}

func (g *URLTestGroup) recordGroupScoreSnapshotLocked(forceFlush bool, readOnly bool) {
	if g == nil {
		return
	}
	now := time.Now()

	g.scoreHistoryMu.Lock()
	if g.scoreHistory == nil {
		g.scoreHistory = make(map[string][]float64, len(g.outbounds))
	}
	for _, detour := range g.outbounds {
		eval := g.evaluateOutbound(detour, N.NetworkTCP)
		if !eval.healthy {
			continue
		}
		rounded := math.Round(eval.score*10) / 10
		series := append(g.scoreHistory[detour.Tag()], rounded)
		if len(series) > maxScoreHistoryPoints {
			series = series[len(series)-maxScoreHistoryPoints:]
		}
		g.scoreHistory[detour.Tag()] = series
		if g.metrics != nil {
			realTag := group.RealTag(detour, N.NetworkTCP)
			g.metrics.RecordEffectiveScore(realTag, rounded)
		}
	}
	g.lastHistoryAt = now
	g.scoreHistoryMu.Unlock()

	if readOnly {
		return
	}
	g.cacheDirty = true
	if forceFlush || g.lastCacheFlushAt.IsZero() || now.Sub(g.lastCacheFlushAt) >= hotCacheFlushInterval {
		g.persistGroupStateLocked(now)
	}
}

func (g *URLTestGroup) persistGroupStateLocked(now time.Time) {
	if g == nil || g.cacheFile == nil || g.groupTag == "" {
		return
	}
	var selectedTCP, selectedUDP string
	if g.selectedOutboundTCP != nil {
		selectedTCP = g.selectedOutboundTCP.Tag()
	}
	if g.selectedOutboundUDP != nil {
		selectedUDP = g.selectedOutboundUDP.Tag()
	}
	var switchTCPUnix, switchUDPUnix int64
	if !g.lastSwitchTCP.IsZero() {
		switchTCPUnix = g.lastSwitchTCP.Unix()
	}
	if !g.lastSwitchUDP.IsZero() {
		switchUDPUnix = g.lastSwitchUDP.Unix()
	}

	type keyedEntry struct {
		key   string
		entry hotNodeCacheEntry
	}
	var candidates []keyedEntry
	if g.metrics != nil {
		seen := make(map[string]bool, len(g.outbounds))
		for _, detour := range g.outbounds {
			realTag := group.RealTag(detour, N.NetworkTCP)
			if realTag == "" {
				realTag = detour.Tag()
			}
			if realTag == "" || seen[realTag] {
				continue
			}
			seen[realTag] = true
			if entry, ok := g.metrics.ExportHotNodeEntry(realTag, now); ok {
				candidates = append(candidates, keyedEntry{key: realTag, entry: entry})
			}
		}
	}
	// Cap hot nodes per group to prevent oversized payloads on huge subscriptions
	if len(candidates) > maxHotNodesPerGroup {
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].key == selectedTCP || candidates[i].key == selectedUDP {
				return true
			}
			if candidates[j].key == selectedTCP || candidates[j].key == selectedUDP {
				return false
			}
			if candidates[i].entry.Trip != candidates[j].entry.Trip {
				return !candidates[i].entry.Trip
			}
			return candidates[i].entry.Score < candidates[j].entry.Score
		})
		candidates = candidates[:maxHotNodesPerGroup]
	}
	var hotNodes map[string]hotNodeCacheEntry
	if len(candidates) > 0 {
		hotNodes = make(map[string]hotNodeCacheEntry, len(candidates))
		for _, c := range candidates {
			hotNodes[c.key] = c.entry
		}
	}

	var cfgDTO *SmartRouteGroupConfigDTO
	if g.configModifiedByAPI {
		rawPolicies := make([]URLTestPolicy, len(g.smartRoute.RawPolicies))
		copy(rawPolicies, g.smartRoute.RawPolicies)
		cfgDTO = &SmartRouteGroupConfigDTO{
			Algorithm:         g.smartRoute.Algorithm,
			IntervalMs:        g.interval.Milliseconds(),
			Tolerance:         g.tolerance,
			JitterWeight:      g.smartRoute.JitterWeight,
			LossWeight:        g.smartRoute.LossWeight,
			CostWeight:        g.smartRoute.CostWeight,
			BandwidthWeight:   g.smartRoute.BandwidthWeight,
			MaxLatency:        g.smartRoute.MaxLatency,
			MaxLoss:           g.smartRoute.MaxLoss,
			FailureThreshold:  g.smartRoute.FailureThreshold,
			HoldTimeMs:        g.smartRoute.HoldTime.Milliseconds(),
			StatWindow:        g.smartRoute.StatWindow,
			PenaltyHalfLifeMs: g.smartRoute.PenaltyHalfLife.Milliseconds(),
			BandwidthWindowMs: g.smartRoute.BandwidthWindow.Milliseconds(),
			Policies:          rawPolicies,
		}
	}

	if len(hotNodes) == 0 && !g.configModifiedByAPI && selectedTCP == "" {
		return
	}
	state := persistedGroupState{
		SelectedTCP: selectedTCP,
		SelectedUDP: selectedUDP,
		SwitchTCP:   switchTCPUnix,
		SwitchUDP:   switchUDPUnix,
		HotNodes:    hotNodes,
		Config:      cfgDTO,
		UpdatedAt:   now.Unix(),
	}
	if data, err := json.Marshal(state); err == nil {
		_ = g.cacheFile.StoreSelected(cacheKeyGroupPrefix+g.groupTag, string(data))
	}
	if selectedTCP != "" {
		_ = g.cacheFile.StoreSelected(g.groupTag, selectedTCP)
	}
	if len(hotNodes) > 0 || g.configModifiedByAPI {
		g.lastCacheFlushAt = now
		g.cacheDirty = false
	}
}
