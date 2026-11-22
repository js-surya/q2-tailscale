// Package loopback puts the tailnet's Tailscale IPs on the Q2's loopback interface, so a connection
// another program makes to 100.x.y.z lands on a local listener (internal/relay) instead of needing a route
// the Q2 has no VPN for. Each address is a /32 labelled lo:q2ts, so a killed run's leftovers are found and
// another program's addresses are never touched.
package loopback

import (
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Label marks our addresses on lo.
const Label = "lo:q2ts"

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Runner runs busybox ip with args.
type Runner func(args ...string) ([]byte, error)

// IP runs the ip command.
func IP(args ...string) ([]byte, error) { return exec.Command("ip", args...).CombinedOutput() }

// After an add fails, the next is tried retryFirst on, the wait doubling to retryMax.
const (
	retryFirst = 30 * time.Second
	retryMax   = 5 * time.Minute
)

// Manager keeps the set of our addresses on lo.
type Manager struct {
	Run  Runner
	Now  func() time.Time // time.Now when nil
	have map[netip.Addr]bool

	// After a failed add no other is tried until next: each add is a process, and a Q2 that
	// refuses them all (a sandbox without CAP_NET_ADMIN) would start one per address per sync.
	next time.Time
	wait time.Duration
}

func (m *Manager) now() time.Time {
	if m.Now == nil {
		return time.Now()
	}
	return m.Now()
}

func (m *Manager) run(args ...string) ([]byte, error) {
	if m.Run == nil {
		return IP(args...)
	}
	return m.Run(args...)
}

// CleanupStale removes every address labelled ours, left on lo by a run that didn't end cleanly.
func (m *Manager) CleanupStale() error {
	out, err := m.run("-4", "addr", "show", "dev", "lo")
	if err != nil {
		return fmt.Errorf("ip addr show: %v: %s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "inet" && f[len(f)-1] == Label {
			m.run("addr", "del", f[1], "dev", "lo")
		}
	}
	return nil
}

// Sync makes our addresses on lo exactly want: each missing one added, each extra removed. Only
// addresses in 100.64.0.0/10 are taken; any other makes it fail before changing anything.
func (m *Manager) Sync(want []netip.Addr) error {
	set := map[netip.Addr]bool{}
	for _, a := range want {
		if !a.Is4() || !cgnat.Contains(a) {
			return fmt.Errorf("loopback: %v is not a Tailscale IPv4", a)
		}
		set[a] = true
	}
	if m.have == nil {
		m.have = map[netip.Addr]bool{}
	}
	var errs []string
	for _, a := range sorted(set) {
		if m.have[a] {
			continue
		}
		if now := m.now(); now.Before(m.next) {
			break // waiting after a failure: the rest are tried then
		}
		if out, err := m.run("addr", "add", a.String()+"/32", "dev", "lo", "label", Label); err != nil &&
			!strings.Contains(string(out), "File exists") {
			errs = append(errs, fmt.Sprintf("add %v: %v: %s", a, err, strings.TrimSpace(string(out))))
			m.wait = min(max(2*m.wait, retryFirst), retryMax)
			m.next = m.now().Add(m.wait)
			break
		}
		m.have[a] = true
		m.wait = 0
	}
	for _, a := range sorted(m.have) {
		if !set[a] {
			m.run("addr", "del", a.String()+"/32", "dev", "lo")
			delete(m.have, a)
		}
	}
	if errs != nil {
		return fmt.Errorf("loopback: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Have is our addresses on lo, sorted.
func (m *Manager) Have() []netip.Addr { return sorted(m.have) }

// Close removes all of our addresses.
func (m *Manager) Close() error { return m.Sync(nil) }

func sorted(s map[netip.Addr]bool) []netip.Addr {
	out := make([]netip.Addr, 0, len(s))
	for a := range s {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return out
}
