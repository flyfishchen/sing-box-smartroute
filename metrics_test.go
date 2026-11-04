package smartroute_test

import (
	"testing"
	"time"

	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/smartroute"
	"github.com/stretchr/testify/require"
)

func TestLinkMetricsEWMAAndJitter(t *testing.T) {
	t.Parallel()
	storage := smartroute.NewMetricsStorage(urltest.NewHistoryStorage())

	smoothed1 := storage.RecordProbeSuccess("node-a", 40)
	require.Equal(t, uint16(40), smoothed1)

	snap, ok := storage.LoadLinkMetrics("node-a")
	require.True(t, ok)
	require.True(t, snap.Available)
	require.InDelta(t, 40.0, snap.SmoothedRTT, 0.1)
	require.InDelta(t, 0.0, snap.Jitter, 0.1)

	smoothed2 := storage.RecordProbeSuccess("node-a", 140)
	require.Equal(t, uint16(70), smoothed2) // 0.7*40 + 0.3*140 = 70

	snap, ok = storage.LoadLinkMetrics("node-a")
	require.True(t, ok)
	require.InDelta(t, 70.0, snap.SmoothedRTT, 0.1)
	require.InDelta(t, 25.0, snap.Jitter, 0.1) // 0.25 * |140 - 40| = 25
}

func TestLinkMetricsPassiveCircuitBreaker(t *testing.T) {
	t.Parallel()
	storage := smartroute.NewMetricsStorage(urltest.NewHistoryStorage())
	storage.RecordProbeSuccess("node-primary", 35)

	require.False(t, storage.RecordPassiveFailure("node-primary", 3))
	require.NotNil(t, storage.LoadURLTestHistory("node-primary"))

	require.False(t, storage.RecordPassiveFailure("node-primary", 3))
	require.NotNil(t, storage.LoadURLTestHistory("node-primary"))

	storage.RecordPassiveSuccess("node-primary", 40*time.Millisecond)
	snap, _ := storage.LoadLinkMetrics("node-primary")
	require.Equal(t, uint32(0), snap.ConsecutivePassiveFailures)

	require.False(t, storage.RecordPassiveFailure("node-primary", 3))
	require.False(t, storage.RecordPassiveFailure("node-primary", 3))
	require.True(t, storage.RecordPassiveFailure("node-primary", 3))
	require.Nil(t, storage.LoadURLTestHistory("node-primary"))

	snap, _ = storage.LoadLinkMetrics("node-primary")
	require.False(t, snap.Available)
	require.True(t, snap.CircuitOpen)
	require.Greater(t, snap.Penalty, 100.0)
}

func TestLinkMetricsThroughputEstimation(t *testing.T) {
	t.Parallel()
	storage := smartroute.NewMetricsStorage(urltest.NewHistoryStorage())
	storage.RecordProbeSuccess("node-bw", 30)

	storage.RecordTrafficBytes("node-bw", 10*1024*1024)
	snap, ok := storage.LoadLinkMetrics("node-bw")
	require.True(t, ok)
	require.Equal(t, uint64(10*1024*1024), snap.TotalBytes)
	require.Equal(t, uint64(10*1024*1024*8), snap.ThroughputBps)

	storage.SetWindowConfig(smartroute.MetricsWindowConfig{
		StatWindow:      1,
		PenaltyHalfLife: 10 * time.Second,
		BandwidthWindow: 4 * time.Second,
	})
	snap, ok = storage.LoadLinkMetrics("node-bw")
	require.True(t, ok)
	require.Equal(t, uint64(10*1024*1024*8/4), snap.ThroughputBps)

	smoothed := storage.RecordProbeSuccess("node-bw", 120)
	require.Equal(t, uint16(120), smoothed)
}

func TestLinkMetricsActiveProbeFailureTolerance(t *testing.T) {
	t.Parallel()
	storage := smartroute.NewMetricsStorage(urltest.NewHistoryStorage())

	// Uninitialized node fails probe -> immediately unavailable
	storage.RecordProbeFailure("node-cold")
	coldSnap, ok := storage.LoadLinkMetrics("node-cold")
	require.True(t, ok)
	require.False(t, coldSnap.Available)

	// Established node succeeds first
	storage.RecordProbeSuccess("node-warm", 50)
	snap1, _ := storage.LoadLinkMetrics("node-warm")
	require.True(t, snap1.Available)

	// 1st probe failure on warm node -> stays available with degraded loss/penalty, not immediately offline
	storage.RecordProbeFailure("node-warm")
	snap2, _ := storage.LoadLinkMetrics("node-warm")
	require.True(t, snap2.Available)
	require.Equal(t, uint32(1), snap2.ConsecutiveProbeFailures)
	require.Greater(t, snap2.LossRate, 0.0)
	require.InDelta(t, 50.0, snap2.Penalty, 0.5)
	require.NotNil(t, storage.LoadURLTestHistory("node-warm"))

	// 2nd consecutive probe failure -> marked unavailable (offline)
	storage.RecordProbeFailure("node-warm")
	snap3, _ := storage.LoadLinkMetrics("node-warm")
	require.False(t, snap3.Available)
	require.Equal(t, uint32(2), snap3.ConsecutiveProbeFailures)
	require.Nil(t, storage.LoadURLTestHistory("node-warm"))
}

