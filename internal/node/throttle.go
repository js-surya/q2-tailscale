package node

import (
	"sync"
	"time"

	"tailscale.com/ipn"
)

// throttle runs f at most once a gap: a kick after a quiet gap runs it at once; the kicks within the gap
// after a run fold into one more run at the gap's end. Tailscale sends its bus messages in bursts, and
// each refresh is a full status (every peer, as JSON), so a burst is one refresh, or two.
type throttle struct {
	gap     time.Duration
	f       func()
	mu      sync.Mutex
	last    time.Time
	pending bool
}

func newThrottle(gap time.Duration, f func()) *throttle { return &throttle{gap: gap, f: f} }

func (t *throttle) kick() {
	t.mu.Lock()
	if t.pending {
		t.mu.Unlock()
		return
	}
	if wait := t.gap - time.Since(t.last); wait > 0 {
		t.pending = true
		t.mu.Unlock()
		time.AfterFunc(wait, func() {
			t.mu.Lock()
			t.pending, t.last = false, time.Now()
			t.mu.Unlock()
			t.f()
		})
		return
	}
	t.last = time.Now()
	t.mu.Unlock()
	t.f()
}

// statsOnly is a bus message with nothing but the engine's statistics (bytes, the live peers' count),
// which nothing the page shows comes from: it needs no refresh.
func statsOnly(m ipn.Notify) bool {
	return m.Engine != nil && m.ErrMessage == nil && m.LoginFinished == nil && m.State == nil &&
		m.Prefs == nil && m.SelfChange == nil && m.InitialStatus == nil && len(m.PeerChangedPatch) == 0 &&
		len(m.PeersChanged) == 0 && len(m.PeersRemoved) == 0 && len(m.UserProfiles) == 0 &&
		len(m.PeerState) == 0 && m.BrowseToURL == nil && m.FilesWaiting == nil && len(m.IncomingFiles) == 0 &&
		len(m.OutgoingFiles) == 0 && m.Health == nil
}
