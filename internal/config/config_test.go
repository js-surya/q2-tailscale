package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	dir := t.TempDir()
	s := LoadSettings(dir)
	if s.Hostname != "q2-pod" || len(s.Ports) != 0 {
		t.Fatalf("settings %+v", s)
	}
	if got := AllPorts(s); !reflect.DeepEqual(got, []uint16{80, 443, 4533, 8096, 8920, 32400}) {
		t.Fatalf("ports %v", got)
	}
	f, errs := LoadForwards(dir)
	if len(f) != 0 || len(errs) != 0 {
		t.Fatalf("forwards %v %v", f, errs)
	}
}

func TestParseForwards(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "forwards.txt"), []byte("9000 nas 4533\r\n# a comment\n\nx nas 1\n9001 100.101.1.2 8096\n9002 nas 0\n"), 0o644)
	f, errs := LoadForwards(dir)
	want := []Forward{{9000, "nas", 4533}, {9001, "100.101.1.2", 8096}}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("forwards %v", f)
	}
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), "line 4") || !strings.Contains(errs[1].Error(), "line 6") {
		t.Fatalf("errs %v", errs)
	}
}

func TestAllPortsMerge(t *testing.T) {
	got := AllPorts(Settings{Ports: []uint16{8443, 443, 8443}})
	if !reflect.DeepEqual(got, []uint16{80, 443, 4533, 8096, 8443, 8920, 32400}) {
		t.Fatalf("got %v", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := Settings{Hostname: "my-q2", Ports: []uint16{8443, 9000}, Off: true, Manual: true}
	if err := SaveSettings(dir, s); err != nil {
		t.Fatal(err)
	}
	if got := LoadSettings(dir); !reflect.DeepEqual(got, s) {
		t.Fatalf("settings %+v", got)
	}
	f := []Forward{{9000, "nas.tail1234.ts.net", 4533}}
	if err := SaveForwards(dir, f); err != nil {
		t.Fatal(err)
	}
	if got, errs := LoadForwards(dir); !reflect.DeepEqual(got, f) || len(errs) != 0 {
		t.Fatalf("forwards %v %v", got, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "forwards.txt")); !strings.HasPrefix(string(b), "#") {
		t.Fatalf("no header: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestParsePorts(t *testing.T) {
	got, err := ParsePorts(" 8443, 9000 ,8443")
	if err != nil || !reflect.DeepEqual(got, []uint16{8443, 9000}) {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"0", "70000", "80,x", "-1"} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if got, err := ParsePorts(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}

func TestBadSettingsFallBack(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "settings"), []byte("hostname=Bad Name!\nports=443,x\n"), 0o644)
	if s := LoadSettings(dir); s.Hostname != "q2-pod" || len(s.Ports) != 0 {
		t.Fatalf("settings %+v", s)
	}
}

func TestValidHostname(t *testing.T) {
	for _, ok := range []string{"q2-pod", "a", "my-q2-2"} {
		if !ValidHostname(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-q2", "q2-", "Q2", "q2 pod", strings.Repeat("a", 64)} {
		if ValidHostname(bad) {
			t.Errorf("%q taken", bad)
		}
	}
}

// Start with Wi-Fi is q2app's wifi=1, the line Q2 Pod reads: turned off, the line goes and every other
// line stays as it was; turned on, it comes back. The choice is kept in settings too (Manual), so an
// App Store update that brings q2app's wifi=1 back is undone at the app's next start.
func TestAutoStartEditsQ2app(t *testing.T) {
	dir := t.TempDir()
	q2app := filepath.Join(dir, "q2app")
	os.WriteFile(q2app, []byte("name=Tailscale\nqr=1\nwifi=1\r\nrow=network\n"), 0o644)
	if !AutoStart(dir) {
		t.Fatal("wifi=1 reads as off")
	}
	if err := SetAutoStart(dir, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(q2app); string(b) != "name=Tailscale\nqr=1\nrow=network\n" || AutoStart(dir) {
		t.Fatalf("off: %q", b)
	}
	if err := SetAutoStart(dir, false); err != nil { // already off: nothing to do
		t.Fatal(err)
	}
	if err := SetAutoStart(dir, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(q2app); string(b) != "name=Tailscale\nqr=1\nrow=network\nwifi=1\n" || !AutoStart(dir) {
		t.Fatalf("on: %q", b)
	}
	os.WriteFile(q2app, []byte("name=Tailscale\nwifi=0\n"), 0o644)
	if AutoStart(dir) {
		t.Fatal("wifi=0 reads as on")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestAutoStartWithoutQ2app(t *testing.T) {
	if err := SetAutoStart(t.TempDir(), false); err == nil {
		t.Fatal("no q2app: nothing to say the choice was kept")
	}
}

func TestOldSettingsAreOnAndAuto(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "settings"), []byte("hostname=my-q2\nports=\n"), 0o644)
	if s := LoadSettings(dir); s.Off || s.Manual {
		t.Fatalf("%+v", s)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSettings(dir, Settings{Hostname: "q2-pod", Save: "Albums/Rock"}); err != nil {
		t.Fatal(err)
	}
	if s := LoadSettings(dir); s.Save != "Albums/Rock" {
		t.Fatalf("save %q", s.Save)
	}
	SaveSettings(dir, Settings{Hostname: "q2-pod"})
	if b, _ := os.ReadFile(filepath.Join(dir, "settings")); strings.Contains(string(b), "save=") {
		t.Fatalf("default written: %q", b)
	}
}

func TestValidSave(t *testing.T) {
	for _, s := range []string{"", "A", "A/B", "Rock & Roll"} {
		if !ValidSave(s) {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"..", "../x", "/abs", ".hidden", "A/.x", "A/../..", "A//B", "A/"} {
		if ValidSave(s) {
			t.Errorf("%q taken", s)
		}
	}
}

func TestSaveDirFallback(t *testing.T) {
	card := t.TempDir()
	app := filepath.Join(card, ".tailscale")
	os.Mkdir(app, 0o755)
	if got := CardRoot(app); got != card {
		t.Fatalf("card root %q", got)
	}
	def := filepath.Join(card, "musics", "Taildrop")
	for _, save := range []string{"", "Gone", "../x", ".tailscale"} {
		got, err := SaveDir(app, Settings{Save: save})
		if err != nil || got != def {
			t.Fatalf("save %q: %q %v", save, got, err)
		}
		if fi, err := os.Stat(def); err != nil || !fi.IsDir() {
			t.Fatalf("default not made: %v", err)
		}
	}
	os.MkdirAll(filepath.Join(card, "Podcasts"), 0o755) // anywhere on the card, not only musics/
	if got, _ := SaveDir(app, Settings{Save: "Podcasts"}); got != filepath.Join(card, "Podcasts") {
		t.Fatalf("chosen %q", got)
	}
}

// Logs to Tailscale are off unless the settings say logs=on, and saved as that line.
func TestLogsAreOffUnlessTheSettingsSayOn(t *testing.T) {
	dir := t.TempDir()
	if LoadSettings(dir).Logs {
		t.Fatal("logs on with no settings")
	}
	if err := SaveSettings(dir, Settings{Hostname: "q2-pod", Logs: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "settings"))
	if !strings.Contains(string(b), "logs=on\n") || !LoadSettings(dir).Logs {
		t.Fatalf("settings %q", b)
	}
	SaveSettings(dir, Settings{Hostname: "q2-pod"})
	if b, _ := os.ReadFile(filepath.Join(dir, "settings")); strings.Contains(string(b), "logs") {
		t.Fatalf("settings %q", b)
	}
}

// The developer option is kept with the other settings, off unless chosen.
func TestRemoteSetting(t *testing.T) {
	dir := t.TempDir()
	if LoadSettings(dir).Remote {
		t.Fatal("on by default")
	}
	s := LoadSettings(dir)
	s.Remote = true
	if err := SaveSettings(dir, s); err != nil {
		t.Fatal(err)
	}
	if !LoadSettings(dir).Remote {
		t.Fatal("not kept")
	}
}

// App pages on the tailnet, and over HTTPS, are off unless chosen, and kept once chosen.
func TestAppsSettings(t *testing.T) {
	dir := t.TempDir()
	if s := LoadSettings(dir); s.Apps || s.HTTPS {
		t.Fatal("on before chosen")
	}
	s := LoadSettings(dir)
	s.Apps, s.HTTPS = true, true
	if err := SaveSettings(dir, s); err != nil {
		t.Fatal(err)
	}
	if s := LoadSettings(dir); !s.Apps || !s.HTTPS {
		t.Fatalf("not kept: %+v", s)
	}
}
