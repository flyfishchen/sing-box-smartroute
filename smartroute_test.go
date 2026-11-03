package smartroute

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/urltest"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"
)

type mockOutbound struct {
	outbound.Adapter
	dialFunc func(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error)
}

func newMockOutbound(tag string, dialFunc func(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error)) *mockOutbound {
	return &mockOutbound{
		Adapter:  outbound.NewAdapter(C.TypeDirect, tag, []string{N.NetworkTCP, N.NetworkUDP}, nil),
		dialFunc: dialFunc,
	}
}

func (m *mockOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if m.dialFunc != nil {
		return m.dialFunc(ctx, network, destination)
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func (m *mockOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("not implemented")
}

func newTestContext() (context.Context, *MetricsStorage) {
	ctx := context.Background()
	history := urltest.NewHistoryStorage()
	metrics := NewMetricsStorage(history)
	ctx = service.ContextWithPtr(ctx, history)
	ctx = service.ContextWithPtr(ctx, metrics)
	return ctx, metrics
}

func TestSmartRouteTierAndCablePreference(t *testing.T) {
	t.Parallel()
	ctx, history := newTestContext()

	iplc := newMockOutbound("hk-iplc-01", nil)
	cn2 := newMockOutbound("hk-cn2-01", nil)
	cmi := newMockOutbound("hk-cmi-01", nil)

	cfg, err := NewSmartRouteConfig(URLTestOptions{
		Tolerance:  15,
		MaxLatency: 150,
		HoldTime:   badoption.Duration(10 * time.Millisecond),
		Policies: []URLTestPolicy{
			{
				Match:      "(?i)iplc",
				Tier:       1,
				Preference: -25,
				Cost:       1.0,
			},
			{
				Match:      "(?i)cn2",
				Tier:       1,
				Preference: 0,
				Cost:       1.0,
			},
			{
				Match:      "(?i)cmi",
				Tier:       2,
				Preference: 0,
				Cost:       1.0,
			},
		},
	})
	require.NoError(t, err)

	group, err := NewURLTestGroupWithSmartRoute(
		ctx, nil, log.NewNOPFactory().Logger(),
		[]adapter.Outbound{iplc, cn2, cmi},
		"", time.Minute, 15, 10*time.Minute, false, cfg,
	)
	require.NoError(t, err)

	// Even though CMI (Tier 2) has 25ms raw RTT and CN2 (Tier 1) has 40ms raw RTT,
	// IPLC (Tier 1, 55ms raw RTT - 25ms preference = 30ms effective score) wins within Tier 1!
	history.RecordProbeSuccess("hk-iplc-01", 55)
	history.RecordProbeSuccess("hk-cn2-01", 40)
	history.RecordProbeSuccess("hk-cmi-01", 25)

	selected, ok := group.Select(N.NetworkTCP)
	require.True(t, ok)
	require.Equal(t, "hk-iplc-01", selected.Tag())

	// If Tier 1 nodes degrade above MaxLatency (150ms), traffic spills over to Tier 2 (hk-cmi-01).
	for range 10 {
		history.RecordProbeSuccess("hk-iplc-01", 300)
		history.RecordProbeSuccess("hk-cn2-01", 280)
	}
	selected, ok = group.Select(N.NetworkTCP)
	require.True(t, ok)
	require.Equal(t, "hk-cmi-01", selected.Tag())
}

func TestSmartRouteCostAndJitterPenalty(t *testing.T) {
	t.Parallel()
	ctx, history := newTestContext()

	expensiveIPLC := newMockOutbound("hk-iplc-expensive", nil)
	cheapBGP := newMockOutbound("hk-bgp-cheap", nil)

	cfg, err := NewSmartRouteConfig(URLTestOptions{
		Tolerance:  10,
		CostWeight: 30,
		HoldTime:   badoption.Duration(1 * time.Millisecond),
		Policies: []URLTestPolicy{
			{
				Match: "iplc",
				Tier:  1,
				Cost:  3.0, // +60ms cost penalty
			},
			{
				Match: "bgp",
				Tier:  1,
				Cost:  0.5, // -15ms cost bonus
			},
		},
	})
	require.NoError(t, err)

	group, err := NewURLTestGroupWithSmartRoute(
		ctx, nil, log.NewNOPFactory().Logger(),
		[]adapter.Outbound{expensiveIPLC, cheapBGP},
		"", time.Minute, 10, 10*time.Minute, false, cfg,
	)
	require.NoError(t, err)

	// Off-peak: both links are clean (IPLC 30ms + 60ms cost = 90; BGP 50ms - 15ms cost = 35).
	// Cheap BGP is selected to save cost.
	history.RecordProbeSuccess("hk-iplc-expensive", 30)
	history.RecordProbeSuccess("hk-bgp-cheap", 50)

	selected, ok := group.Select(N.NetworkTCP)
	require.True(t, ok)
	require.Equal(t, "hk-bgp-cheap", selected.Tag())

	// Peak hours: cheap BGP experiences high jitter and probe loss, while IPLC stays rock-solid.
	history.RecordProbeFailure("hk-bgp-cheap")
	history.RecordProbeSuccess("hk-bgp-cheap", 160)

	selected, ok = group.Select(N.NetworkTCP)
	require.True(t, ok)
	require.Equal(t, "hk-iplc-expensive", selected.Tag())
}

func TestSmartRouteBandwidthOverflowWithoutInterrupt(t *testing.T) {
	t.Parallel()
	ctx, history := newTestContext()

	primary := newMockOutbound("hk-primary-100m", nil)
	secondary := newMockOutbound("hk-secondary-100m", nil)

	cfg, err := NewSmartRouteConfig(URLTestOptions{
		Tolerance: 10,
		Policies: []URLTestPolicy{
			{
				Match:            ".*",
				Tier:             1,
				MaxBandwidthMbps: 100,
				Cost:             1.0,
			},
		},
	})
	require.NoError(t, err)

	group, err := NewURLTestGroupWithSmartRoute(
		ctx, nil, log.NewNOPFactory().Logger(),
		[]adapter.Outbound{primary, secondary},
		"", time.Minute, 10, 10*time.Minute, true, cfg,
	)
	require.NoError(t, err)

	history.RecordProbeSuccess("hk-primary-100m", 30)
	history.RecordProbeSuccess("hk-secondary-100m", 55)
	group.performUpdateCheck()
	require.Equal(t, "hk-primary-100m", group.selectedOutboundTCP.Tag())

	// Saturate primary with 12.5 MB/s (100 Mbps >= 95% of 100 Mbps).
	history.RecordTrafficBytes("hk-primary-100m", 12_500_000)

	// New connections overflow to secondary, while group.selectedOutboundTCP is NOT interrupted!
	urlTestOutbound := &URLTest{group: group}
	require.Equal(t, "hk-secondary-100m", urlTestOutbound.Selected(N.NetworkTCP).Tag())
	group.performUpdateCheck()
	require.Equal(t, "hk-primary-100m", group.selectedOutboundTCP.Tag())
}

func TestSmartRouteHoldDownAndFastFailoverOnDial(t *testing.T) {
	t.Parallel()
	ctx, history := newTestContext()

	var nodeADials int32
	nodeA := newMockOutbound("node-a", func(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
		nodeADials++
		return nil, errors.New("simulated connection reset")
	})
	var nodeBDials int32
	nodeB := newMockOutbound("node-b", func(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
		nodeBDials++
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	})

	cfg, err := NewSmartRouteConfig(URLTestOptions{
		Tolerance:        10,
		FailureThreshold: 3,
		HoldTime:         badoption.Duration(30 * time.Second),
	})
	require.NoError(t, err)

	group, err := NewURLTestGroupWithSmartRoute(
		ctx, nil, log.NewNOPFactory().Logger(),
		[]adapter.Outbound{nodeA, nodeB},
		"", time.Minute, 10, 10*time.Minute, false, cfg,
	)
	require.NoError(t, err)

	history.RecordProbeSuccess("node-a", 30)
	history.RecordProbeSuccess("node-b", 60)
	group.performUpdateCheck()
	require.Equal(t, "node-a", group.selectedOutboundTCP.Tag())

	// Hold-down check: node-b improves to 35ms, and even if node-a becomes 55ms, within 30s HoldTime node-a is retained.
	history.RecordProbeSuccess("node-a", 90)
	history.RecordProbeSuccess("node-b", 30)
	group.performUpdateCheck()
	require.Equal(t, "node-a", group.selectedOutboundTCP.Tag())

	ut := &URLTest{
		logger: log.NewNOPFactory().Logger(),
		group:  group,
	}

	// Dial 1 & 2 fail on node-a via Selected(N.NetworkTCP) (the exact path route/conn.go takes):
	// below FailureThreshold (3), so node-a is not yet circuit-broken.
	_, err = ut.Selected(N.NetworkTCP).DialContext(ctx, N.NetworkTCP, M.ParseSocksaddr("1.1.1.1:443"))
	require.Error(t, err)
	require.Equal(t, "node-a", group.selectedOutboundTCP.Tag())

	_, err = ut.Selected(N.NetworkTCP).DialContext(ctx, N.NetworkTCP, M.ParseSocksaddr("1.1.1.1:443"))
	require.Error(t, err)
	require.Equal(t, "node-a", group.selectedOutboundTCP.Tag())

	// Dial 3 hits FailureThreshold (3): active probe runs on node-a and also fails, confirming node-a is down,
	// while the current request immediately fails over to node-b AND succeeds on the same DialContext call!
	conn, err := ut.Selected(N.NetworkTCP).DialContext(ctx, N.NetworkTCP, M.ParseSocksaddr("1.1.1.1:443"))
	require.NoError(t, err)
	require.NotNil(t, conn)
	_ = conn.Close()
	require.Equal(t, "node-b", group.selectedOutboundTCP.Tag())
	require.Equal(t, int32(4), nodeADials) // 3 passive dials + 1 active verification probe
	require.Equal(t, int32(1), nodeBDials)
}

func TestSmartRouteActiveProbePreventsFalseCircuitBreakOnTargetFailure(t *testing.T) {
	t.Parallel()
	ctx, history := newTestContext()

	nodeA := newMockOutbound("node-a", func(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
		// Active probe to 1.1.1.1:80 succeeds with HTTP 200 OK
		if destination.AddrString() == "1.1.1.1" && destination.Port == 80 {
			c1, c2 := net.Pipe()
			go func() {
				defer c2.Close()
				buf := make([]byte, 512)
				_, _ = c2.Read(buf)
				_, _ = c2.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
			}()
			return c1, nil
		}
		// Target site dead.example.com:443 refuses connection
		return nil, errors.New("target site connection refused")
	})
	nodeB := newMockOutbound("node-b", func(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
		return nil, errors.New("target site connection refused")
	})

	cfg, err := NewSmartRouteConfig(URLTestOptions{
		Tolerance:        10,
		FailureThreshold: 3,
	})
	require.NoError(t, err)

	group, err := NewURLTestGroupWithSmartRoute(
		ctx, nil, log.NewNOPFactory().Logger(),
		[]adapter.Outbound{nodeA, nodeB},
		"", time.Minute, 10, 10*time.Minute, false, cfg,
	)
	require.NoError(t, err)

	history.RecordProbeSuccess("node-a", 30)
	history.RecordProbeSuccess("node-b", 60)
	group.performUpdateCheck()
	require.Equal(t, "node-a", group.selectedOutboundTCP.Tag())

	ut := &URLTest{
		logger: log.NewNOPFactory().Logger(),
		group:  group,
	}

	// 3 consecutive failures to dead.example.com:443 reach FailureThreshold (3),
	// triggering the active probe to http://1.1.1.1/cdn-cgi/trace which SUCCEEDS.
	for i := 0; i < 3; i++ {
		_, err = ut.Selected(N.NetworkTCP).DialContext(ctx, N.NetworkTCP, M.ParseSocksaddr("dead.example.com:443"))
		require.Error(t, err)
	}

	snap, ok := history.LoadLinkMetrics("node-a")
	require.True(t, ok)
	require.True(t, snap.Available)
	require.False(t, snap.CircuitOpen)
	require.Equal(t, uint32(0), snap.ConsecutivePassiveFailures)
	require.Equal(t, "node-a", group.selectedOutboundTCP.Tag())
}

func TestSmartRouteStatusAndDynamicConfigUpdate(t *testing.T) {
	t.Parallel()
	ctx, history := newTestContext()

	nodeIPLC := newMockOutbound("hk-iplc-01", nil)
	nodeBGP := newMockOutbound("hk-bgp-01", nil)

	cfg, err := NewSmartRouteConfig(URLTestOptions{
		Tolerance: 10,
	})
	require.NoError(t, err)

	group, err := NewURLTestGroupWithSmartRoute(
		ctx, nil, log.NewNOPFactory().Logger(),
		[]adapter.Outbound{nodeIPLC, nodeBGP},
		"", time.Minute, 10, 10*time.Minute, false, cfg,
	)
	require.NoError(t, err)
	require.Equal(t, DefaultURLTestURL, group.link)

	history.RecordProbeSuccess("hk-iplc-01", 55)
	history.RecordProbeSuccess("hk-bgp-01", 40)
	group.performUpdateCheck()

	ut := &URLTest{
		Adapter: outbound.NewAdapter(C.TypeURLTest, "auto-smartroute", []string{N.NetworkTCP, N.NetworkUDP}, []string{"hk-iplc-01", "hk-bgp-01"}),
		group:   group,
	}

	status := ut.SmartRouteStatus()
	require.Equal(t, "auto-smartroute", status.Tag)
	require.Equal(t, "hk-bgp-01", status.SelectedTCP)
	require.Len(t, status.Nodes, 2)

	// Dynamically update policy via API DTO to prefer IPLC (-30ms preference).
	err = ut.UpdateSmartRouteConfig(SmartRouteGroupConfigDTO{
		Tolerance:  10,
		HoldTimeMs: 1,
		Policies: []URLTestPolicy{
			{
				Match:      "iplc",
				Tier:       1,
				Preference: -30,
				Cost:       1.0,
			},
		},
	})
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	group.performUpdateCheck()

	updatedStatus := ut.SmartRouteStatus()
	require.Equal(t, "hk-iplc-01", updatedStatus.SelectedTCP)
}

func TestSmartRouteSameTierRespectsToleranceAndPassiveDialDoesNotInflateRTT(t *testing.T) {
	t.Parallel()
	ctx, history := newTestContext()

	jp := newMockOutbound("jp-cn2", nil)
	us := newMockOutbound("us-cn2gia", nil)

	// Both nodes are Tier 1, Tolerance is 5000, MaxLatency is 400ms.
	cfg, err := NewSmartRouteConfig(URLTestOptions{
		Tolerance:  5000,
		MaxLatency: 400,
		HoldTime:   badoption.Duration(15 * time.Second),
	})
	require.NoError(t, err)

	group, err := NewURLTestGroupWithSmartRoute(
		ctx, nil, log.NewNOPFactory().Logger(),
		[]adapter.Outbound{jp, us},
		"", 20*time.Second, 5000, 10*time.Minute, false, cfg,
	)
	require.NoError(t, err)

	history.RecordProbeSuccess("jp-cn2", 78)
	history.RecordProbeSuccess("us-cn2gia", 215)
	group.performUpdateCheck()
	require.Equal(t, "jp-cn2", group.selectedOutboundTCP.Tag())

	// Slow passive destination dial (e.g., 1200ms to a distant web server) must NOT inflate jp-cn2's SmoothedRTT.
	history.RecordPassiveSuccess("jp-cn2", 1200*time.Millisecond)
	snap, ok := history.LoadLinkMetrics("jp-cn2")
	require.True(t, ok)
	require.InDelta(t, 78.0, snap.SmoothedRTT, 0.1)

	// Even if jp-cn2 probe RTT rises to 493ms (>400ms MaxLatency) while us-cn2gia is 359ms (<=400ms),
	// since both are in the SAME Tier (T1) and delta (493 - 359 = 134) < Tolerance (5000),
	// same-tier hysteresis MUST suppress the switch instead of bypassing tolerance.
	for range 15 {
		history.RecordProbeSuccess("jp-cn2", 493)
		history.RecordProbeSuccess("us-cn2gia", 359)
	}
	group.performUpdateCheck()
	require.Equal(t, "jp-cn2", group.selectedOutboundTCP.Tag())
}

func TestSmartRouteContextOptionsUnmarshal(t *testing.T) {
	t.Parallel()
	ctx := Context(context.Background())
	rawJSON := []byte(`{
		"outbounds": [
			{
				"type": "urltest",
				"tag": "auto-urltest",
				"outbounds": ["direct"],
				"algorithm": "lcb_robust",
				"jitter_weight": 2.5,
				"policies": [
					{"match": ".*", "tier": 1, "preference": -15}
				]
			},
			{
				"type": "smartroute",
				"tag": "auto-smartroute",
				"outbounds": ["direct"],
				"algorithm": "mos_itu",
				"max_latency": 350
			},
			{
				"type": "direct",
				"tag": "direct"
			}
		]
	}`)
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, rawJSON)
	require.NoError(t, err)
	require.Len(t, opts.Outbounds, 3)

	utOpts1, ok := opts.Outbounds[0].Options.(*URLTestOptions)
	require.True(t, ok)
	require.Equal(t, AlgorithmLCBRobust, utOpts1.Algorithm)
	require.InDelta(t, 2.5, utOpts1.JitterWeight, 0.001)
	require.Len(t, utOpts1.Policies, 1)

	utOpts2, ok := opts.Outbounds[1].Options.(*URLTestOptions)
	require.True(t, ok)
	require.Equal(t, AlgorithmMOSITU, utOpts2.Algorithm)
	require.Equal(t, uint16(350), utOpts2.MaxLatency)
}

type mockGroupOutbound struct {
	*mockOutbound
	members  []string
	selected adapter.Outbound
}

func (g *mockGroupOutbound) Now() string {
	if g.selected == nil {
		return ""
	}
	return g.selected.Tag()
}

func (g *mockGroupOutbound) All() []string {
	return g.members
}

func (g *mockGroupOutbound) Selected(network string) adapter.Outbound {
	return g.selected
}

func (g *mockGroupOutbound) AttachConnection(closer io.Closer) func() {
	return func() {}
}

func newHTTP200Outbound(tag string, counter *int32) *mockOutbound {
	return newMockOutbound(tag, func(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
		if counter != nil {
			*counter++
		}
		c1, c2 := net.Pipe()
		go func() {
			defer c2.Close()
			buf := make([]byte, 512)
			_, _ = c2.Read(buf)
			_, _ = c2.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
		}()
		return c1, nil
	})
}

func TestURLTestOutboundsOnlyProbesCurrentSelectedNodeForSelectAndURLTest(t *testing.T) {
	t.Parallel()
	ctx, metrics := newTestContext()

	var select1Probes, select2Probes int32
	selectNode1 := newHTTP200Outbound("select-node-1", &select1Probes)
	selectNode2 := newHTTP200Outbound("select-node-2", &select2Probes)
	selectorGroup := &mockGroupOutbound{
		mockOutbound: &mockOutbound{
			Adapter: outbound.NewAdapter(C.TypeSelector, "select-group", []string{N.NetworkTCP, N.NetworkUDP}, []string{"select-node-1", "select-node-2"}),
		},
		members:  []string{"select-node-1", "select-node-2"},
		selected: selectNode1,
	}

	var ut1Probes, ut2Probes int32
	utNode1 := newHTTP200Outbound("ut-node-1", &ut1Probes)
	utNode2 := newHTTP200Outbound("ut-node-2", &ut2Probes)
	nestedURLTestGroup, err := NewURLTestGroupWithSmartRoute(
		ctx, nil, log.NewNOPFactory().Logger(),
		[]adapter.Outbound{utNode1, utNode2},
		DefaultURLTestURL, time.Minute, 10, 10*time.Minute, false, SmartRouteConfig{},
	)
	require.NoError(t, err)
	nestedURLTestGroup.selectedOutboundTCP = utNode1
	nestedURLTest := &URLTest{
		Adapter: outbound.NewAdapter(C.TypeURLTest, "nested-urltest-group", []string{N.NetworkTCP, N.NetworkUDP}, []string{"ut-node-1", "ut-node-2"}),
		tags:    []string{"ut-node-1", "ut-node-2"},
		group:   nestedURLTestGroup,
	}

	_ = selectNode2
	result := URLTestOutbounds(
		ctx, nil, metrics, log.NewNOPFactory().Logger(),
		[]adapter.Outbound{selectorGroup, nestedURLTest},
		DefaultURLTestURL, time.Minute, true,
	)

	require.Equal(t, int32(1), select1Probes)
	require.Equal(t, int32(0), select2Probes)
	require.Equal(t, int32(1), ut1Probes)
	require.Equal(t, int32(0), ut2Probes)
	require.NotZero(t, result["select-node-1"])
	require.NotZero(t, result["select-group"])
	require.Zero(t, result["select-node-2"])
	require.NotZero(t, result["ut-node-1"])
	require.NotZero(t, result["nested-urltest-group"])
	require.Zero(t, result["ut-node-2"])
}


