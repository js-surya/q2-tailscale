// Package node is the Q2's Tailscale node (tsnet, userspace networking: the Q2's kernel has no TUN) and
// the view of it the rest of the app uses.
package node

import (
	"context"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"tailscale.com/health"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"

	_ "tailscale.com/feature/taildrop" // receiving files: tsnet leaves it out

	"q2-tailscale/internal/taildrop"
)

// Peer is a machine on the tailnet: its MagicDNS name (the first label, lower case), its full name
// (no trailing dot) and its Tailscale IPv4.
type Peer struct {
	Name, FQDN string
	IP         netip.Addr
	Online     bool
}

// Status is the node as the pages and the reconciler see it. State is tsnet's backend state
// (NeedsLogin, Starting, Running, Stopped, …); AuthURL the sign-in link while NeedsLogin.
type Status struct {
	State, AuthURL, Self string
	SelfIP               netip.Addr
	Suffix               string
	Peers                []Peer // by name; machines without an IPv4 left out
	Services             []Peer // Tailscale Services (navidrome.<tailnet>.ts.net): names, not machines
	Warnings             []Warning
}

// Warning is one of Tailscale's health warnings, as its apps show it.
type Warning struct{ Title, Text string }

// quiet are warnings the page shows as its state already.
var quiet = map[health.WarnableCode]bool{"warming-up": true, "wantrunning-false": true}

// Warnings are Tailscale's health warnings of medium severity or more, the most severe first, then
// by title; nil when healthy.
func Warnings(st *health.State) []Warning {
	if st == nil {
		return nil
	}
	rank := map[health.Severity]int{health.SeverityHigh: 0, health.SeverityMedium: 1}
	var us []health.UnhealthyState
	for code, u := range st.Warnings {
		if _, ok := rank[u.Severity]; ok && !quiet[code] {
			us = append(us, u)
		}
	}
	slices.SortFunc(us, func(a, b health.UnhealthyState) int {
		if d := rank[a.Severity] - rank[b.Severity]; d != 0 {
			return d
		}
		return strings.Compare(a.Title, b.Title)
	})
	var out []Warning
	for _, u := range us {
		out = append(out, Warning{Title: u.Title, Text: u.Text})
	}
	return out
}

// cgnat is the range Tailscale gives out IPv4s in, machines' and Services' alike.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Services are the tailnet names in the netmap's extra DNS records that aren't machines: Tailscale
// Services, whose name resolves to a virtual IP that Tailscale routes to the machine hosting it. They
// come by name, Online (a Service has no state of its own), IPv4 in 100.64/10 only, by name.
func Services(recs []tailcfg.DNSRecord, peers []Peer) []Peer {
	var out []Peer
	for _, r := range recs {
		if r.Type != "" && r.Type != "A" {
			continue
		}
		ip, err := netip.ParseAddr(r.Value)
		if err != nil || !ip.Is4() || !cgnat.Contains(ip) {
			continue
		}
		fqdn := strings.ToLower(strings.TrimSuffix(r.Name, "."))
		name, _, _ := strings.Cut(fqdn, ".")
		if name == "" || slices.ContainsFunc(peers, func(p Peer) bool { return p.FQDN == fqdn || p.IP == ip }) ||
			slices.ContainsFunc(out, func(p Peer) bool { return p.FQDN == fqdn }) {
			continue
		}
		out = append(out, Peer{Name: name, FQDN: fqdn, IP: ip, Online: true})
	}
	slices.SortFunc(out, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Node is what the app needs of the Tailscale node.
type Node interface {
	Start(ctx context.Context) error
	Status() Status
	Updates() <-chan struct{} // a value after each change of Status
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
	Logout(ctx context.Context) error              // signs out, then asks for a new sign-in link
	SetRunning(ctx context.Context, on bool) error // connects or disconnects, signed in either way
	Close() error
}

func ipv4(ips []netip.Addr) netip.Addr {
	for _, ip := range ips {
		if ip.Is4() {
			return ip
		}
	}
	return netip.Addr{}
}

// FromIPN converts tsnet's status; nil is Starting.
func FromIPN(st *ipnstate.Status) Status {
	if st == nil {
		return Status{State: "Starting"}
	}
	out := Status{State: st.BackendState, AuthURL: st.AuthURL}
	if st.Self != nil {
		// its name on the tailnet, as MagicDNS has it (a rename in the admin console shows here),
		// else, before it has one, its own
		out.Self, _, _ = strings.Cut(strings.ToLower(strings.TrimSuffix(st.Self.DNSName, ".")), ".")
		if out.Self == "" {
			out.Self = st.Self.HostName
		}
		out.SelfIP = ipv4(st.Self.TailscaleIPs)
	}
	if st.CurrentTailnet != nil {
		out.Suffix = strings.ToLower(st.CurrentTailnet.MagicDNSSuffix)
	}
	for _, p := range st.Peer {
		ip := ipv4(p.TailscaleIPs)
		if !ip.IsValid() {
			continue
		}
		fqdn := strings.ToLower(strings.TrimSuffix(p.DNSName, "."))
		name, _, _ := strings.Cut(fqdn, ".")
		if name == "" {
			continue
		}
		out.Peers = append(out.Peers, Peer{Name: name, FQDN: fqdn, IP: ip, Online: p.Online})
	}
	slices.SortFunc(out.Peers, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// TSNet is the Node, on tsnet.
type TSNet struct {
	dir     string
	srv     *tsnet.Server
	updates chan struct{}
	mu      sync.Mutex
	st      Status
	health  []Warning // from the bus's health messages
}

// NewTSNet is a node keeping its identity in stateDir (on the card: the same device across restarts),
// named hostname, logging to logf.
func NewTSNet(stateDir, hostname string, keys ipn.StateStore, logf func(string, ...any)) *TSNet {
	return &TSNet{
		dir: stateDir,
		srv: &tsnet.Server{Dir: stateDir, Store: keys, Hostname: hostname, Ephemeral: false, Logf: logf,
			UserLogf: logf},
		updates: make(chan struct{}, 1),
		st:      Status{State: "Starting"},
	}
}

func (n *TSNet) Updates() <-chan struct{} { return n.updates }

func (n *TSNet) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.st
}

func (n *TSNet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return n.srv.Dial(ctx, network, addr)
}

// Start starts tsnet (which, without a saved login, asks for a sign-in link itself) and follows its
// state: each change refreshes Status and signals Updates.
func (n *TSNet) Start(ctx context.Context) error {
	if err := n.srv.Start(); err != nil {
		return err
	}
	lc, err := n.srv.LocalClient()
	if err != nil {
		return err
	}
	go func() {
		for ctx.Err() == nil {
			w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState|ipn.NotifyInitialHealthState)
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			for {
				msg, err := w.Next()
				if err != nil {
					break
				}
				if msg.Health != nil {
					n.mu.Lock()
					n.health = Warnings(msg.Health)
					n.mu.Unlock()
				}
				n.refresh(ctx)
			}
			w.Close()
		}
	}()
	go func() { // the peers' online state changes without a bus message: a slow poll keeps it fresh
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n.refresh(ctx)
			}
		}
	}()
	n.refresh(ctx)
	return nil
}

func (n *TSNet) refresh(ctx context.Context) {
	lc, err := n.srv.LocalClient()
	if err != nil {
		return
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := lc.Status(c)
	if err != nil {
		return
	}
	s := FromIPN(st)
	if s.State == "Running" {
		if dns, err := lc.DNSConfig(c); err == nil && dns != nil {
			s.Services = Services(dns.ExtraRecords, s.Peers)
		}
	}
	n.mu.Lock()
	s.Warnings = n.health
	n.st = s
	n.mu.Unlock()
	select {
	case n.updates <- struct{}{}:
	default:
	}
}

func (n *TSNet) Logout(ctx context.Context) error {
	lc, err := n.srv.LocalClient()
	if err != nil {
		return err
	}
	if err := lc.Logout(ctx); err != nil {
		return err
	}
	err = lc.StartLoginInteractive(ctx)
	n.refresh(ctx)
	return err
}

// SetRunning is tailscale up and down: WantRunning, the login kept.
func (n *TSNet) SetRunning(ctx context.Context, on bool) error {
	lc, err := n.srv.LocalClient()
	if err != nil {
		return err
	}
	_, err = lc.EditPrefs(ctx, &ipn.MaskedPrefs{Prefs: ipn.Prefs{WantRunning: on}, WantRunningSet: true})
	n.refresh(ctx)
	return err
}

func (n *TSNet) Close() error { return n.srv.Close() }

// Await waits up to d for whole Taildrop files (taildrop.Source).
func (n *TSNet) Await(ctx context.Context, d time.Duration) ([]taildrop.File, error) {
	lc, err := n.srv.LocalClient()
	if err != nil {
		return nil, err
	}
	ws, err := lc.AwaitWaitingFiles(ctx, d)
	if err != nil {
		return nil, err
	}
	out := make([]taildrop.File, len(ws))
	for i, w := range ws {
		out[i] = taildrop.File{Name: w.Name, Size: w.Size}
	}
	return out, nil
}

// Open reads a waiting Taildrop file.
func (n *TSNet) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	lc, err := n.srv.LocalClient()
	if err != nil {
		return nil, err
	}
	rc, _, err := lc.GetWaitingFile(ctx, name)
	return rc, err
}

// Delete lets Tailscale forget a waiting Taildrop file.
func (n *TSNet) Delete(ctx context.Context, name string) error {
	lc, err := n.srv.LocalClient()
	if err != nil {
		return err
	}
	return lc.DeleteWaitingFile(ctx, name)
}

// Staged is where Tailscale keeps the waiting file name: <state>/files/<login>-uid-<n>/name, "" when not
// exactly one matches.
func (n *TSNet) Staged(name string) string {
	m, _ := filepath.Glob(filepath.Join(n.dir, "files", "*", name))
	if len(m) != 1 || filepath.Base(m[0]) != name {
		return ""
	}
	return m[0]
}
