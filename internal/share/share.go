// Package share puts the pages of the Q2's running card apps on the tailnet (Settings: Q2 Apps on
// Tailscale, off unless chosen), so a phone or computer away from the Q2's Wi-Fi reaches them, such as
// EQ Studio or File Browser. An app with a page writes its address in its status file for the QR page
// (/tmp/q2-<id>.state: addr=<ip>:<port>); while it runs, its port is passed on to it, plain or over
// HTTPS, and taken back when it stops. Only the owner's own devices get through (Allow), and each app
// still asks for its own key or PIN.
package share

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"q2-tailscale/internal/forward"
)

// App is a running card app with a page.
type App struct {
	ID, Name string
	Port     uint16
}

// own are ports this app serves itself (its name server) or passes on otherwise (q2remote's).
var own = []uint16{53, 2302}

// Running lists the card apps running now (their launcher's pid file in tmp names a live process)
// whose status file gives their page's port, by id; tailscale itself left out, and a port only once.
func Running(tmp, card string, alive func(pid int) bool) []App {
	states, _ := filepath.Glob(filepath.Join(tmp, "q2-*.state"))
	slices.Sort(states)
	var out []App
	for _, path := range states {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "q2-"), ".state")
		if id == "" || id == "tailscale" || strings.ContainsAny(id, "./") {
			continue
		}
		port := pagePort(path)
		if port == 0 || slices.Contains(own, port) || slices.ContainsFunc(out, func(a App) bool { return a.Port == port }) {
			continue
		}
		pid, err := readPid(filepath.Join(tmp, "q2-"+id))
		if err != nil || !alive(pid) {
			continue
		}
		out = append(out, App{ID: id, Name: appName(card, id), Port: port})
	}
	return out
}

// pagePort is the status file's addr= port, or 0.
func pagePort(path string) uint16 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, _ := strings.Cut(strings.TrimRight(s.Text(), "\r"), "=")
		if k != "addr" {
			continue
		}
		_, p, err := net.SplitHostPort(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return 0
		}
		return uint16(n)
	}
	return 0
}

func readPid(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// appName is the name its q2app shows, else its id.
func appName(card, id string) string {
	b, _ := os.ReadFile(filepath.Join(card, "."+id, "q2app"))
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok && strings.TrimSpace(k) == "name" {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return id
}

// Alive is whether pid is a process on this system.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// Manager passes each app Scan finds on, from the tailnet's same port (Listen) to Target:port, every
// Every; an app gone is taken back. A port that won't open is tried again once the apps change.
type Manager struct {
	Listen func(network, addr string) (net.Listener, error)
	Allow  func(context.Context, net.Conn) bool
	Scan   func() []App
	Target string // the Q2 itself: 127.0.0.1
	Every  time.Duration
	Logf   func(string, ...any)

	mu     sync.Mutex
	live   map[uint16]*running
	failed map[uint16]bool
}

type running struct {
	app  App
	stop context.CancelFunc
}

// Shared is what's passed on now, by name.
func (m *Manager) Shared() []App {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]App, 0, len(m.live))
	for _, r := range m.live {
		out = append(out, r.app)
	}
	slices.SortFunc(out, func(a, b App) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

// Run follows the apps until ctx ends, then takes every port back.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.live, m.failed = map[uint16]*running{}, map[uint16]bool{}
	m.mu.Unlock()
	t := time.NewTicker(m.Every)
	defer t.Stop()
	for {
		m.sync(ctx, m.Scan())
		select {
		case <-ctx.Done():
			m.sync(ctx, nil)
			return
		case <-t.C:
		}
	}
}

func (m *Manager) logf(format string, a ...any) {
	if m.Logf != nil {
		m.Logf(format, a...)
	}
}

func (m *Manager) sync(ctx context.Context, apps []App) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[uint16]App{}
	for _, a := range apps {
		want[a.Port] = a
	}
	for port, r := range m.live {
		if a, ok := want[port]; !ok || a.ID != r.app.ID {
			r.stop()
			delete(m.live, port)
			m.logf("app pages: %s's port %d taken back", r.app.Name, port)
		}
	}
	for port := range m.failed {
		if _, ok := want[port]; !ok {
			delete(m.failed, port) // gone: try again when it's back
		}
	}
	if ctx.Err() != nil {
		return
	}
	for port, a := range want {
		if m.live[port] != nil || m.failed[port] {
			continue
		}
		p := strconv.Itoa(int(port))
		l, err := m.Listen("tcp", ":"+p)
		if err != nil {
			m.failed[port] = true
			m.logf("app pages: %s on port %d: %v", a.Name, port, err)
			continue
		}
		cctx, stop := context.WithCancel(ctx)
		m.live[port] = &running{app: a, stop: stop}
		f := &forward.Forwarder{Listen: func(string, string) (net.Listener, error) { return l, nil }, Port: p,
			Target: net.JoinHostPort(m.Target, p), Allow: m.Allow, Name: "app pages: " + a.Name, Logf: m.Logf}
		go f.Run(cctx)
		m.logf("app pages: %s on port %d", a.Name, port)
	}
}
