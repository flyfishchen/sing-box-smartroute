package smartroute

import (
	"context"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/interrupt"
	"github.com/sagernet/sing-box/common/urltest"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/protocol/group"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/batch"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/x/list"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"
)

func RegisterURLTest(registry *outbound.Registry) {
	outbound.Register[URLTestOptions](registry, C.TypeURLTest, NewURLTest)
	outbound.Register[URLTestOptions](registry, TypeSmartRoute, NewURLTest)
}

var (
	_ adapter.OutboundGroup           = (*URLTest)(nil)
	_ adapter.URLTestGroup            = (*URLTest)(nil)
	_ adapter.InterfaceUpdateListener = (*URLTest)(nil)
	_ adapter.Referrer                = (*URLTest)(nil)
)

type URLTest struct {
	outbound.Adapter
	ctx                          context.Context
	outbound                     adapter.OutboundManager
	logger                       log.ContextLogger
	tags                         []string
	link                         string
	interval                     time.Duration
	tolerance                    uint16
	idleTimeout                  time.Duration
	smartRoute                   SmartRouteConfig
	group                        *URLTestGroup
	checkAccess                  sync.Mutex
	interruptExternalConnections bool
}

func NewURLTest(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options URLTestOptions) (adapter.Outbound, error) {
	smartRouteConfig, err := NewSmartRouteConfig(options)
	if err != nil {
		return nil, err
	}
	outbound := &URLTest{
		Adapter:                      outbound.NewAdapter(C.TypeURLTest, tag, []string{N.NetworkTCP, N.NetworkUDP}, options.Outbounds),
		ctx:                          ctx,
		outbound:                     service.FromContext[adapter.OutboundManager](ctx),
		logger:                       logger,
		tags:                         options.Outbounds,
		link:                         options.URL,
		interval:                     time.Duration(options.Interval),
		tolerance:                    options.Tolerance,
		idleTimeout:                  time.Duration(options.IdleTimeout),
		smartRoute:                   smartRouteConfig,
		interruptExternalConnections: options.InterruptExistConnections,
	}
	if len(outbound.tags) == 0 {
		return nil, E.New("missing tags")
	}
	for i, tag := range outbound.tags {
		if slices.Contains(outbound.tags[:i], tag) {
			return nil, E.New("duplicate tag: ", tag)
		}
	}
	return outbound, nil
}

func (s *URLTest) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	switch stage {
	case adapter.StartStateStart:
		outbounds := make([]adapter.Outbound, 0, len(s.tags))
		for i, tag := range s.tags {
			detour, loaded := s.outbound.Outbound(tag)
			if !loaded {
				return E.New("outbound ", i, " not found: ", tag)
			}
			outbounds = append(outbounds, detour)
		}
		group, err := NewURLTestGroupWithSmartRoute(s.ctx, s.outbound, s.logger, outbounds, s.link, s.interval, s.tolerance, s.idleTimeout, s.interruptExternalConnections, s.smartRoute)
		if err != nil {
			return err
		}
		s.group = group
	case adapter.StartStateStarted:
		s.group.PostStart()
		scope.Add(s.group.Close)
	}
	return nil
}

func (s *URLTest) All() []string {
	return s.tags
}

func (s *URLTest) selectRaw(networkName string) adapter.Outbound {
	var detour adapter.Outbound
	if networkName == N.NetworkUDP {
		detour = s.group.selectedOutboundUDP
	} else {
		detour = s.group.selectedOutboundTCP
	}
	if detour == nil || s.group.isCurrentOutboundCongested(detour, networkName) {
		detour, _ = s.group.Select(networkName)
	}
	return detour
}

func unwrapLeafOutbound(detour adapter.Outbound, network string) adapter.Outbound {
	for detour != nil {
		if ro, ok := detour.(*routedOutbound); ok {
			return ro.Outbound
		}
		grp, isGroup := detour.(adapter.OutboundGroup)
		if !isGroup {
			return detour
		}
		detour = grp.Selected(network)
	}
	return nil
}

func (s *URLTest) Selected(network string) adapter.Outbound {
	if s.group == nil {
		return nil
	}
	s.group.Touch()
	networkName := N.NetworkName(network)
	detour := s.selectRaw(networkName)
	leaf := unwrapLeafOutbound(detour, networkName)
	if leaf == nil {
		return nil
	}
	return &routedOutbound{
		Outbound: leaf,
		parent:   s,
		network:  networkName,
	}
}

type routedOutbound struct {
	adapter.Outbound
	parent  *URLTest
	network string
}

func (r *routedOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	start := time.Now()
	conn, err := r.Outbound.DialContext(ctx, network, destination)
	realTag := r.Outbound.Tag()
	if err == nil {
		r.parent.group.metrics.RecordPassiveSuccess(realTag, time.Since(start))
		return r.parent.group.metrics.WrapConn(realTag, conn), nil
	}
	if r.parent.logger != nil {
		r.parent.logger.ErrorContext(ctx, err)
	}
	if ctx.Err() != nil {
		return nil, err
	}
	if r.parent.group.metrics.RecordPassiveFailureReachedThreshold(realTag, r.parent.group.smartRoute.FailureThreshold) {
		alt := unwrapLeafOutbound(r.parent.group.selectAlternateOutbound(r.network, realTag), r.network)
		altTag := ""
		if alt != nil && alt.Tag() != realTag {
			altTag = alt.Tag()
		}
		probeDone := r.parent.group.verifyOutboundBeforeCircuitBreak(r.Outbound, r.network, altTag, err)
		if alt != nil && altTag != "" {
			retryStart := time.Now()
			retryConn, retryErr := alt.DialContext(ctx, network, destination)
			select {
			case <-probeDone:
			case <-time.After(50 * time.Millisecond):
			}
			if retryErr == nil {
				r.parent.group.metrics.RecordPassiveSuccess(altTag, time.Since(retryStart))
				return r.parent.group.metrics.WrapConn(altTag, retryConn), nil
			}
		} else {
			select {
			case <-probeDone:
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return nil, err
}

func (r *routedOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	start := time.Now()
	conn, err := r.Outbound.ListenPacket(ctx, destination)
	realTag := r.Outbound.Tag()
	if err == nil {
		r.parent.group.metrics.RecordPassiveSuccess(realTag, time.Since(start))
		return r.parent.group.metrics.WrapPacketConn(realTag, conn), nil
	}
	if r.parent.logger != nil {
		r.parent.logger.ErrorContext(ctx, err)
	}
	if ctx.Err() != nil {
		return nil, err
	}
	if r.parent.group.metrics.RecordPassiveFailureReachedThreshold(realTag, r.parent.group.smartRoute.FailureThreshold) {
		alt := unwrapLeafOutbound(r.parent.group.selectAlternateOutbound(N.NetworkUDP, realTag), N.NetworkUDP)
		altTag := ""
		if alt != nil && alt.Tag() != realTag {
			altTag = alt.Tag()
		}
		probeDone := r.parent.group.verifyOutboundBeforeCircuitBreak(r.Outbound, N.NetworkUDP, altTag, err)
		if alt != nil && altTag != "" {
			retryStart := time.Now()
			retryConn, retryErr := alt.ListenPacket(ctx, destination)
			select {
			case <-probeDone:
			case <-time.After(50 * time.Millisecond):
			}
			if retryErr == nil {
				r.parent.group.metrics.RecordPassiveSuccess(altTag, time.Since(retryStart))
				return r.parent.group.metrics.WrapPacketConn(altTag, retryConn), nil
			}
		} else {
			select {
			case <-probeDone:
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return nil, err
}

func (s *URLTest) AttachConnection(closer io.Closer) func() {
	s.group.Touch()
	return s.group.interruptGroup.Add(closer, true)
}

func (s *URLTest) References() []string {
	group := s.group
	if group == nil {
		return nil
	}
	var references []string
	if group.selectedOutboundTCP != nil {
		references = append(references, group.selectedOutboundTCP.Tag())
	}
	if group.selectedOutboundUDP != nil && group.selectedOutboundUDP != group.selectedOutboundTCP {
		references = append(references, group.selectedOutboundUDP.Tag())
	}
	return references
}

func (s *URLTest) URLTest(ctx context.Context) (map[string]uint16, error) {
	return s.group.URLTest(ctx)
}

func (s *URLTest) CheckOutbounds() {
	s.group.CheckOutbounds(s.ctx, true)
}

func (s *URLTest) PerformUpdateCheck() {
	s.group.performUpdateCheck()
}

func (s *URLTest) InterfaceUpdated(ctx context.Context) {
	group := s.group
	if group == nil {
		return
	}
	if group.pause != nil && (group.pause.IsDevicePaused() || group.pause.IsNetworkPaused()) {
		return
	}
	go func() {
		s.checkAccess.Lock()
		defer s.checkAccess.Unlock()
		if ctx.Err() != nil {
			return
		}
		group.CheckOutbounds(ctx, true)
	}()
}

func (s *URLTest) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	s.group.Touch()
	networkName := N.NetworkName(network)
	switch networkName {
	case N.NetworkTCP, N.NetworkUDP:
	default:
		return nil, E.Extend(N.ErrUnknownNetwork, network)
	}
	selected := s.Selected(networkName)
	if selected == nil {
		return nil, E.New("missing supported outbound")
	}
	conn, err := selected.DialContext(ctx, network, destination)
	if err != nil {
		return nil, err
	}
	return s.group.interruptGroup.NewConn(conn, interrupt.IsExternalConnectionFromContext(ctx)), nil
}

func (s *URLTest) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	s.group.Touch()
	selected := s.Selected(N.NetworkUDP)
	if selected == nil {
		return nil, E.New("missing supported outbound")
	}
	conn, err := selected.ListenPacket(ctx, destination)
	if err != nil {
		return nil, err
	}
	return s.group.interruptGroup.NewPacketConn(conn, interrupt.IsExternalConnectionFromContext(ctx)), nil
}

type URLTestGroup struct {
	ctx                          context.Context
	outbound                     adapter.OutboundManager
	pause                        pause.Manager
	pauseCallback                *list.Element[pause.Callback]
	logger                       log.Logger
	outbounds                    []adapter.Outbound
	link                         string
	interval                     time.Duration
	tolerance                    uint16
	idleTimeout                  time.Duration
	smartRoute                   SmartRouteConfig
	metrics                      *MetricsStorage
	checking                     atomic.Bool
	probingNodes                 sync.Map
	selectedOutboundTCP          adapter.Outbound
	selectedOutboundUDP          adapter.Outbound
	lastSwitchTCP                time.Time
	lastSwitchUDP                time.Time
	interruptGroup               *interrupt.Group
	interruptExternalConnections bool
	access                       sync.Mutex
	updateAccess                 sync.Mutex
	eventsMu                     sync.Mutex
	events                       []SmartRouteEvent
	stats                        smartRouteDecisionStats
	lastSuppressedKey            string
	lastSuppressedAt             time.Time
	lastOverflowTag              string
	ticker                       *time.Ticker
	close                        chan struct{}
	started                      bool
	lastActive                   common.TypedValue[time.Time]
}

func NewURLTestGroup(ctx context.Context, outboundManager adapter.OutboundManager, logger log.Logger, outbounds []adapter.Outbound, link string, interval time.Duration, tolerance uint16, idleTimeout time.Duration, interruptExternalConnections bool) (*URLTestGroup, error) {
	return NewURLTestGroupWithSmartRoute(ctx, outboundManager, logger, outbounds, link, interval, tolerance, idleTimeout, interruptExternalConnections, SmartRouteConfig{})
}

func NewURLTestGroupWithSmartRoute(ctx context.Context, outboundManager adapter.OutboundManager, logger log.Logger, outbounds []adapter.Outbound, link string, interval time.Duration, tolerance uint16, idleTimeout time.Duration, interruptExternalConnections bool, smartRoute SmartRouteConfig) (*URLTestGroup, error) {
	if link == "" {
		link = DefaultURLTestURL
	}
	if interval == 0 {
		interval = DefaultURLTestInterval
	}
	if tolerance == 0 {
		tolerance = 50
	}
	if idleTimeout == 0 {
		idleTimeout = DefaultURLTestIdleTimeout
	}
	if interval > idleTimeout {
		return nil, E.New("interval must be less or equal than idle_timeout")
	}
	metrics := MetricsFromContext(ctx)
	smartRoute.ensureDefaults()
	metrics.SetWindowConfig(MetricsWindowConfig{
		StatWindow:      smartRoute.StatWindow,
		PenaltyHalfLife: smartRoute.PenaltyHalfLife,
		BandwidthWindow: smartRoute.BandwidthWindow,
	})
	return &URLTestGroup{
		ctx:                          ctx,
		outbound:                     outboundManager,
		logger:                       logger,
		outbounds:                    outbounds,
		link:                         link,
		interval:                     interval,
		tolerance:                    tolerance,
		idleTimeout:                  idleTimeout,
		smartRoute:                   smartRoute,
		metrics:                      metrics,
		close:                        make(chan struct{}),
		pause:                        service.FromContext[pause.Manager](ctx),
		interruptGroup:               interrupt.NewGroup(),
		interruptExternalConnections: interruptExternalConnections,
	}, nil
}

func (g *URLTestGroup) PostStart() {
	g.access.Lock()
	defer g.access.Unlock()
	g.started = true
	g.lastActive.Store(time.Now())
	go g.CheckOutbounds(g.ctx, false)
}

func (g *URLTestGroup) Touch() {
	if !g.started {
		return
	}
	g.access.Lock()
	defer g.access.Unlock()
	if g.ticker != nil {
		g.lastActive.Store(time.Now())
		return
	}
	g.ticker = time.NewTicker(g.interval)
	go g.loopCheck(g.ticker, g.close)
	if g.pause != nil {
		g.pauseCallback = pause.RegisterTicker(g.pause, g.ticker, g.interval, nil)
	}
}

func (g *URLTestGroup) SetInterval(interval time.Duration) {
	if interval <= 0 {
		return
	}
	g.access.Lock()
	defer g.access.Unlock()
	if g.interval == interval {
		return
	}
	g.interval = interval
	if g.idleTimeout < g.interval {
		g.idleTimeout = g.interval
	}
	if g.ticker != nil {
		g.ticker.Reset(g.interval)
		if g.pause != nil && g.pauseCallback != nil {
			g.pause.UnregisterCallback(g.pauseCallback)
			g.pauseCallback = pause.RegisterTicker(g.pause, g.ticker, g.interval, nil)
		}
	}
}

func (g *URLTestGroup) Close() error {
	g.access.Lock()
	defer g.access.Unlock()
	if g.ticker == nil {
		return nil
	}
	g.ticker.Stop()
	if g.pause != nil && g.pauseCallback != nil {
		g.pause.UnregisterCallback(g.pauseCallback)
		g.pauseCallback = nil
	}
	close(g.close)
	return nil
}

func (g *URLTestGroup) Select(network string) (adapter.Outbound, bool) {
	networkName := N.NetworkName(network)
	var current adapter.Outbound
	var lastSwitch time.Time
	switch networkName {
	case N.NetworkTCP:
		current = g.selectedOutboundTCP
		lastSwitch = g.lastSwitchTCP
	case N.NetworkUDP:
		current = g.selectedOutboundUDP
		lastSwitch = g.lastSwitchUDP
	}
	outbound, exists, _ := g.selectSmartRoute(networkName, current, lastSwitch, false)
	return outbound, exists
}

func (g *URLTestGroup) loopCheck(ticker *time.Ticker, closeChan <-chan struct{}) {
	if time.Since(g.lastActive.Load()) > g.interval {
		g.lastActive.Store(time.Now())
		g.CheckOutbounds(g.ctx, false)
	}
	for {
		select {
		case <-closeChan:
			return
		case <-ticker.C:
		}
		if time.Since(g.lastActive.Load()) > g.idleTimeout {
			g.access.Lock()
			if g.ticker == ticker {
				g.ticker.Stop()
				g.ticker = nil
				if g.pause != nil && g.pauseCallback != nil {
					g.pause.UnregisterCallback(g.pauseCallback)
					g.pauseCallback = nil
				}
			}
			g.access.Unlock()
			return
		}
		g.CheckOutbounds(g.ctx, false)
	}
}

func (g *URLTestGroup) CheckOutbounds(ctx context.Context, force bool) {
	_, _ = g.urlTest(ctx, force)
}

func (g *URLTestGroup) URLTest(ctx context.Context) (map[string]uint16, error) {
	return g.urlTest(ctx, true)
}

func (g *URLTestGroup) urlTest(ctx context.Context, force bool) (map[string]uint16, error) {
	if g.checking.Swap(true) {
		return make(map[string]uint16), nil
	}
	defer g.checking.Store(false)
	result := URLTestOutbounds(ctx, g.outbound, g.metrics, g.logger, g.outbounds, g.link, g.interval, force)
	g.performUpdateCheck()
	return result, nil
}

type urlTestResult struct {
	delay uint16
	err   error
}

type urlTestBatch struct {
	ctx      context.Context
	outbound adapter.OutboundManager
	metrics  *MetricsStorage
	logger   log.Logger
	batch    *batch.Batch[any]
	checked  map[string]bool
	groups   []adapter.OutboundGroup
	access   sync.Mutex
	result   map[string]uint16
}

func URLTestOutbounds(ctx context.Context, outboundManager adapter.OutboundManager, metrics *MetricsStorage, logger log.Logger, outbounds []adapter.Outbound, link string, interval time.Duration, force bool) map[string]uint16 {
	b, _ := batch.New(ctx, batch.WithConcurrencyNum[any](10))
	testBatch := &urlTestBatch{
		ctx:      ctx,
		outbound: outboundManager,
		metrics:  metrics,
		logger:   logger,
		batch:    b,
		checked:  make(map[string]bool),
		result:   make(map[string]uint16),
	}
	testBatch.test(outbounds, link, interval, force)
	b.Wait()
	for _, outboundGroup := range testBatch.groups {
		groupHistory := metrics.LoadURLTestHistory(group.RealTag(outboundGroup, N.NetworkTCP))
		if groupHistory != nil {
			testBatch.result[outboundGroup.Tag()] = groupHistory.Delay
		}
	}
	return testBatch.result
}

func (b *urlTestBatch) test(outbounds []adapter.Outbound, link string, interval time.Duration, force bool) {
	for _, detour := range outbounds {
		tag := detour.Tag()
		if b.checked[tag] {
			continue
		}
		switch nested := detour.(type) {
		case adapter.OutboundGroup:
			b.checked[tag] = true
			b.groups = append(b.groups, nested)
			if selected := unwrapLeafOutbound(nested, N.NetworkTCP); selected != nil {
				b.test([]adapter.Outbound{selected}, link, interval, force)
			}
		default:
			history := b.metrics.LoadURLTestHistory(tag)
			if !force && history != nil && time.Since(history.Time) < interval*4/5 {
				continue
			}
			b.checked[tag] = true
			b.batch.Go(tag, func() (any, error) {
				testCtx, cancel := context.WithTimeout(b.ctx, C.TCPTimeout)
				defer cancel()
				testChan := make(chan urlTestResult, 1)
				go func() {
					delay, testErr := urltest.URLTest(testCtx, link, detour)
					testChan <- urlTestResult{delay, testErr}
				}()
				var testResult urlTestResult
				select {
				case testResult = <-testChan:
				case <-testCtx.Done():
					testResult.err = testCtx.Err()
				}
				if testResult.err != nil {
					if b.ctx.Err() != nil {
						return nil, nil
					}
					b.logger.Debug("outbound ", tag, " unavailable: ", testResult.err)
					b.metrics.RecordProbeFailure(tag)
				} else {
					smoothed := b.metrics.RecordProbeSuccess(tag, testResult.delay)
					b.logger.Debug("outbound ", tag, " available: ", testResult.delay, "ms (smoothed: ", smoothed, "ms)")
					b.access.Lock()
					b.result[tag] = smoothed
					b.access.Unlock()
				}
				return nil, nil
			})
		}
	}
}

func (g *URLTestGroup) performUpdateCheck() {
	g.updateAccess.Lock()
	defer g.updateAccess.Unlock()
	var (
		updated  bool
		selected bool
	)
	now := time.Now()
	tcpDecision := g.selectSmartRouteWithDecision(N.NetworkTCP, g.selectedOutboundTCP, g.lastSwitchTCP, false)
	g.observeDecision(N.NetworkTCP, tcpDecision, g.selectedOutboundTCP, g.lastSwitchTCP, now)
	if outbound := tcpDecision.selected; outbound != nil && !tcpDecision.overflow && (g.selectedOutboundTCP == nil || (tcpDecision.exists && outbound != g.selectedOutboundTCP)) {
		if g.selectedOutboundTCP != nil {
			updated = true
		}
		if tcpDecision.exists {
			g.lastSwitchTCP = now
		}
		g.selectedOutboundTCP = outbound
		selected = true
	}
	udpDecision := g.selectSmartRouteWithDecision(N.NetworkUDP, g.selectedOutboundUDP, g.lastSwitchUDP, false)
	if outbound := udpDecision.selected; outbound != nil && !udpDecision.overflow && (g.selectedOutboundUDP == nil || (udpDecision.exists && outbound != g.selectedOutboundUDP)) {
		if g.selectedOutboundUDP != nil {
			updated = true
		}
		if udpDecision.exists {
			g.lastSwitchUDP = now
		}
		g.selectedOutboundUDP = outbound
		selected = true
	}
	if updated {
		g.interruptGroup.Interrupt(g.interruptExternalConnections)
	}
	if selected {
		g.metrics.NotifyUpdated()
	}
}

func (g *URLTestGroup) verifyOutboundBeforeCircuitBreak(detour adapter.Outbound, network string, failoverTag string, triggerErr error) <-chan struct{} {
	realTag := detour.Tag()
	done := make(chan struct{})
	if actual, loaded := g.probingNodes.LoadOrStore(realTag, done); loaded {
		return actual.(chan struct{})
	}
	go func() {
		defer func() {
			g.probingNodes.Delete(realTag)
			close(done)
		}()
		baseCtx := g.ctx
		if baseCtx == nil {
			baseCtx = context.Background()
		}
		probeCtx, cancel := context.WithTimeout(baseCtx, 3*time.Second)
		defer cancel()
		delay, probeErr := urltest.URLTest(probeCtx, g.link, detour)
		if probeErr == nil {
			smoothed := g.metrics.CancelPassiveCircuitBreaker(realTag, delay)
			g.recordProbeVerifiedEvent(network, realTag, delay, smoothed, triggerErr)
			return
		}
		g.metrics.ConfirmPassiveCircuitBreaker(realTag)
		g.performUpdateCheck()
		if failoverTag == "" {
			if alt := unwrapLeafOutbound(g.selectAlternateOutbound(network, realTag), network); alt != nil {
				failoverTag = alt.Tag()
			}
		}
		g.recordCircuitBreakerEvent(network, realTag, failoverTag, triggerErr, probeErr)
	}()
	return done
}
