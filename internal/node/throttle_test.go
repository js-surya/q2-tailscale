package node

import (
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/health"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
)

// A burst of kicks is one run at once and one more at the gap's end, not one a kick.
func TestThrottleFoldsABurst(t *testing.T) {
	var runs atomic.Int32
	th := newThrottle(100*time.Millisecond, func() { runs.Add(1) })
	for i := 0; i < 20; i++ {
		th.kick()
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("at once: %d runs", n)
	}
	time.Sleep(250 * time.Millisecond)
	if n := runs.Load(); n != 2 {
		t.Fatalf("after the gap: %d runs", n)
	}
}

// A kick after a quiet gap runs at once.
func TestThrottleRunsAtOnceWhenQuiet(t *testing.T) {
	var runs atomic.Int32
	th := newThrottle(50*time.Millisecond, func() { runs.Add(1) })
	th.kick()
	time.Sleep(120 * time.Millisecond)
	th.kick()
	if n := runs.Load(); n != 2 {
		t.Fatalf("%d runs", n)
	}
}

// Tailscale's engine statistics change nothing the page shows: such a message alone needs no refresh;
// one with a state, the peers or health does.
func TestEngineStatsAloneNeedNoRefresh(t *testing.T) {
	if !statsOnly(ipn.Notify{Engine: &ipn.EngineStatus{}}) {
		t.Fatal("engine stats refreshed")
	}
	st := ipn.Running
	for _, m := range []ipn.Notify{{Engine: &ipn.EngineStatus{}, State: &st}, {Health: &health.State{}},
		{PeersRemoved: []tailcfg.NodeID{1}}, {}} {
		if statsOnly(m) {
			t.Fatalf("%+v skipped", m)
		}
	}
}
