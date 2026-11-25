package pages

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"q2-tailscale/internal/config"
	"q2-tailscale/internal/node"
	"q2-tailscale/internal/q2spot"
)

type fakeApp struct {
	st       node.Status
	problems map[uint16]string
	warnings []node.Warning
	received string
}

func (f *fakeApp) Received() string { return f.received }

func (f *fakeApp) Warnings() []node.Warning { return f.warnings }

func (f *fakeApp) Status() node.Status            { return f.st }
func (f *fakeApp) ForwardProblem(p uint16) string { return f.problems[p] }

var (
	nas    = node.Peer{Name: "nas", FQDN: "nas.tail1234.ts.net", IP: netip.MustParseAddr("100.101.1.2"), Online: true}
	laptop = node.Peer{Name: "laptop", FQDN: "laptop.tail1234.ts.net", IP: netip.MustParseAddr("100.101.1.3")}
)

type world struct {
	h       q2spot.Handler
	app     *fakeApp
	dir     string
	stopped atomic.Bool
	logouts int
	renews  int
	paths   map[netip.Addr]string // how each machine is reached, nil: not known
	now     time.Time
	taken   map[uint16]bool
	ons     []bool // each SetOn
	caps    string // the firmware's /tmp/q2-addon.caps
}

func newWorld(t *testing.T, st node.Status) *world {
	dir := filepath.Join(t.TempDir(), ".tailscale") // the app's folder on a card
	os.Mkdir(dir, 0o755)
	w := &world{app: &fakeApp{st: st, problems: map[uint16]string{}}, dir: dir, now: time.Unix(1e9, 0), taken: map[uint16]bool{}}
	w.h = Handler(Deps{App: w.app, Dir: w.dir,
		Logout:   func(context.Context) error { w.logouts++; return nil },
		Reauth:   func(context.Context) error { w.renews++; return nil },
		Paths:    func() map[netip.Addr]string { return w.paths },
		Stop:     func() { w.stopped.Store(true) },
		PortFree: func(p uint16) bool { return !w.taken[p] },
		Now:      func() time.Time { return w.now },
		Caps:     func() string { return w.caps },
		Version:  "q2-v0.1",
		SetOn: func(_ context.Context, on bool) error {
			w.ons = append(w.ons, on)
			s := config.LoadSettings(w.dir)
			s.Off = !on
			return config.SaveSettings(w.dir, s)
		}})
	return w
}

func running(peers ...node.Peer) node.Status {
	return node.Status{State: "Running", Self: "q2-pod", SelfIP: netip.MustParseAddr("100.64.0.5"), Suffix: "tail1234.ts.net", Peers: peers}
}

func titles(r q2spot.Reply) []string {
	var out []string
	for _, row := range r.Rows {
		out = append(out, row.Kind+":"+row.Title)
	}
	return out
}

func TestMenuSignedOut(t *testing.T) {
	w := newWorld(t, node.Status{State: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/x"})
	r := w.h("menu", nil)
	want := []string{"action:Tailscale", "info:Signed out", "remote:Sign In from Your Phone", "list:Machines", "list:Forwards", "list:Settings"}
	if r.Status != "ok" || strings.Join(titles(r), "|") != strings.Join(want, "|") || r.Message != "refresh" {
		t.Fatalf("%+v", r)
	}
}

// The menu is asked again every 2 s while it is on screen, connected too: a warning that comes while
// it is open shows at once (found on a real Q2: the DNS warning waited for the page to be reopened).
func TestMenuRunningRefreshes(t *testing.T) {
	w := newWorld(t, running(nas))
	r := w.h("menu", nil)
	if r.Rows[1].Title != "Connected" || r.Rows[1].Subtitle != "q2-pod · 100.64.0.5" || r.Message != "refresh" {
		t.Fatalf("%+v", r)
	}
	for _, row := range r.Rows {
		if row.Kind == "remote" {
			t.Fatal("a sign-in row while connected")
		}
	}
	if s := newWorld(t, node.Status{State: "Starting"}).h("menu", nil); s.Rows[1].Title != "Connecting…" || s.Message != "refresh" {
		t.Fatalf("%+v", s)
	}
}

func TestMachinesSorted(t *testing.T) {
	w := newWorld(t, running(laptop, nas)) // as the node gives them
	r := w.h("machines", nil)
	if strings.Join(titles(r), "|") != "info:laptop|info:nas" || r.Rows[0].Subtitle != "100.101.1.3 · offline" || r.Rows[1].Subtitle != "100.101.1.2 · online" {
		t.Fatalf("%+v", r.Rows)
	}
	if e := newWorld(t, running()).h("machines", nil); len(e.Rows) != 1 || e.Rows[0].Title != "No machines" {
		t.Fatalf("%+v", e.Rows)
	}
}

func TestAddForwardFlow(t *testing.T) {
	w := newWorld(t, running(nas))
	if r := w.h("addfwd", nil); len(r.Rows) != 1 || r.Rows[0].Kind != "list" || r.Rows[0].URI != "addfwd2" || r.Rows[0].Context != nas.FQDN {
		t.Fatalf("%+v", r.Rows)
	}
	r := w.h("addfwd2", []string{nas.FQDN})
	if len(r.Rows) != len(config.DefaultPorts)+1 || r.Rows[2].URI != "fwadd" || r.Rows[2].Context != nas.FQDN+" 4533" || r.Rows[len(r.Rows)-1].Kind != "field" {
		t.Fatalf("%+v", r.Rows)
	}
	if r := w.h("fwadd", []string{nas.FQDN + " 4533"}); r.Status != "ok" || r.Message != "Forward added · 127.0.0.1:4533" {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(w.dir, "forwards.txt")); !strings.Contains(string(b), "\n4533 nas.tail1234.ts.net 4533\n") {
		t.Fatalf("forwards.txt %q", b)
	}
	f := w.h("forwards", nil)
	if f.Rows[0].Kind != "action" || f.Rows[0].URI != "fwdel" || f.Rows[0].Title != "127.0.0.1:4533" || f.Rows[0].Subtitle != "→ nas.tail1234.ts.net:4533" {
		t.Fatalf("%+v", f.Rows)
	}
	if f.Rows[len(f.Rows)-1].Title != "Add Forward" {
		t.Fatalf("%+v", f.Rows)
	}
}

func TestAddForwardPortTaken(t *testing.T) {
	w := newWorld(t, running(nas))
	w.taken[4533] = true
	if r := w.h("fwadd", []string{nas.FQDN + " 4533"}); r.Message != "Forward added · 127.0.0.1:10000" {
		t.Fatalf("%+v", r)
	}
	w.h("fwadd", []string{nas.FQDN + " 4533"}) // the same again: 10000 is now ours, so 10001
	fs, _ := config.LoadForwards(w.dir)
	if len(fs) != 2 || fs[1].LocalPort != 10001 {
		t.Fatalf("%+v", fs)
	}
}

func TestOtherPortField(t *testing.T) {
	w := newWorld(t, running(nas))
	if r := w.h("fwaddport", []string{nas.FQDN, "8443"}); r.Message != "Forward added · 127.0.0.1:8443" {
		t.Fatalf("%+v", r)
	}
	if r := w.h("fwaddport", []string{nas.FQDN, "99999"}); r.Status != "error" {
		t.Fatalf("%+v", r)
	}
}

func TestForwardProblemShown(t *testing.T) {
	w := newWorld(t, running(nas))
	config.SaveForwards(w.dir, []config.Forward{{LocalPort: 9000, Host: "gone", Port: 1}})
	w.app.problems[9000] = "gone isn't on the tailnet"
	if r := w.h("forwards", nil); !strings.HasSuffix(r.Rows[0].Subtitle, " · gone isn't on the tailnet") {
		t.Fatalf("%+v", r.Rows)
	}
}

func TestRemoveForward(t *testing.T) {
	w := newWorld(t, running(nas))
	config.SaveForwards(w.dir, []config.Forward{{LocalPort: 9000, Host: "nas", Port: 4533}, {LocalPort: 9001, Host: "nas", Port: 80}})
	if r := w.h("fwdel", []string{"9000"}); r.Message != "Forward removed" {
		t.Fatalf("%+v", r)
	}
	if fs, _ := config.LoadForwards(w.dir); len(fs) != 1 || fs[0].LocalPort != 9001 {
		t.Fatalf("%+v", fs)
	}
}

func TestSettingsFields(t *testing.T) {
	w := newWorld(t, running(nas))
	r := w.h("settings", nil)
	if strings.Join(titles(r), "|") != "action:Start with Wi-Fi|action:Send Logs to Tailscale|list:Save To|field:Extra Ports:|field:Device Name:|action:Q2 Apps on Tailscale|list:About|action:Sign Out" ||
		r.Rows[3].Subtitle != "None" || r.Rows[4].Subtitle != "q2-pod" {
		t.Fatalf("%+v", r.Rows)
	}
	if r := w.h("setports", []string{"8443, 9000"}); r.Status != "ok" {
		t.Fatalf("%+v", r)
	}
	if r := w.h("setports", []string{"80,x"}); r.Status != "error" {
		t.Fatalf("%+v", r)
	}
	if r := w.h("sethost", []string{"my-q2"}); r.Message != "Restart Tailscale to apply" {
		t.Fatalf("%+v", r)
	}
	if r := w.h("sethost", []string{"Bad Name"}); r.Status != "error" {
		t.Fatalf("%+v", r)
	}
	if s := config.LoadSettings(w.dir); s.Hostname != "my-q2" || len(s.Ports) != 2 {
		t.Fatalf("%+v", s)
	}
}

func TestSignOutNeedsTwoPresses(t *testing.T) {
	w := newWorld(t, running(nas))
	if r := w.h("signout", nil); r.Message != "Press again to sign out" || w.logouts != 0 {
		t.Fatalf("%+v", r)
	}
	w.now = w.now.Add(11 * time.Second) // too late: starts over
	if w.h("signout", nil); w.logouts != 0 {
		t.Fatal("signed out after 11 s")
	}
	w.now = w.now.Add(5 * time.Second)
	if r := w.h("signout", nil); r.Message != "Signed out" || w.logouts != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestStopOp(t *testing.T) {
	w := newWorld(t, running())
	if r := w.h("stop", nil); r.Status != "ok" {
		t.Fatalf("%+v", r)
	}
	time.Sleep(50 * time.Millisecond)
	if !w.stopped.Load() {
		t.Fatal("not stopped")
	}
}

func TestUnknownOp(t *testing.T) {
	if r := newWorld(t, running()).h("nope", nil); r.Status != "unsupported" {
		t.Fatalf("%+v", r)
	}
}

// The menu's first row turns Tailscale on and off; off is still signed in, and says so.
func TestTailscaleSwitch(t *testing.T) {
	w := newWorld(t, running(nas))
	r := w.h("menu", nil)
	if r.Rows[0].Kind != "action" || r.Rows[0].URI != "power" || r.Rows[0].Subtitle != "On" {
		t.Fatalf("%+v", r.Rows[0])
	}
	if r := w.h("power", nil); r.Status != "ok" || r.Message != "Tailscale off" || !slices.Equal(w.ons, []bool{false}) {
		t.Fatalf("%+v %v", r, w.ons)
	}
	w.app.st = node.Status{State: "Stopped", Self: "q2-pod"}
	r = w.h("menu", nil)
	if r.Rows[0].Subtitle != "Off" || r.Rows[1].Title != "Off" || r.Rows[1].Subtitle != "Signed in · not connected" {
		t.Fatalf("%+v", r.Rows[:2])
	}
	if r := w.h("power", nil); r.Message != "Tailscale on" || !slices.Equal(w.ons, []bool{false, true}) {
		t.Fatalf("%+v %v", r, w.ons)
	}
}

// Start with Wi-Fi is q2app's wifi=1 and the settings' autostart, flipped by a press.
func TestStartWithWiFiSwitch(t *testing.T) {
	w := newWorld(t, running(nas))
	os.WriteFile(filepath.Join(w.dir, "q2app"), []byte("name=Tailscale\nwifi=1\nrow=network\n"), 0o644)
	if r := w.h("settings", nil); r.Rows[0].URI != "autostart" || r.Rows[0].Subtitle != "On" {
		t.Fatalf("%+v", r.Rows[0])
	}
	if r := w.h("autostart", nil); r.Message != "Starts when you open it" || config.AutoStart(w.dir) || !config.LoadSettings(w.dir).Manual {
		t.Fatalf("%+v", r)
	}
	if r := w.h("settings", nil); r.Rows[0].Subtitle != "Off" {
		t.Fatalf("%+v", r.Rows[0])
	}
	if r := w.h("autostart", nil); r.Message != "Starts with Wi-Fi" || !config.AutoStart(w.dir) || config.LoadSettings(w.dir).Manual {
		t.Fatalf("%+v", r)
	}
}

// With firmware that draws switches (caps: switch) the two on/off rows are switches, their state in
// the subtitle as "on" or "off".
func TestSwitchRowsWhenTheFirmwareHasThem(t *testing.T) {
	w := newWorld(t, running(nas))
	w.caps = "playlist field wifi network bar switch"
	os.WriteFile(filepath.Join(w.dir, "q2app"), []byte("name=Tailscale\nwifi=1\n"), 0o644)
	if r := w.h("menu", nil).Rows[0]; r.Kind != "switch" || r.URI != "power" || r.Title != "Tailscale" || r.Subtitle != "on" {
		t.Fatalf("%+v", r)
	}
	if r := w.h("settings", nil).Rows[0]; r.Kind != "switch" || r.URI != "autostart" || r.Subtitle != "on" {
		t.Fatalf("%+v", r)
	}
	w.h("autostart", nil)
	if r := w.h("settings", nil).Rows[0]; r.Subtitle != "off" {
		t.Fatalf("%+v", r)
	}
}

// Tailscale's warnings, as its Android app shows them, under the state.
func TestWarningRows(t *testing.T) {
	w := newWorld(t, running(nas))
	w.app.warnings = []node.Warning{{Title: "DNS unavailable", Text: "The Wi-Fi's name servers aren't answering"}}
	r := w.h("menu", nil)
	if r.Rows[2].Kind != "info" || r.Rows[2].Title != "Warning: DNS unavailable" || r.Rows[2].Subtitle != "The Wi-Fi's name servers aren't answering" {
		t.Fatalf("%+v", r.Rows)
	}
	if r.Message != "refresh" { // asked again, so the warning goes when it does
		t.Fatalf("message %q", r.Message)
	}
}

// About: the version, who made it and what it stands on, the trademark notice, and where the licences are.
func TestAbout(t *testing.T) {
	w := newWorld(t, running(nas))
	r := w.h("about", nil)
	if r.Status != "ok" {
		t.Fatalf("%+v", r)
	}
	if len(r.Rows) != 7 {
		t.Fatalf("%d rows", len(r.Rows))
	}
	text := ""
	for _, row := range r.Rows {
		if row.Kind != "info" {
			t.Fatalf("a row that does something: %+v", row)
		}
		text += row.Title + " | " + row.Subtitle + "\n"
	}
	for _, want := range []string{
		"Version | q2-v0.1",
		"Made by | Syntax-Error",
		"Thanks | DiamondBond & the Q2 Pod community",
		"Thanks — andsuor | QR code & launcher, from q2-subsonic",
		"Tailscale | © Tailscale Inc. · BSD-3-Clause",
		"Not an official Tailscale app | Tailscale is a trademark of Tailscale Inc.",
		"Licenses | .tailscale/LICENSES.txt",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
}

func TestMenuReceivedNote(t *testing.T) {
	w := newWorld(t, running(nas))
	for _, row := range w.h("menu", nil).Rows {
		if strings.HasPrefix(row.Title, "Received") {
			t.Fatal("a note with nothing received")
		}
	}
	w.app.received = "Received song.flac"
	r := w.h("menu", nil)
	if r.Rows[2].Kind != "info" || r.Rows[2].Title != "Received song.flac" || r.Rows[2].Subtitle != "Run Scan music now to see it in Library" {
		t.Fatalf("%+v", r.Rows)
	}
}

func TestSaveToRows(t *testing.T) {
	w := newWorld(t, running())
	card := config.CardRoot(w.dir)
	for _, d := range []string{"musics/Taildrop", "musics/Albums/Rock", "musics/Albums/Jazz", "Podcasts", ".hidden"} {
		os.MkdirAll(filepath.Join(card, d), 0o755)
	}
	os.WriteFile(filepath.Join(card, "song.flac"), nil, 0o644)
	if r := w.h("settings", nil).Rows[2]; r.URI != "saveto" || r.Subtitle != "musics/Taildrop" {
		t.Fatalf("%+v", r)
	}
	r := w.h("saveto", []string{""})    // the card itself: its folders only ("" is the default folder)
	want := "list:musics|list:Podcasts" // .tailscale (the app's own) and .hidden left out
	if got := strings.Join(titles(r), "|"); got != want {
		t.Fatalf("%s", got)
	}
	if r.Rows[0].Context != "musics" {
		t.Fatalf("%+v", r.Rows)
	}
	r = w.h("saveto", []string{"musics"})
	if got := strings.Join(titles(r), "|"); got != "action:Save here|list:Albums|list:Taildrop" || r.Rows[0].Subtitle != "musics" {
		t.Fatalf("%s %+v", got, r.Rows)
	}
	os.RemoveAll(filepath.Join(card, "musics/Taildrop"))
	if got := strings.Join(titles(w.h("saveto", []string{""})), "|"); got != want {
		t.Fatalf("%s", got)
	}
	if got := strings.Join(titles(w.h("saveto", []string{"musics"})), "|"); got != "action:Save here|list:Albums|list:Taildrop" {
		t.Fatalf("the default folder not offered once gone: %s", got)
	}
	r = w.h("saveto", []string{"musics/Taildrop"})
	if r.Rows[0].Title != "✓ Save here" || r.Rows[0].Subtitle != "musics/Taildrop" {
		t.Fatalf("the current choice unmarked: %+v", r.Rows[0])
	}
	r = w.h("saveto", []string{"musics/Albums"})
	if got := strings.Join(titles(r), "|"); got != "action:Save here|list:Jazz|list:Rock" || r.Rows[2].Context != "musics/Albums/Rock" {
		t.Fatalf("%s %+v", got, r.Rows)
	}
}

func TestSaveHere(t *testing.T) {
	w := newWorld(t, running())
	os.MkdirAll(filepath.Join(config.CardRoot(w.dir), "Podcasts/New"), 0o755)
	r := w.h("savehere", []string{"Podcasts/New"})
	if r.Status != "ok" || r.Message != "Files go to Podcasts/New" {
		t.Fatalf("%+v", r)
	}
	if s := config.LoadSettings(w.dir); s.Save != "Podcasts/New" {
		t.Fatalf("saved %q", s.Save)
	}
	if r := w.h("settings", nil).Rows[2]; r.Subtitle != "Podcasts/New" {
		t.Fatalf("%+v", r)
	}
	if r := w.h("savehere", []string{"Gone"}); r.Status != "error" {
		t.Fatalf("a missing folder saved: %+v", r)
	}
}

func TestSaveToRejectsDotDot(t *testing.T) {
	w := newWorld(t, running())
	for _, rel := range []string{"", "..", "../x", ".tailscale", ".hidden", "/etc"} {
		if rel == "" {
			if r := w.h("savehere", []string{rel}); r.Status != "error" {
				t.Errorf("savehere on the card itself: %+v", r)
			}
			continue
		}
		if r := w.h("saveto", []string{rel}); r.Status != "error" {
			t.Errorf("saveto %q: %+v", rel, r)
		}
		if r := w.h("savehere", []string{rel}); r.Status != "error" {
			t.Errorf("savehere %q: %+v", rel, r)
		}
	}
}

// Send Logs to Tailscale is a switch in Settings, off at first; a press saves the other way and restarts
// the app, as Tailscale's logs are set at its start.
func TestSendLogsSwitchSavesAndRestarts(t *testing.T) {
	w := newWorld(t, running())
	restarts := 0
	w.h = Handler(Deps{App: w.app, Dir: w.dir, Caps: func() string { return "switch" }, Restart: func() { restarts++ }})
	r := w.h("settings", nil)
	if len(r.Rows) < 2 || r.Rows[1].URI != "logs" || r.Rows[1].Title != "Send Logs to Tailscale" || r.Rows[1].Subtitle != "off" {
		t.Fatalf("%+v", r.Rows)
	}
	if r := w.h("logs", nil); !strings.Contains(r.Message, "Restarting") || !config.LoadSettings(w.dir).Logs {
		t.Fatalf("%+v %+v", r, config.LoadSettings(w.dir))
	}
	deadline := time.Now().Add(2 * time.Second)
	for restarts == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if restarts != 1 {
		t.Fatalf("restarts %d", restarts)
	}
	w.h("logs", nil)
	if config.LoadSettings(w.dir).Logs {
		t.Fatal("still on")
	}
}

// The developer row shows on a developer firmware only, and its switch saves and restarts.
func TestRemoteRowOnlyOnDeveloperFirmware(t *testing.T) {
	w := newWorld(t, running())
	restarts := 0
	has := func(r q2spot.Reply) bool {
		for _, row := range r.Rows {
			if row.URI == "remote" {
				return true
			}
		}
		return false
	}
	w.h = Handler(Deps{App: w.app, Dir: w.dir, Caps: func() string { return "switch" }, Dev: func() bool { return false }})
	if has(w.h("settings", nil)) {
		t.Fatal("shown on a release firmware")
	}
	if r := w.h("remote", nil); config.LoadSettings(w.dir).Remote || r.Message == "" {
		t.Fatal("switched on a release firmware")
	}
	w.h = Handler(Deps{App: w.app, Dir: w.dir, Caps: func() string { return "switch" }, Dev: func() bool { return true },
		Restart: func() { restarts++ }})
	if !has(w.h("settings", nil)) {
		t.Fatal("not shown on a developer firmware")
	}
	w.h("remote", nil)
	if !config.LoadSettings(w.dir).Remote {
		t.Fatal("not saved")
	}
	deadline := time.Now().Add(2 * time.Second)
	for restarts == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if restarts != 1 {
		t.Fatalf("restarts %d", restarts)
	}
}

// Q2 Apps on Tailscale is off unless chosen; on, it shows Use HTTPS and what's shared, with the address to
// open each at, and each switch saves and restarts.
func TestAppsOnTailscale(t *testing.T) {
	w := newWorld(t, running())
	restarts := 0
	shared := []SharedApp{{Name: "EQ Studio", Address: "https://q2-pod.tail1234.ts.net:8020"}}
	w.h = Handler(Deps{App: w.app, Dir: w.dir, Caps: func() string { return "switch" }, Restart: func() { restarts++ },
		Shared: func() ([]SharedApp, string) { return shared, "" }})
	rows := func() string { return strings.Join(titles(w.h("settings", nil)), "|") }
	if strings.Contains(rows(), "Use HTTPS") || !strings.Contains(rows(), "switch:Q2 Apps on Tailscale") {
		t.Fatal(rows())
	}
	w.h("apps", nil)
	if !config.LoadSettings(w.dir).Apps || !strings.Contains(rows(), "switch:Use HTTPS|list:Apps Shared Now") {
		t.Fatal(rows())
	}
	w.h("https", nil)
	if !config.LoadSettings(w.dir).HTTPS {
		t.Fatal("HTTPS not saved")
	}
	r := w.h("sharedapps", nil)
	if r.Rows[0].Title != "EQ Studio" || r.Rows[0].Subtitle != "https://q2-pod.tail1234.ts.net:8020" {
		t.Fatalf("%+v", r.Rows)
	}
	shared = nil
	if r := w.h("sharedapps", nil); !strings.HasPrefix(r.Rows[0].Title, "None running") {
		t.Fatalf("%+v", r.Rows)
	}
	for end := time.Now().Add(2 * time.Second); restarts < 2 && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
	}
	if restarts != 2 {
		t.Fatalf("restarts %d", restarts)
	}
}

// Expiring soon: Renew Sign-In on the menu asks for a new sign-in, the node kept; with its link, the
// menu offers the QR page, as for the first sign-in.
func TestRenewSignIn(t *testing.T) {
	st := running(nas)
	w := newWorld(t, st)
	st.KeyExpiry = w.now.Add(3 * 24 * time.Hour)
	w.app.st = st
	r := w.h("menu", nil)
	i := slices.IndexFunc(r.Rows, func(r q2spot.Row) bool { return r.URI == "renew" })
	if i < 0 || r.Rows[i].Kind != "action" || r.Rows[i].Title != "Renew Sign-In" {
		t.Fatalf("%+v", r.Rows)
	}
	if r := w.h("renew", nil); r.Status != "ok" || w.renews != 1 {
		t.Fatalf("%+v, renews %d", r, w.renews)
	}

	st.AuthURL = "https://login.tailscale.com/a/renew"
	w.app.st = st
	r = w.h("menu", nil)
	i = slices.IndexFunc(r.Rows, func(r q2spot.Row) bool { return r.URI == "signin" })
	if i < 0 || r.Rows[i].Kind != "remote" || r.Rows[i].Title != "Renew from Your Phone" {
		t.Fatalf("%+v", r.Rows)
	}
	if slices.ContainsFunc(r.Rows, func(r q2spot.Row) bool { return r.URI == "renew" }) {
		t.Fatal("Renew Sign-In shown with the code already up")
	}

	w2 := newWorld(t, running(nas)) // key expiry off
	if slices.ContainsFunc(w2.h("menu", nil).Rows, func(r q2spot.Row) bool { return r.URI == "renew" }) {
		t.Fatal("Renew Sign-In without an expiry")
	}
}

// Machines say how each online one is reached, read while the page is open, and a tip when one goes
// through Tailscale's relays; the page asks again so it follows a path that changes.
func TestMachinesShowThePath(t *testing.T) {
	tv := node.Peer{Name: "tv", FQDN: "tv.tail1234.ts.net", IP: netip.MustParseAddr("100.101.1.4"), Online: true}
	w := newWorld(t, running(nas, laptop, tv))
	w.paths = map[netip.Addr]string{nas.IP: "direct", tv.IP: "via relay (fra)", laptop.IP: "direct"}
	r := w.h("machines", nil)
	if r.Rows[0].Subtitle != "100.101.1.3 · offline" || r.Rows[1].Subtitle != "100.101.1.2 · online · direct" ||
		r.Rows[2].Subtitle != "100.101.1.4 · online · via relay (fra)" {
		t.Fatalf("%+v", r.Rows)
	}
	last := r.Rows[len(r.Rows)-1]
	if len(r.Rows) != 4 || last.Kind != "info" || !strings.HasPrefix(last.Title, "Relayed:") {
		t.Fatalf("tip: %+v", r.Rows)
	}
	if r.Message != "refresh" {
		t.Fatalf("message %q", r.Message)
	}

	w.paths = map[netip.Addr]string{nas.IP: "direct"}
	if r := w.h("machines", nil); len(r.Rows) != 3 {
		t.Fatalf("no relay, no tip: %+v", r.Rows)
	}
}
