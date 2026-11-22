package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"q2-tailscale/internal/config"
	"q2-tailscale/internal/loopback"
	"q2-tailscale/internal/node"
	"q2-tailscale/internal/qr"
	"q2-tailscale/internal/relay"
)

type fakeNode struct {
	mu      sync.Mutex
	st      node.Status
	up      chan struct{}
	started bool
	closed  bool
	logouts int
	wants   []bool // each SetRunning
}

func newNode(st node.Status) *fakeNode { return &fakeNode{st: st, up: make(chan struct{}, 1)} }
func (n *fakeNode) Start(context.Context) error {
	n.mu.Lock()
	n.started = true
	n.mu.Unlock()
	return nil
}
func (n *fakeNode) Status() node.Status          { n.mu.Lock(); defer n.mu.Unlock(); return n.st }
func (n *fakeNode) Updates() <-chan struct{}     { return n.up }
func (n *fakeNode) Logout(context.Context) error { n.logouts++; return nil }
func (n *fakeNode) SetRunning(_ context.Context, on bool) error {
	n.mu.Lock()
	n.wants = append(n.wants, on)
	n.mu.Unlock()
	return nil
}
func (n *fakeNode) Close() error { n.mu.Lock(); n.closed = true; n.mu.Unlock(); return nil }
func (n *fakeNode) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return nil, net.ErrClosed
}
func (n *fakeNode) set(st node.Status) {
	n.mu.Lock()
	n.st = st
	n.mu.Unlock()
	n.up <- struct{}{}
}

type fakeRelay struct {
	mu      sync.Mutex
	targets []relay.Target
	errs    map[netip.AddrPort]error
	closed  bool
	hold    chan struct{} // when set, Sync waits for it: a reconcile that is slow (the Q2 under load)
	syncs   int
}

func (r *fakeRelay) Sync(t []relay.Target) map[netip.AddrPort]error {
	r.mu.Lock()
	hold := r.hold
	r.syncs++
	r.mu.Unlock()
	if hold != nil {
		<-hold
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets = slices.Clone(t)
	return r.errs
}
func (r *fakeRelay) Close() { r.mu.Lock(); r.closed = true; r.targets = nil; r.mu.Unlock() }
func (r *fakeRelay) got() []relay.Target {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.targets)
}

type fakeIP struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeIP) run(args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	return nil, nil
}
func (f *fakeIP) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

const udhcpc = "nameserver 192.168.1.1 # wlan0\n"

var (
	nas    = node.Peer{Name: "nas", FQDN: "nas.tail1234.ts.net", IP: netip.MustParseAddr("100.101.1.2"), Online: true}
	laptop = node.Peer{Name: "laptop", FQDN: "laptop.tail1234.ts.net", IP: netip.MustParseAddr("100.101.1.3")}
)

func running(peers ...node.Peer) node.Status {
	return node.Status{State: "Running", Self: "q2-pod", SelfIP: netip.MustParseAddr("100.64.0.5"), Suffix: "tail1234.ts.net", Peers: peers}
}

type world struct {
	app   *App
	node  *fakeNode
	relay *fakeRelay
	ip    *fakeIP
	dir   string
}

func newWorld(t *testing.T, st node.Status) *world {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "resolv.conf"), []byte(udhcpc), 0o644)
	w := &world{node: newNode(st), relay: &fakeRelay{}, ip: &fakeIP{}, dir: dir}
	w.app = &App{Node: w.node, Loop: &loopback.Manager{Run: w.ip.run}, Relay: w.relay, Dir: dir,
		ResolvPath: filepath.Join(dir, "resolv.conf"), StatePath: filepath.Join(dir, "q2-tailscale.state"), DNSAddr: "127.0.0.1:0"}
	return w
}

func (w *world) file(name string) string {
	b, _ := os.ReadFile(filepath.Join(w.dir, name))
	return string(b)
}

func TestNeedsLoginWritesQR(t *testing.T) {
	w := newWorld(t, node.Status{State: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/abc"})
	w.app.Reconcile()
	// Q2 Pod's QR page draws the code the app made (qr: its rows in hex, qrsize: its width) and shows addr
	// under it, for a phone that can't scan (q2-pod addon.c qr_refresh)
	c, _ := qr.Encode([]byte("https://login.tailscale.com/a/abc"))
	want := "addr=login.tailscale.com/a/abc\nqr=" + c.Hex() + "\nqrsize=" + strconv.Itoa(c.Size) +
		"\nnote=Scan to sign in to Tailscale\nbar=off\n"
	if got := w.file("q2-tailscale.state"); got != want {
		t.Fatalf("state %q\nwant  %q", got, want)
	}
	if w.ip.count("addr add") != 0 || len(w.relay.got()) != 0 {
		t.Fatalf("ip %v, targets %v", w.ip.calls, w.relay.got())
	}
	if got := w.file("resolv.conf"); got != "nameserver 127.0.0.1 # q2-tailscale\n"+udhcpc {
		t.Fatalf("resolv %q", got)
	}
}

func TestRunningReconciles(t *testing.T) {
	w := newWorld(t, running(nas, laptop))
	w.app.Reconcile()
	if w.ip.count("addr add 100.101.1.2/32") != 1 || w.ip.count("addr add 100.101.1.3/32") != 1 || w.ip.count("addr add") != 2 {
		t.Fatalf("ip %v", w.ip.calls)
	}
	targets := w.relay.got()
	if len(targets) != 12 {
		t.Fatalf("%d targets", len(targets))
	}
	for _, tg := range targets {
		if tg.Listen != tg.Dial || (tg.Listen.Addr() != nas.IP && tg.Listen.Addr() != laptop.IP) {
			t.Fatalf("target %v", tg)
		}
	}
	if !strings.Contains(w.file("resolv.conf"), "search tail1234.ts.net # q2-tailscale\n") {
		t.Fatalf("resolv %q", w.file("resolv.conf"))
	}
	if got := w.file("q2-tailscale.state"); got != "bar=on\n" {
		t.Fatalf("state %q", got)
	}
}

func TestExtraPortsFromSettings(t *testing.T) {
	w := newWorld(t, running(nas))
	os.WriteFile(filepath.Join(w.dir, "settings"), []byte("ports=8443\n"), 0o644)
	w.app.Reconcile()
	if n := len(w.relay.got()); n != 7 {
		t.Fatalf("%d targets", n)
	}
}

func TestPeerRemovedTearsDown(t *testing.T) {
	w := newWorld(t, running(nas, laptop))
	w.app.Reconcile()
	w.node.set(running(nas))
	w.app.Reconcile()
	if w.ip.count("addr del 100.101.1.3/32") != 1 || len(w.relay.got()) != 6 {
		t.Fatalf("ip %v, %d targets", w.ip.calls, len(w.relay.got()))
	}
}

func TestForwardToName(t *testing.T) {
	w := newWorld(t, running(nas))
	os.WriteFile(filepath.Join(w.dir, "forwards.txt"), []byte("9000 nas 4533\n9001 nas.tail1234.ts.net 80\n9002 100.101.1.2 443\n9003 gone 1\n"), 0o644)
	w.app.Reconcile()
	var fw []relay.Target
	for _, tg := range w.relay.got() {
		if tg.Listen.Addr() == netip.MustParseAddr("127.0.0.1") {
			fw = append(fw, tg)
		}
	}
	slices.SortFunc(fw, func(a, b relay.Target) int { return int(a.Listen.Port()) - int(b.Listen.Port()) })
	want := []relay.Target{
		{Listen: netip.MustParseAddrPort("127.0.0.1:9000"), Dial: netip.MustParseAddrPort("100.101.1.2:4533")},
		{Listen: netip.MustParseAddrPort("127.0.0.1:9001"), Dial: netip.MustParseAddrPort("100.101.1.2:80")},
		{Listen: netip.MustParseAddrPort("127.0.0.1:9002"), Dial: netip.MustParseAddrPort("100.101.1.2:443")},
	}
	if !slices.Equal(fw, want) {
		t.Fatalf("forwards %v", fw)
	}
	if w.app.ForwardProblem(9003) == "" {
		t.Fatal("the unknown machine's forward isn't reported")
	}
}

func TestBindErrorsReported(t *testing.T) {
	w := newWorld(t, running(nas))
	os.WriteFile(filepath.Join(w.dir, "forwards.txt"), []byte("9000 nas 4533\n"), 0o644)
	w.relay.errs = map[netip.AddrPort]error{netip.MustParseAddrPort("127.0.0.1:9000"): os.ErrExist}
	w.app.Reconcile()
	if w.app.ForwardProblem(9000) == "" {
		t.Fatal("bind error not reported")
	}
}

func TestLookup(t *testing.T) {
	w := newWorld(t, running(nas))
	w.app.Reconcile()
	if ip, ok := w.app.Lookup("nas.tail1234.ts.net"); !ok || ip != nas.IP {
		t.Fatalf("nas: %v %v", ip, ok)
	}
	if _, ok := w.app.Lookup("q2-pod.tail1234.ts.net"); ok {
		t.Fatal("our own name answered with an address nothing listens on")
	}
	if w.app.Suffix() != "tail1234.ts.net" {
		t.Fatal(w.app.Suffix())
	}
	w.node.set(node.Status{State: "NeedsLogin"})
	w.app.Reconcile()
	if w.app.Suffix() != "" {
		t.Fatal("signed out, still a suffix")
	}
}

func TestShutdownRestoresEverything(t *testing.T) {
	w := newWorld(t, running(nas, laptop))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.app.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for w.ip.count("addr add") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't end within 2 s of its cancel")
	}
	if got := w.file("resolv.conf"); got != udhcpc {
		t.Fatalf("resolv %q", got)
	}
	if w.ip.count("addr del") != 2 || !w.relay.closed || !w.node.closed {
		t.Fatalf("ip %v relay %v node %v", w.ip.calls, w.relay.closed, w.node.closed)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "q2-tailscale.state")); !os.IsNotExist(err) {
		t.Fatalf("state file left: %v", err)
	}
}

func TestStartCleansStaleLeftovers(t *testing.T) {
	w := newWorld(t, node.Status{State: "Starting"})
	os.WriteFile(w.app.ResolvPath, []byte("search old.ts.net # q2-tailscale\n"+udhcpc), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.app.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(w.file("resolv.conf"), "old.ts.net") || w.ip.count("-4 addr show dev lo") != 1 {
		t.Fatalf("resolv %q, ip %v", w.file("resolv.conf"), w.ip.calls)
	}
	cancel()
	<-done
}

func TestResolvReassertedAfterRewrite(t *testing.T) {
	w := newWorld(t, running(nas))
	w.app.Every = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.app.Run(ctx) }()
	defer func() { cancel(); <-done }() // Run done before t.TempDir is removed
	time.Sleep(150 * time.Millisecond)
	os.WriteFile(w.app.ResolvPath, []byte("nameserver 10.0.0.1 # wlan0\n"), 0o644) // a DHCP client writing the file whole
	time.Sleep(200 * time.Millisecond)
	if got := w.file("resolv.conf"); !strings.HasPrefix(got, "nameserver 127.0.0.1 # q2-tailscale\n") || !strings.Contains(got, "10.0.0.1") {
		t.Fatalf("resolv %q", got)
	}
}

// A Tailscale Service (navidrome.<tailnet>.ts.net) is answered, listened on and forwarded to like a machine.
func TestServiceLikeAMachine(t *testing.T) {
	navi := node.Peer{Name: "navidrome", FQDN: "navidrome.tail1234.ts.net", IP: netip.MustParseAddr("100.109.4.217"), Online: true}
	st := running(nas)
	st.Services = []node.Peer{navi}
	w := newWorld(t, st)
	os.WriteFile(filepath.Join(w.dir, "forwards.txt"), []byte("9000 navidrome 4533\n"), 0o644)
	w.app.Reconcile()
	if ip, ok := w.app.Lookup("navidrome.tail1234.ts.net"); !ok || ip != navi.IP {
		t.Fatalf("lookup navidrome: %v %v", ip, ok)
	}
	https := netip.AddrPortFrom(navi.IP, 443)
	fwd := relay.Target{Listen: netip.MustParseAddrPort("127.0.0.1:9000"), Dial: netip.AddrPortFrom(navi.IP, 4533)}
	got := w.relay.got()
	if !slices.Contains(got, relay.Target{Listen: https, Dial: https}) || !slices.Contains(got, fwd) {
		t.Fatalf("targets %v", got)
	}
	if w.ip.count("addr add 100.109.4.217/32") != 1 {
		t.Fatalf("ip calls %v", w.ip.calls)
	}
	if p := w.app.ForwardProblem(9000); p != "" {
		t.Fatalf("forward problem %q", p)
	}
}

// A stop that comes in the middle of a slow reconcile still gives resolv.conf back and drops the status
// file at once: the launcher KILLs an app that hasn't ended 2 s after its TERM, and a KILLed app's
// name server line would leave the Q2 asking a name server that isn't there.
func TestStopMidReconcileRestoresResolvAtOnce(t *testing.T) {
	w := newWorld(t, running(nas))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.app.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(w.file("resolv.conf"), "127.0.0.1") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	hold := make(chan struct{})
	w.relay.mu.Lock()
	w.relay.hold, w.relay.syncs = hold, 0
	w.relay.mu.Unlock()
	w.node.set(running(nas, laptop)) // the next reconcile, which now hangs in the relay
	for time.Now().Before(deadline) {
		w.relay.mu.Lock()
		n := w.relay.syncs
		w.relay.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	deadline = time.Now().Add(500 * time.Millisecond)
	for w.file("resolv.conf") != udhcpc && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := w.file("resolv.conf"); got != udhcpc {
		close(hold)
		t.Fatalf("resolv.conf still ours while the reconcile hangs: %q", got)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "q2-tailscale.state")); !os.IsNotExist(err) {
		close(hold)
		t.Fatalf("state file left: %v", err)
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := w.file("resolv.conf"); got != udhcpc { // the hung reconcile, ending, doesn't put it back
		t.Fatalf("resolv.conf after the reconcile ended: %q", got)
	}
}

// A listener that can't be opened is logged when it starts failing, not again at every reconcile: the
// log is on the card, and 29 Services × 6 ports at every 3 s check filled it and slowed the app down.
func TestListenErrorLoggedOnce(t *testing.T) {
	w := newWorld(t, running(nas))
	var mu sync.Mutex
	var lines []string
	w.app.Logf = func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	bad := netip.AddrPortFrom(nas.IP, 443)
	w.relay.errs = map[netip.AddrPort]error{bad: errors.New("bind: cannot assign requested address")}
	w.app.Reconcile()
	w.app.Reconcile()
	count := func() (n int) {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range lines {
			if strings.Contains(l, bad.String()) {
				n++
			}
		}
		return
	}
	if n := count(); n != 1 {
		t.Fatalf("logged %d times: %q", n, lines)
	}
	w.relay.errs = nil // it works now; then fails again: that is news
	w.app.Reconcile()
	w.relay.errs = map[netip.AddrPort]error{bad: errors.New("bind: cannot assign requested address")}
	w.app.Reconcile()
	if n := count(); n != 2 {
		t.Fatalf("logged %d times after failing again: %q", n, lines)
	}
}

// Turned off on the page, Tailscale stays signed in but offers nothing: no listeners, no addresses,
// resolv.conf as the Wi-Fi left it and no status bar icon.
func TestOffOffersNothing(t *testing.T) {
	w := newWorld(t, running(nas))
	w.app.Reconcile()
	config.SaveSettings(w.dir, config.Settings{Hostname: "q2-pod", Off: true})
	w.node.set(node.Status{State: "Stopped", Self: "q2-pod", Suffix: "tail1234.ts.net"})
	<-w.node.up
	w.app.Reconcile()
	if got := w.file("resolv.conf"); got != udhcpc {
		t.Fatalf("resolv %q", got)
	}
	if got := w.file("q2-tailscale.state"); got != "" {
		t.Fatalf("state %q: an icon for a Tailscale turned off", got)
	}
	if len(w.relay.got()) != 0 || w.ip.count("addr del") != 1 {
		t.Fatalf("targets %v, ip %v", w.relay.got(), w.ip.calls)
	}
}

// SetOn saves the choice and tells the node; the reconcile follows the node's change.
func TestSetOnSavesAndTellsTheNode(t *testing.T) {
	w := newWorld(t, running(nas))
	if err := w.app.SetOn(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !config.LoadSettings(w.dir).Off || !slices.Equal(w.node.wants, []bool{false}) {
		t.Fatalf("settings %+v wants %v", config.LoadSettings(w.dir), w.node.wants)
	}
	w.app.SetOn(context.Background(), true)
	if config.LoadSettings(w.dir).Off || !slices.Equal(w.node.wants, []bool{false, true}) {
		t.Fatalf("settings %+v wants %v", config.LoadSettings(w.dir), w.node.wants)
	}
}

// At its start the app puts its saved choices back: Tailscale off stays off, and a Start with Wi-Fi
// turned off stays off though an App Store update brought q2app's wifi=1 back.
func TestStartKeepsSavedChoices(t *testing.T) {
	w := newWorld(t, node.Status{State: "Starting"})
	config.SaveSettings(w.dir, config.Settings{Hostname: "q2-pod", Off: true, Manual: true})
	os.WriteFile(filepath.Join(w.dir, "q2app"), []byte("name=Tailscale\nwifi=1\nrow=network\n"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.app.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.node.mu.Lock()
		n := len(w.node.wants)
		w.node.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if !slices.Equal(w.node.wants, []bool{false}) || config.AutoStart(w.dir) {
		t.Fatalf("wants %v, autostart %v", w.node.wants, config.AutoStart(w.dir))
	}
}

// A login the Q2's internal storage couldn't take, kept on the SD card, is a warning while connected: the
// red-marked icon and a row saying why.
func TestKeysLeftOnTheCardAreAWarning(t *testing.T) {
	w := newWorld(t, running(nas))
	w.app.KeysOnCard = errors.New("internal storage: no space left on device")
	w.app.Reconcile()
	if got := w.file("q2-tailscale.state"); got != "bar=warn\n" {
		t.Fatalf("state %q", got)
	}
	ws := w.app.Warnings()
	if len(ws) != 1 || ws[0].Title != "Login kept on the SD card" || !strings.Contains(ws[0].Text, "no space left on device") {
		t.Fatalf("%+v", ws)
	}
}

// A health warning while connected, or the Wi-Fi's name servers not answering, shows the icon with its
// red mark (bar=warn) and the page's warning rows; a lookup that works again clears the DNS one.
func TestWarningsMarkTheIcon(t *testing.T) {
	st := running(nas)
	st.Warnings = []node.Warning{{Title: "Relay server unavailable", Text: "Tailscale could not connect to any relay server."}}
	w := newWorld(t, st)
	w.app.Reconcile()
	if got := w.file("q2-tailscale.state"); got != "bar=warn\n" {
		t.Fatalf("state %q", got)
	}
	if ws := w.app.Warnings(); len(ws) != 1 || ws[0].Title != "Relay server unavailable" {
		t.Fatalf("%+v", ws)
	}

	w2 := newWorld(t, running(nas))
	w2.app.Forwarded(false)
	w2.app.Reconcile()
	if got := w2.file("q2-tailscale.state"); got != "bar=warn\n" {
		t.Fatalf("state %q", got)
	}
	if ws := w2.app.Warnings(); len(ws) != 1 || ws[0].Title != "DNS unavailable" {
		t.Fatalf("%+v", ws)
	}
	w2.app.Forwarded(true)
	w2.app.Reconcile()
	if got := w2.file("q2-tailscale.state"); got != "bar=on\n" || w2.app.Warnings() != nil {
		t.Fatalf("state %q, %+v", got, w2.app.Warnings())
	}
}

// Signed in from the QR page, the page stays as it was for a minute with its note saying so (Q2 Pod's
// page reads a status file without a code as no Wi-Fi), as q2-subsonic's does; then the code goes.
func TestSignedInKeepsTheCodeAMinute(t *testing.T) {
	now := time.Unix(1000, 0)
	w := newWorld(t, node.Status{State: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/abc"})
	w.app.Now = func() time.Time { return now }
	w.app.Reconcile()
	w.node.set(running(nas))
	<-w.node.up
	w.app.Reconcile()
	c, _ := qr.Encode([]byte("https://login.tailscale.com/a/abc"))
	want := "addr=login.tailscale.com/a/abc\nqr=" + c.Hex() + "\nqrsize=" + strconv.Itoa(c.Size) +
		"\nnote=Signed in to Tailscale · press Return\nbar=on\n"
	if got := w.file("q2-tailscale.state"); got != want {
		t.Fatalf("state %q", got)
	}
	now = now.Add(61 * time.Second)
	w.app.Reconcile()
	if got := w.file("q2-tailscale.state"); got != "bar=on\n" {
		t.Fatalf("after a minute: %q", got)
	}
}

func TestReceivedNoteFiveMinutes(t *testing.T) {
	now := time.Unix(1e9, 0)
	a := &App{Now: func() time.Time { return now }}
	if a.Received() != "" {
		t.Fatal("note before anything came")
	}
	a.Got([]string{"song.flac"})
	if got := a.Received(); got != "Received song.flac" {
		t.Fatalf("%q", got)
	}
	a.Got([]string{"a.flac", "b.flac"})
	if got := a.Received(); got != "Received 2 files" {
		t.Fatalf("%q", got)
	}
	now = now.Add(5*time.Minute + time.Second)
	if a.Received() != "" {
		t.Fatal("note kept past 5 minutes")
	}
}

// A tailnet that requires logs, with them off, stops the node: the page says why and what to turn on,
// though Tailscale isn't running.
func TestATailnetThatRequiresLogsSaysSo(t *testing.T) {
	w := newWorld(t, node.Status{State: "Stopped", Self: "q2-pod", LogsRequired: true})
	ws := w.app.Warnings()
	if len(ws) != 1 || ws[0].Title != "This tailnet requires logs" || !strings.Contains(ws[0].Text, "Send Logs to Tailscale") {
		t.Fatalf("%+v", ws)
	}
}

// With logs turned on, a start turns Tailscale on again: a tailnet that required them had stopped it.
func TestStartWithLogsOnTurnsTailscaleOn(t *testing.T) {
	w := newWorld(t, node.Status{State: "Stopped"})
	config.SaveSettings(w.dir, config.Settings{Hostname: "q2-pod", Logs: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.app.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.node.mu.Lock()
		n := len(w.node.wants)
		w.node.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if !slices.Equal(w.node.wants, []bool{true}) {
		t.Fatalf("wants %v", w.node.wants)
	}
}

// The status file is written when what it says changes, not on every check-up: a check-up that finds the
// same leaves it alone.
func TestStatusFileWrittenOnlyWhenItChanges(t *testing.T) {
	w := newWorld(t, running(nas))
	w.app.Reconcile()
	path := filepath.Join(w.dir, "q2-tailscale.state")
	old := time.Unix(1e9, 0)
	os.Chtimes(path, old, old)
	w.app.Reconcile()
	if fi, err := os.Stat(path); err != nil || !fi.ModTime().Equal(old) {
		t.Fatalf("rewritten unchanged: %v %v", fi.ModTime(), err)
	}
	w.node.set(node.Status{State: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/abc"})
	<-w.node.up
	w.app.Reconcile()
	if fi, _ := os.Stat(path); fi.ModTime().Equal(old) {
		t.Fatal("a change not written")
	}
}
