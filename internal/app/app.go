// Package app keeps the Q2 in step with the tailnet: on each change of the node, and every few seconds,
// it writes the status file Q2 Pod reads (the sign-in QR code, the status bar icon), puts the machines'
// Tailscale IPs on lo with listeners on the port list, sets up the forwards, and keeps our name server
// first in resolv.conf. Run undoes all of it when it ends, and a run that was killed is cleaned up at the
// next start.
package app

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"q2-tailscale/internal/config"
	"q2-tailscale/internal/dnsd"
	"q2-tailscale/internal/loopback"
	"q2-tailscale/internal/node"
	"q2-tailscale/internal/qr"
	"q2-tailscale/internal/relay"
	"q2-tailscale/internal/resolvconf"
)

// Relayer is the listeners: internal/relay.
type Relayer interface {
	Sync([]relay.Target) map[netip.AddrPort]error
	Close()
}

// App is the running app. Dir is the card folder (settings, forwards.txt); ResolvPath /etc/resolv.conf;
// StatePath Q2 Pod's status file (/tmp/q2-tailscale.state); DNSAddr where the name server listens
// (127.0.0.1:53). Every is the re-check period (3 s if 0).
type App struct {
	Node                                node.Node
	Loop                                *loopback.Manager
	Relay                               Relayer
	Dir, ResolvPath, StatePath, DNSAddr string
	Every                               time.Duration
	Logf                                func(string, ...any)

	mu       sync.Mutex
	st       node.Status
	problems map[uint16]string // a forward's local port: why it doesn't work

	// out guards what the rest of the Q2 sees of the app, resolv.conf and the status file; stopping,
	// set at a stop, keeps a reconcile still running from writing them again.
	out      sync.Mutex
	stopping bool

	failing map[string]bool // the listen and lo errors of the last reconcile, logged when they first came

	dnsDown atomic.Bool // the last forwarded question had no answer from the Wi-Fi's name servers

	Now      func() time.Time // time.Now when nil
	authURL  string           // the last sign-in link shown
	signedAt time.Time        // when it was signed in with, for the QR page's minute

	note   string    // what Taildrop last brought, for the menu
	noteAt time.Time // when
}

// noteFor is how long the menu says what Taildrop brought.
const noteFor = 5 * time.Minute

// Got hears the Taildrop receiver: the files it just saved.
func (a *App) Got(names []string) {
	note := "Received " + names[0]
	if len(names) > 1 {
		note = "Received " + strconv.Itoa(len(names)) + " files"
	}
	a.mu.Lock()
	a.note, a.noteAt = note, a.now()
	a.mu.Unlock()
}

// Received is the menu's note on what Taildrop brought, for noteFor after it came; "" otherwise.
func (a *App) Received() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.note == "" || a.now().Sub(a.noteAt) > noteFor {
		return ""
	}
	return a.note
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// Status is the node's status as of the last reconcile.
func (a *App) Status() node.Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st
}

// ForwardProblem says why the forward on 127.0.0.1:port doesn't work, "" when it does.
func (a *App) ForwardProblem(port uint16) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.problems[port]
}

// Lookup answers the name server: a machine's or a Tailscale Service's IP by its full name (not ours: nothing
// listens on our own IP here).
func (a *App) Lookup(name string) (netip.Addr, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st.State != "Running" {
		return netip.Addr{}, false
	}
	for _, p := range names(a.st) {
		if p.FQDN == name {
			return p.IP, true
		}
	}
	return netip.Addr{}, false
}

// Suffix is the MagicDNS suffix while signed in, else "".
func (a *App) Suffix() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st.State != "Running" {
		return ""
	}
	return a.st.Suffix
}

// names are everything on the tailnet with a name and an IPv4: machines, then Tailscale Services.
func names(st node.Status) []node.Peer {
	return append(slices.Clone(st.Peers), st.Services...)
}

// peer finds a forward's machine by full name, short name or Tailscale IP.
func peer(peers []node.Peer, host string) (node.Peer, bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range peers {
		if p.FQDN == host || p.Name == host || p.IP.String() == host {
			return p, true
		}
	}
	return node.Peer{}, false
}

// Reconcile brings the status file, lo, the listeners and resolv.conf in line with the node now.
func (a *App) Reconcile() {
	st := a.Node.Status()
	a.mu.Lock()
	a.st = st
	a.mu.Unlock()

	settings := config.LoadSettings(a.Dir)
	off := settings.Off
	state := "bar=off\n"
	switch {
	case off:
		state = "" // turned off on the page: no icon
	case st.State == "NeedsLogin" && st.AuthURL != "":
		a.authURL, a.signedAt = st.AuthURL, time.Time{}
		state = signIn(st.AuthURL, "Scan to sign in to Tailscale") + "bar=off\n"
	case st.State == "Running" && a.authURL != "" && a.keepCode():
		// just signed in: the QR page, still open, keeps its code and says so (it reads a file
		// without one as no Wi-Fi), for a minute
		state = signIn(a.authURL, "Signed in to Tailscale · press Return") + "bar=on\n"
	case st.State == "Running" && a.Warnings() != nil:
		state = "bar=warn\n" // the icon with its red mark
	case st.State == "Running":
		state = "bar=on\n"
	}
	a.out.Lock()
	if !a.stopping {
		if err := writeWhole(a.StatePath, state); err != nil {
			a.logf("status file: %v", err)
		}
	}
	a.out.Unlock()

	var ips []netip.Addr
	var targets []relay.Target
	problems := map[uint16]string{}
	if st.State == "Running" && !off {
		ports := config.AllPorts(settings)
		for _, p := range names(st) {
			ips = append(ips, p.IP)
			for _, port := range ports {
				ap := netip.AddrPortFrom(p.IP, port)
				targets = append(targets, relay.Target{Listen: ap, Dial: ap})
			}
		}
		forwards, errs := config.LoadForwards(a.Dir)
		for _, err := range errs {
			a.logf("%v", err)
		}
		for _, f := range forwards {
			p, ok := peer(names(st), f.Host)
			if !ok {
				problems[f.LocalPort] = f.Host + " isn't on the tailnet"
				continue
			}
			targets = append(targets, relay.Target{Listen: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), f.LocalPort),
				Dial: netip.AddrPortFrom(p.IP, f.Port)})
		}
	}
	failing := map[string]bool{}
	if err := a.Loop.Sync(ips); err != nil {
		failing[err.Error()] = true
	}
	for addr, err := range a.Relay.Sync(targets) {
		if addr.Addr().IsLoopback() {
			problems[addr.Port()] = "port taken: " + err.Error()
		} else {
			failing[fmt.Sprintf("listen %v: %v", addr, err)] = true
		}
	}
	for _, msg := range slices.Sorted(maps.Keys(failing)) {
		if !a.failing[msg] {
			a.logf("%s", msg)
		}
	}
	a.failing = failing
	a.mu.Lock()
	a.problems = problems
	a.mu.Unlock()
	a.out.Lock()
	defer a.out.Unlock()
	if a.stopping {
		return
	}
	if off {
		if err := resolvconf.Restore(a.ResolvPath); err != nil {
			a.logf("resolv.conf: %v", err)
		}
		return
	}
	if _, err := resolvconf.ApplyChanged(a.ResolvPath, a.Suffix()); err != nil {
		a.logf("resolv.conf: %v", err)
	}
}

// signIn is the status file's lines for Q2 Pod's QR page (q2-pod addon.c qr_refresh): the code, drawn
// here (qr: its rows in hex; qrsize: its width), and the link under it, for a phone that can't scan.
func signIn(url, note string) string {
	addr := strings.TrimPrefix(url, "https://")
	c, err := qr.Encode([]byte(url))
	if err != nil {
		return "addr=" + addr + "\nnote=" + note + "\n"
	}
	return "addr=" + addr + "\nqr=" + c.Hex() + "\nqrsize=" + strconv.Itoa(c.Size) + "\nnote=" + note + "\n"
}

// keepCode is whether the sign-in's minute on the QR page is still running; it starts at the first
// reconcile that sees the node signed in, and its end forgets the link.
func (a *App) keepCode() bool {
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	if a.signedAt.IsZero() {
		a.signedAt = now
	}
	if now.Sub(a.signedAt) <= time.Minute {
		return true
	}
	a.authURL = ""
	return false
}

// dnsWarning is Tailscale's own title for it, as its apps show it.
var dnsWarning = node.Warning{Title: "DNS unavailable",
	Text: "The Wi-Fi's name servers aren't answering: names outside the tailnet may not work."}

// Forwarded hears the name server's forwards: whether the Wi-Fi's name servers answered.
func (a *App) Forwarded(ok bool) { a.dnsDown.Store(!ok) }

// Warnings are what's wrong while connected: Tailscale's health warnings, then DNS; nil when nothing is.
func (a *App) Warnings() []node.Warning {
	st := a.Node.Status()
	if st.State != "Running" {
		return nil
	}
	ws := slices.Clone(st.Warnings)
	if a.dnsDown.Load() {
		ws = append(ws, dnsWarning)
	}
	return ws
}

// SetOn turns Tailscale on or off, keeping the choice: off is signed in but not connected. The
// reconcile that follows the node's change takes the listeners, lo and resolv.conf along.
func (a *App) SetOn(ctx context.Context, on bool) error {
	s := config.LoadSettings(a.Dir)
	s.Off = !on
	if err := config.SaveSettings(a.Dir, s); err != nil {
		return err
	}
	return a.Node.SetRunning(ctx, on)
}

// release gives resolv.conf back and drops the status file, for good: the first thing a stop does, even
// in the middle of a reconcile, as a KILL 2 s after the TERM would leave the Q2 asking our name server.
func (a *App) release() {
	a.out.Lock()
	defer a.out.Unlock()
	if a.stopping {
		return
	}
	a.stopping = true
	if err := resolvconf.Restore(a.ResolvPath); err != nil {
		a.logf("resolv.conf: %v", err)
	}
	os.Remove(a.StatePath)
}

// Run cleans a killed run's leftovers, starts the name server and the node, reconciles on every change
// and every few seconds, and when ctx ends undoes it all: listeners, lo, resolv.conf, the status file,
// the node.
func (a *App) Run(ctx context.Context) error {
	if err := a.Loop.CleanupStale(); err != nil {
		a.logf("%v", err)
	}
	resolvconf.Restore(a.ResolvPath)
	a.out.Lock()
	a.stopping = false
	a.out.Unlock()
	go func() { <-ctx.Done(); a.release() }()

	dnsCtx, stopDNS := context.WithCancel(context.Background())
	defer stopDNS()
	srv := &dnsd.Server{Table: a, Forwarded: a.Forwarded, Upstream: func() []netip.AddrPort {
		b, _ := os.ReadFile(a.ResolvPath)
		return resolvconf.Upstreams(b)
	}}
	dnsErr := make(chan error, 1)
	go func() { dnsErr <- srv.ListenAndServe(dnsCtx, a.DNSAddr) }()

	nodeCtx, stopNode := context.WithCancel(context.Background())
	defer stopNode()
	if err := a.Node.Start(nodeCtx); err != nil {
		a.shutdown()
		return fmt.Errorf("tailscale: %w", err)
	}
	// the page's choices, as they were saved: an App Store update brings q2app's wifi=1 back
	if s := config.LoadSettings(a.Dir); s.Off {
		if err := a.Node.SetRunning(ctx, false); err != nil {
			a.logf("turn off: %v", err)
		}
	}
	if s := config.LoadSettings(a.Dir); config.AutoStart(a.Dir) == s.Manual {
		if err := config.SetAutoStart(a.Dir, !s.Manual); err != nil {
			a.logf("q2app: %v", err)
		}
	}
	every := a.Every
	if every == 0 {
		every = 3 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	a.Reconcile()
	for {
		select {
		case <-ctx.Done():
			stopDNS()
			stopNode()
			a.shutdown()
			return nil
		case err := <-dnsErr:
			a.logf("name server: %v", err) // the app runs on: names via the Wi-Fi's servers, IPs still work
			dnsErr = nil
		case <-a.Node.Updates():
			a.Reconcile()
		case <-tick.C:
			a.Reconcile()
		}
	}
}

func (a *App) shutdown() {
	a.release()
	a.Relay.Close()
	a.Loop.Close()
	a.Node.Close()
}

func writeWhole(path, text string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
