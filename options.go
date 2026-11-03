package smartroute

import (
	"time"

	"github.com/sagernet/sing/common/json/badoption"
)

const (
	TypeSmartRoute            = "smartroute"
	DefaultURLTestURL         = "http://1.1.1.1/cdn-cgi/trace"
	DefaultURLTestInterval    = 30 * time.Second
	DefaultURLTestIdleTimeout = 30 * time.Minute
)

type URLTestOptions struct {
	Outbounds                 []string           `json:"outbounds" reference:"outbound"`
	URL                       string             `json:"url,omitempty"`
	Interval                  badoption.Duration `json:"interval,omitempty"`
	Tolerance                 uint16             `json:"tolerance,omitempty"`
	IdleTimeout               badoption.Duration `json:"idle_timeout,omitempty"`
	InterruptExistConnections bool               `json:"interrupt_exist_connections,omitempty"`
	Algorithm                 string             `json:"algorithm,omitempty"`
	Policies                  []URLTestPolicy    `json:"policies,omitempty"`
	JitterWeight              float64            `json:"jitter_weight,omitempty"`
	LossWeight                float64            `json:"loss_weight,omitempty"`
	CostWeight                float64            `json:"cost_weight,omitempty"`
	BandwidthWeight           float64            `json:"bandwidth_weight,omitempty"`
	MaxLatency                uint16             `json:"max_latency,omitempty"`
	MaxLoss                   float64            `json:"max_loss,omitempty"`
	FailureThreshold          uint8              `json:"failure_threshold,omitempty"`
	HoldTime                  badoption.Duration `json:"hold_time,omitempty"`
	StatWindow                uint8              `json:"stat_window,omitempty"`
	PenaltyHalfLife           badoption.Duration `json:"penalty_half_life,omitempty"`
	BandwidthWindow           badoption.Duration `json:"bandwidth_window,omitempty"`
}

type URLTestPolicy struct {
	Match            string  `json:"match,omitempty"`
	Tier             int     `json:"tier,omitempty"`
	Preference       int     `json:"preference,omitempty"`
	MaxBandwidthMbps uint32  `json:"max_bandwidth_mbps,omitempty"`
	Cost             float64 `json:"cost,omitempty"`
}
