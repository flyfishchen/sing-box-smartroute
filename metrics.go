package smartroute

import (
	"context"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

const (
	ewmaAlphaRTT            = 0.30
	ewmaBetaJitter          = 0.25
	ewmaGammaLoss           = 0.20
	penaltyPerProbeFailure  = 50.0
	penaltyPerCircuitTrip   = 100.0
	DefaultStatWindow       = uint8(6)
	DefaultPenaltyHalfLife  = 60 * time.Second
	DefaultBandwidthWindow  = 1 * time.Second
	DefaultFailureThreshold = 5
	maxBandwidthWindowSec   = 30
	bwSlotMask              = 31
)

type MetricsWindowConfig struct {
	StatWindow      uint8
	PenaltyHalfLife time.Duration
	BandwidthWindow time.Duration
}

type bwSlot struct {
	sec   atomic.Int64
	bytes atomic.Int64
}

type LinkMetricsSnapshot struct {
	Available                  bool
	CircuitOpen                bool
	SampleCount                uint32
	LastDelay                  uint16
	SmoothedRTT                float64
	Jitter                     float64
	LossRate                   float64
	Penalty                    float64
	ConsecutiveProbeFailures   uint32
	ConsecutivePassiveFailures uint32
	ThroughputBps              uint64
	PeakThroughputBps          uint64
	TotalBytes                 uint64
	LastUpdated                time.Time
	LastPassiveSuccess         time.Time
}

type LinkMetrics struct {
	access                     sync.Mutex
	available                  bool
	circuitOpen                bool
	sampleCount                uint32
	lastDelay                  uint16
	smoothedRTT                float64
	jitter                     float64
	lossRate                   float64
	penalty                    float64
	penaltyUpdatedAt           time.Time
	consecutiveProbeFailures   uint32
	consecutivePassiveFailures uint32
	lastUpdated                time.Time
	lastPassiveSuccess         time.Time
	statWindow                 uint8
	penaltyHalfLife            time.Duration
	bwWindowSec                atomic.Int64

	totalBytes        atomic.Int64
	peakThroughputBps atomic.Uint64
	bwSlots           [32]bwSlot
}

func NewLinkMetrics() *LinkMetrics {
	m := &LinkMetrics{
		statWindow:      DefaultStatWindow,
		penaltyHalfLife: DefaultPenaltyHalfLife,
	}
	m.bwWindowSec.Store(1)
	return m
}

func (m *LinkMetrics) SetWindowConfig(cfg MetricsWindowConfig) {
	m.access.Lock()
	if cfg.StatWindow > 0 {
		m.statWindow = cfg.StatWindow
	} else {
		m.statWindow = DefaultStatWindow
	}
	if cfg.PenaltyHalfLife > 0 {
		m.penaltyHalfLife = cfg.PenaltyHalfLife
	} else {
		m.penaltyHalfLife = DefaultPenaltyHalfLife
	}
	m.access.Unlock()

	bwSec := int64(cfg.BandwidthWindow / time.Second)
	if bwSec <= 0 {
		bwSec = 1
	} else if bwSec > maxBandwidthWindowSec {
		bwSec = maxBandwidthWindowSec
	}
	m.bwWindowSec.Store(bwSec)
}

func (m *LinkMetrics) ewmaFactorsLocked() (alphaRTT, betaJitter, gammaLoss float64) {
	w := m.statWindow
	if w == 0 || w == DefaultStatWindow {
		return ewmaAlphaRTT, ewmaBetaJitter, ewmaGammaLoss
	}
	if w == 1 {
		return 1.0, 0.50, 0.40
	}
	n := float64(w)
	return 2.0 / (n + 1.0), 2.0 / (n + 2.0), 2.0 / (n + 4.0)
}

func (m *LinkMetrics) decayPenaltyLocked(now time.Time) {
	if m.penalty <= 0 || m.penaltyUpdatedAt.IsZero() {
		m.penaltyUpdatedAt = now
		return
	}
	elapsed := now.Sub(m.penaltyUpdatedAt)
	if elapsed <= 0 {
		return
	}
	halfLife := m.penaltyHalfLife
	if halfLife <= 0 {
		halfLife = DefaultPenaltyHalfLife
	}
	decay := math.Pow(0.5, float64(elapsed)/float64(halfLife))
	m.penalty *= decay
	if m.penalty < 0.5 {
		m.penalty = 0
	}
	m.penaltyUpdatedAt = now
}

func (m *LinkMetrics) RecordProbeSuccess(delay uint16, now time.Time) uint16 {
	if now.IsZero() {
		now = time.Now()
	}
	if delay == 0 {
		delay = 1
	}
	m.access.Lock()
	defer m.access.Unlock()

	m.decayPenaltyLocked(now)
	alphaRTT, betaJitter, gammaLoss := m.ewmaFactorsLocked()
	sample := float64(delay)
	if m.sampleCount == 0 {
		m.smoothedRTT = sample
		m.jitter = 0
		m.lossRate = 0
	} else {
		delta := math.Abs(sample - m.smoothedRTT)
		maxDelta := math.Max(m.smoothedRTT*2.5, 300.0)
		if delta > maxDelta {
			delta = maxDelta
		}
		m.jitter = (1.0-betaJitter)*m.jitter + betaJitter*delta
		m.smoothedRTT = (1.0-alphaRTT)*m.smoothedRTT + alphaRTT*sample
		m.lossRate = (1.0 - gammaLoss) * m.lossRate
		if m.lossRate < 0.001 {
			m.lossRate = 0
		}
	}
	m.sampleCount++
	m.lastDelay = delay
	m.available = true
	m.circuitOpen = false
	m.consecutiveProbeFailures = 0
	m.consecutivePassiveFailures = 0
	m.lastUpdated = now

	smoothed := uint16(math.Round(m.smoothedRTT))
	if smoothed == 0 {
		smoothed = 1
	}
	return smoothed
}

func (m *LinkMetrics) RecordProbeFailure(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	m.access.Lock()
	defer m.access.Unlock()

	m.decayPenaltyLocked(now)
	_, _, gammaLoss := m.ewmaFactorsLocked()
	if m.sampleCount == 0 {
		m.lossRate = 1.0
	} else {
		m.lossRate = (1.0-gammaLoss)*m.lossRate + gammaLoss*1.0
	}
	m.penalty += penaltyPerProbeFailure
	m.consecutiveProbeFailures++
	m.available = false
	m.lastUpdated = now
}

func (m *LinkMetrics) RecordPassiveSuccess(dialRTT time.Duration, now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	m.access.Lock()
	defer m.access.Unlock()

	m.decayPenaltyLocked(now)
	m.consecutivePassiveFailures = 0
	m.circuitOpen = false
	m.lastPassiveSuccess = now
}

func (m *LinkMetrics) RecordPassiveFailure(threshold uint8, now time.Time) bool {
	if !m.RecordPassiveFailureReachedThreshold(threshold, now) {
		return false
	}
	m.ConfirmPassiveCircuitBreaker(now)
	return true
}

func (m *LinkMetrics) RecordPassiveFailureReachedThreshold(threshold uint8, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	if threshold == 0 {
		threshold = DefaultFailureThreshold
	}
	m.access.Lock()
	defer m.access.Unlock()

	m.decayPenaltyLocked(now)
	m.consecutivePassiveFailures++
	m.penalty += penaltyPerProbeFailure * 0.25
	return m.consecutivePassiveFailures >= uint32(threshold)
}

func (m *LinkMetrics) ConfirmPassiveCircuitBreaker(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	m.access.Lock()
	defer m.access.Unlock()

	m.decayPenaltyLocked(now)
	if !m.circuitOpen {
		m.circuitOpen = true
		m.available = false
		m.penalty += penaltyPerCircuitTrip
	}
}

func (m *LinkMetrics) CancelPassiveCircuitBreaker(probeDelay uint16, now time.Time) uint16 {
	if now.IsZero() {
		now = time.Now()
	}
	m.access.Lock()
	refund := float64(m.consecutivePassiveFailures) * penaltyPerProbeFailure * 0.25
	if m.penalty > refund {
		m.penalty -= refund
	} else {
		m.penalty = 0
	}
	m.consecutivePassiveFailures = 0
	m.circuitOpen = false
	m.access.Unlock()
	return m.RecordProbeSuccess(probeDelay, now)
}

func (m *LinkMetrics) RecordTrafficBytes(n int64) {
	if n <= 0 {
		return
	}
	m.totalBytes.Add(n)
	nowSec := time.Now().Unix()
	slot := &m.bwSlots[nowSec&bwSlotMask]
	slotSec := slot.sec.Load()
	var currentSecBytes int64
	if slotSec != nowSec {
		if slot.sec.CompareAndSwap(slotSec, nowSec) {
			slot.bytes.Store(n)
			currentSecBytes = n
		} else {
			currentSecBytes = slot.bytes.Add(n)
		}
	} else {
		currentSecBytes = slot.bytes.Add(n)
	}
	bps := uint64(currentSecBytes) * 8
	for {
		peak := m.peakThroughputBps.Load()
		if bps <= peak || m.peakThroughputBps.CompareAndSwap(peak, bps) {
			break
		}
	}
}

func (m *LinkMetrics) CurrentThroughputBps(now time.Time) uint64 {
	if now.IsZero() {
		now = time.Now()
	}
	nowSec := now.Unix()
	w := m.bwWindowSec.Load()
	if w <= 1 {
		var maxBytes int64
		for i := range m.bwSlots {
			sec := m.bwSlots[i].sec.Load()
			if sec == nowSec || sec == nowSec-1 {
				b := m.bwSlots[i].bytes.Load()
				if b > maxBytes {
					maxBytes = b
				}
			}
		}
		if maxBytes <= 0 {
			return 0
		}
		return uint64(maxBytes) * 8
	}
	if w > maxBandwidthWindowSec {
		w = maxBandwidthWindowSec
	}
	var sumCurr, sumPrev int64
	for i := range m.bwSlots {
		sec := m.bwSlots[i].sec.Load()
		if sec <= 0 {
			continue
		}
		b := m.bwSlots[i].bytes.Load()
		if b <= 0 {
			continue
		}
		if sec >= nowSec-w+1 && sec <= nowSec {
			sumCurr += b
		}
		if sec >= nowSec-w && sec <= nowSec-1 {
			sumPrev += b
		}
	}
	bestSum := sumCurr
	if sumPrev > bestSum {
		bestSum = sumPrev
	}
	if bestSum <= 0 {
		return 0
	}
	return (uint64(bestSum) * 8) / uint64(w)
}

func (m *LinkMetrics) Snapshot(now time.Time) LinkMetricsSnapshot {
	if now.IsZero() {
		now = time.Now()
	}
	m.access.Lock()
	m.decayPenaltyLocked(now)
	snap := LinkMetricsSnapshot{
		Available:                  m.available && !m.circuitOpen,
		CircuitOpen:                m.circuitOpen,
		SampleCount:                m.sampleCount,
		LastDelay:                  m.lastDelay,
		SmoothedRTT:                m.smoothedRTT,
		Jitter:                     m.jitter,
		LossRate:                   m.lossRate,
		Penalty:                    m.penalty,
		ConsecutiveProbeFailures:   m.consecutiveProbeFailures,
		ConsecutivePassiveFailures: m.consecutivePassiveFailures,
		ThroughputBps:              m.CurrentThroughputBps(now),
		PeakThroughputBps:          m.peakThroughputBps.Load(),
		TotalBytes:                 uint64(m.totalBytes.Load()),
		LastUpdated:                m.lastUpdated,
		LastPassiveSuccess:         m.lastPassiveSuccess,
	}
	m.access.Unlock()
	return snap
}

func (m *LinkMetrics) ResetPenaltyAndCircuit(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	m.access.Lock()
	defer m.access.Unlock()
	m.penalty = 0
	m.penaltyUpdatedAt = now
	m.lossRate = 0
	m.circuitOpen = false
	m.consecutivePassiveFailures = 0
	m.consecutiveProbeFailures = 0
	if m.sampleCount > 0 {
		m.available = true
	}
}

func (m *LinkMetrics) WrapConn(conn net.Conn) net.Conn {
	if conn == nil {
		return nil
	}
	countFunc := []N.CountFunc{m.RecordTrafficBytes}
	return bufio.NewCounterConn(conn, countFunc, countFunc)
}

func (m *LinkMetrics) WrapPacketConn(conn net.PacketConn) net.PacketConn {
	if conn == nil {
		return nil
	}
	countFunc := []N.CountFunc{m.RecordTrafficBytes}
	return bufio.NewNetPacketConn(bufio.NewCounterPacketConn(bufio.NewPacketConn(conn), countFunc, countFunc))
}

type MetricsStorage struct {
	access       sync.RWMutex
	upstream     *urltest.HistoryStorage
	linkMetrics  map[string]*LinkMetrics
	windowConfig MetricsWindowConfig
}

func NewMetricsStorage(upstream *urltest.HistoryStorage) *MetricsStorage {
	return &MetricsStorage{
		upstream:    upstream,
		linkMetrics: make(map[string]*LinkMetrics),
		windowConfig: MetricsWindowConfig{
			StatWindow:      DefaultStatWindow,
			PenaltyHalfLife: DefaultPenaltyHalfLife,
			BandwidthWindow: DefaultBandwidthWindow,
		},
	}
}

func MetricsFromContext(ctx context.Context) *MetricsStorage {
	if m := service.PtrFromContext[MetricsStorage](ctx); m != nil {
		if m.upstream == nil {
			m.upstream = service.PtrFromContext[urltest.HistoryStorage](ctx)
		}
		return m
	}
	upstream := service.PtrFromContext[urltest.HistoryStorage](ctx)
	m := NewMetricsStorage(upstream)
	if service.FromContext[service.Registry](ctx) != nil {
		service.MustRegisterPtr(ctx, m)
	}
	return m
}

func (s *MetricsStorage) SetWindowConfig(cfg MetricsWindowConfig) {
	if s == nil {
		return
	}
	if cfg.StatWindow == 0 {
		cfg.StatWindow = DefaultStatWindow
	}
	if cfg.PenaltyHalfLife <= 0 {
		cfg.PenaltyHalfLife = DefaultPenaltyHalfLife
	}
	if cfg.BandwidthWindow <= 0 {
		cfg.BandwidthWindow = DefaultBandwidthWindow
	}
	s.access.Lock()
	s.windowConfig = cfg
	for _, m := range s.linkMetrics {
		if m != nil {
			m.SetWindowConfig(cfg)
		}
	}
	s.access.Unlock()
}

func (s *MetricsStorage) getOrCreateMetricsLocked(tag string) *LinkMetrics {
	if s.linkMetrics == nil {
		s.linkMetrics = make(map[string]*LinkMetrics)
	}
	m, ok := s.linkMetrics[tag]
	if !ok {
		m = NewLinkMetrics()
		m.SetWindowConfig(s.windowConfig)
		s.linkMetrics[tag] = m
	}
	return m
}

func (s *MetricsStorage) getOrCreateMetrics(tag string) *LinkMetrics {
	if s == nil || tag == "" {
		return nil
	}
	s.access.RLock()
	m, ok := s.linkMetrics[tag]
	s.access.RUnlock()
	if ok {
		return m
	}
	s.access.Lock()
	defer s.access.Unlock()
	return s.getOrCreateMetricsLocked(tag)
}

func (s *MetricsStorage) LoadURLTestHistory(tag string) *adapter.URLTestHistory {
	if s == nil {
		return nil
	}
	if s.upstream != nil {
		if h := s.upstream.LoadURLTestHistory(tag); h != nil {
			return h
		}
	}
	snap, ok := s.LoadLinkMetrics(tag)
	if !ok || !snap.Available {
		return nil
	}
	return &adapter.URLTestHistory{
		Time:  snap.LastUpdated,
		Delay: uint16(math.Round(snap.SmoothedRTT)),
	}
}

func (s *MetricsStorage) LoadLinkMetrics(tag string) (LinkMetricsSnapshot, bool) {
	if s == nil || tag == "" {
		return LinkMetricsSnapshot{}, false
	}
	s.access.RLock()
	metrics, loaded := s.linkMetrics[tag]
	s.access.RUnlock()

	var history *adapter.URLTestHistory
	if s.upstream != nil {
		history = s.upstream.LoadURLTestHistory(tag)
	}
	if !loaded || metrics == nil {
		if history != nil && history.Delay > 0 {
			s.access.Lock()
			metrics = s.getOrCreateMetricsLocked(tag)
			metrics.RecordProbeSuccess(history.Delay, history.Time)
			s.access.Unlock()
			return metrics.Snapshot(time.Now()), true
		}
		return LinkMetricsSnapshot{}, false
	}
	snap := metrics.Snapshot(time.Now())
	if history != nil && history.Delay > 0 && history.Time.After(snap.LastUpdated.Add(100*time.Millisecond)) {
		smoothed := metrics.RecordProbeSuccess(history.Delay, history.Time)
		s.upstream.StoreURLTestHistory(tag, &adapter.URLTestHistory{
			Time:  history.Time,
			Delay: smoothed,
		})
		snap = metrics.Snapshot(time.Now())
	} else if history == nil && snap.Available && s.upstream != nil && snap.SmoothedRTT > 0 {
		// Restore upstream history if route/conn.go deleted it on a single passive dial error below FailureThreshold
		s.upstream.StoreURLTestHistory(tag, &adapter.URLTestHistory{
			Time:  snap.LastUpdated,
			Delay: uint16(math.Round(snap.SmoothedRTT)),
		})
	}
	return snap, true
}

func (s *MetricsStorage) RecordProbeSuccess(tag string, delay uint16) uint16 {
	if s == nil {
		return delay
	}
	now := time.Now()
	s.access.Lock()
	metrics := s.getOrCreateMetricsLocked(tag)
	smoothed := metrics.RecordProbeSuccess(delay, now)
	s.access.Unlock()
	if s.upstream != nil {
		s.upstream.StoreURLTestHistory(tag, &adapter.URLTestHistory{
			Time:  now,
			Delay: smoothed,
		})
	}
	return smoothed
}

func (s *MetricsStorage) RecordProbeFailure(tag string) {
	if s == nil {
		return
	}
	now := time.Now()
	s.access.Lock()
	metrics := s.getOrCreateMetricsLocked(tag)
	metrics.RecordProbeFailure(now)
	s.access.Unlock()
	if s.upstream != nil {
		s.upstream.DeleteURLTestHistory(tag)
	}
}

func (s *MetricsStorage) DeleteURLTestHistory(tag string) {
	s.RecordProbeFailure(tag)
}

func (s *MetricsStorage) StoreURLTestHistory(tag string, history *adapter.URLTestHistory) {
	if s == nil {
		return
	}
	if history == nil {
		s.RecordProbeFailure(tag)
		return
	}
	s.access.Lock()
	metrics := s.getOrCreateMetricsLocked(tag)
	smoothed := metrics.RecordProbeSuccess(history.Delay, history.Time)
	s.access.Unlock()
	if s.upstream != nil {
		s.upstream.StoreURLTestHistory(tag, &adapter.URLTestHistory{
			Time:  history.Time,
			Delay: smoothed,
		})
	}
}

func (s *MetricsStorage) RecordPassiveSuccess(tag string, dialRTT time.Duration) {
	metrics := s.getOrCreateMetrics(tag)
	if metrics == nil {
		return
	}
	metrics.RecordPassiveSuccess(dialRTT, time.Now())
}

func (s *MetricsStorage) RecordPassiveFailure(tag string, threshold uint8) bool {
	metrics := s.getOrCreateMetrics(tag)
	if metrics == nil {
		return false
	}
	tripped := metrics.RecordPassiveFailure(threshold, time.Now())
	if tripped && s.upstream != nil {
		s.upstream.DeleteURLTestHistory(tag)
	}
	return tripped
}

func (s *MetricsStorage) RecordPassiveFailureReachedThreshold(tag string, threshold uint8) bool {
	metrics := s.getOrCreateMetrics(tag)
	if metrics == nil {
		return false
	}
	return metrics.RecordPassiveFailureReachedThreshold(threshold, time.Now())
}

func (s *MetricsStorage) ConfirmPassiveCircuitBreaker(tag string) {
	metrics := s.getOrCreateMetrics(tag)
	if metrics == nil {
		return
	}
	metrics.ConfirmPassiveCircuitBreaker(time.Now())
	if s.upstream != nil {
		s.upstream.DeleteURLTestHistory(tag)
	}
}

func (s *MetricsStorage) CancelPassiveCircuitBreaker(tag string, probeDelay uint16) uint16 {
	if s == nil {
		return probeDelay
	}
	now := time.Now()
	s.access.Lock()
	metrics := s.getOrCreateMetricsLocked(tag)
	smoothed := metrics.CancelPassiveCircuitBreaker(probeDelay, now)
	s.access.Unlock()
	if s.upstream != nil {
		s.upstream.StoreURLTestHistory(tag, &adapter.URLTestHistory{
			Time:  now,
			Delay: smoothed,
		})
	}
	return smoothed
}

func (s *MetricsStorage) ResetLinkMetrics(tag string) {
	if s == nil || tag == "" {
		return
	}
	now := time.Now()
	s.access.Lock()
	metrics := s.getOrCreateMetricsLocked(tag)
	metrics.ResetPenaltyAndCircuit(now)
	snap := metrics.Snapshot(now)
	s.access.Unlock()
	if snap.Available && snap.SmoothedRTT > 0 && s.upstream != nil {
		s.upstream.StoreURLTestHistory(tag, &adapter.URLTestHistory{
			Time:  now,
			Delay: uint16(snap.SmoothedRTT + 0.5),
		})
	} else if s.upstream != nil {
		s.upstream.NotifyUpdated()
	}
}

func (s *MetricsStorage) RecordTrafficBytes(tag string, n int64) {
	metrics := s.getOrCreateMetrics(tag)
	if metrics == nil {
		return
	}
	metrics.RecordTrafficBytes(n)
}

func (s *MetricsStorage) WrapConn(tag string, conn net.Conn) net.Conn {
	metrics := s.getOrCreateMetrics(tag)
	if metrics == nil {
		return conn
	}
	return metrics.WrapConn(conn)
}

func (s *MetricsStorage) WrapPacketConn(tag string, conn net.PacketConn) net.PacketConn {
	metrics := s.getOrCreateMetrics(tag)
	if metrics == nil {
		return conn
	}
	return metrics.WrapPacketConn(conn)
}

func (s *MetricsStorage) NotifyUpdated() {
	if s != nil && s.upstream != nil {
		s.upstream.NotifyUpdated()
	}
}
