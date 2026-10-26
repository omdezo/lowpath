package diag

import (
	"context"
	"testing"
	"time"

	"lowpath/internal/icmp"
)

// TestInternetGatewayPinger pings this PC's real router for 8 seconds with
// the OS's route reader and ping, as the app does. It needs a network, not
// the internet, but sends real packets, so it runs with the internet tests:
//
//	$env:LOWPATH_INTERNET=1; go test -run TestInternetGateway -v ./internal/diag
func TestInternetGatewayPinger(t *testing.T) {
	needInternet(t)
	g := NewGatewayPinger(GatewayConfig{Net: NewNetStats(), Ping: icmp.Ping, PingTTL: icmp.PingTTL})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	end := time.Now().Add(8 * time.Second)
	var v GatewayView
	for time.Now().Before(end) {
		g.Touch()
		time.Sleep(250 * time.Millisecond)
	}
	v = g.View()
	// The address is the LAN router's; the log leaves it out anyway.
	t.Logf("state %s (%s) on %s, %.0f Hz; note %q", v.State, v.Message, linkWords(v.Link, v.Interface), v.RateHz, v.Note)
	switch v.State {
	case GatewayOn:
		s := v.Stats
		t.Logf("%d sent, %d lost (%.1f%%), median %s ms, p95 %s ms, p99 %s ms, max %s ms, jitter %s ms, %d spikes, longest loss run %d, limited %v",
			s.Sent, s.Lost, s.LossPct, msText(s.P50MS), msText(s.P95MS), msText(s.P99MS), msText(s.MaxMS), msText(s.JitterMS), s.Spikes, s.MaxLossRun, s.Limited)
		if s.Sent < 100 {
			t.Errorf("only %d pings settled in 8 s at 20 Hz", s.Sent)
		}
	case GatewayNoReply, GatewayNoGateway, GatewayOffline, GatewayNoPermission:
		// Real networks do this; the view must say why.
		if v.Message == "" {
			t.Errorf("%s without a reason", v.State)
		}
	default:
		t.Errorf("state %s: %s", v.State, v.Message)
	}
	if !v.Running {
		t.Error("not running while touched")
	}
}
