package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"q2-tailscale/internal/vault"
)

type fakeVault struct {
	data    []byte
	err     error // returned by Get and Put while set
	gets    int
	puts    int
	putErr  error
	corrupt bool // Get returns something other than what was put
}

func (f *fakeVault) Get() ([]byte, error) {
	f.gets++
	if f.err != nil {
		return nil, f.err
	}
	if f.data == nil {
		return nil, vault.ErrNone
	}
	if f.corrupt {
		return []byte("x"), nil
	}
	return append([]byte(nil), f.data...), nil
}

func (f *fakeVault) Put(b []byte) error {
	if f.err != nil {
		return f.err
	}
	if f.putErr != nil {
		return f.putErr
	}
	f.puts++
	f.data = append([]byte(nil), b...)
	return nil
}

type world struct {
	card, keys, tmp string
	v               *fakeVault
	k               *Keeper
	removed         bool          // the Vault's program gone from the card
	slept, upAfter  time.Duration // time "slept"; the Vault answers once slept reaches upAfter (0: never)
}

func newWorld(t *testing.T) *world {
	root := t.TempDir()
	w := &world{card: filepath.Join(root, "card", "state"), keys: filepath.Join(root, "data"),
		tmp: filepath.Join(root, "tmp"), v: &fakeVault{}}
	for _, d := range []string{w.card, w.keys, w.tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	w.k = w.keeper(t)
	return w
}

func (w *world) keeper(t *testing.T) *Keeper {
	return &Keeper{Vault: w.v, Card: w.card, Keys: w.keys, Tmp: w.tmp, Logf: t.Logf,
		Installed: func() bool { return !w.removed }, Wait: 20 * time.Second,
		Sleep: func(d time.Duration) {
			w.slept += d
			if w.upAfter > 0 && w.slept >= w.upAfter {
				w.v.err = nil
			}
		}}
}

func put(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := writeFile(filepath.Join(dir, name), []byte(data)); err != nil {
		t.Fatal(err)
	}
}

func read(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func (w *world) cardSecrets(t *testing.T) {
	put(t, w.card, LogConf, "logkey")
	put(t, w.card, "certs/acme-account.key.pem", "acme")
	put(t, w.card, "certs/q2.ts.net.key", "key")
	put(t, w.card, "certs/q2.ts.net.crt", "crt")
	put(t, w.card, "profile-data/c67f/netmap-cache/x", "netmap")
}

func TestNoVaultKeepsTheCard(t *testing.T) {
	w := newWorld(t)
	w.cardSecrets(t)
	w.v.err = vault.ErrNoVault
	if got := w.k.Start(); got != w.card {
		t.Fatalf("folder %q, want the card's", got)
	}
	if read(w.card, "certs/q2.ts.net.key") != "key" || exists(filepath.Join(w.keys, Marker)) {
		t.Fatal("card changed without a Vault")
	}
	w.k.Sync()
	w.k.Stop()
	if w.v.puts != 0 {
		t.Fatal("saved without a Vault")
	}
}

func TestFirstStartMovesTheCardsSecretsIn(t *testing.T) {
	w := newWorld(t)
	w.cardSecrets(t)
	ram := w.k.Start()
	if ram == w.card || filepath.Dir(ram) != w.tmp {
		t.Fatalf("folder %q, want a RAM folder in %q", ram, w.tmp)
	}
	for name, want := range map[string]string{LogConf: "logkey", "certs/q2.ts.net.key": "key",
		"certs/q2.ts.net.crt": "crt", "certs/acme-account.key.pem": "acme"} {
		if got := read(ram, name); got != want {
			t.Errorf("RAM %s = %q, want %q", name, got, want)
		}
		if exists(filepath.Join(w.card, name)) {
			t.Errorf("%s still on the card", name)
		}
	}
	if exists(filepath.Join(w.card, "profile-data")) || exists(filepath.Join(w.card, CertsDir)) {
		t.Error("netmap cache or certs folder left on the card")
	}
	if !exists(filepath.Join(w.keys, Marker)) {
		t.Error("no marker")
	}
	m, err := decode(w.v.data)
	if err != nil || len(m) != 4 || string(m["certs/q2.ts.net.key"]) != "key" {
		t.Fatalf("Vault has %v, %v", m, err)
	}
	if fi, _ := os.Stat(ram); fi.Mode().Perm() != 0o700 {
		t.Errorf("RAM folder mode %v", fi.Mode().Perm())
	}
}

func TestReceivedFilesAndLogsStayOnTheCard(t *testing.T) {
	w := newWorld(t)
	ram := w.k.Start()
	for _, name := range onCard {
		target, err := os.Readlink(filepath.Join(ram, name))
		if err != nil || target != filepath.Join(w.card, name) {
			t.Errorf("%s -> %q, %v", name, target, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(ram, "files", "me"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(w.card, "files", "me")) {
		t.Error("received files not on the card")
	}
}

func TestNextStartRestoresFromTheVault(t *testing.T) {
	w := newWorld(t)
	w.cardSecrets(t)
	first := w.k.Start()
	w.k.Stop()
	if exists(first) {
		t.Error("RAM folder left after the stop")
	}
	k := w.keeper(t)
	ram := k.Start()
	if read(ram, "certs/q2.ts.net.key") != "key" || read(ram, LogConf) != "logkey" {
		t.Fatal("not restored")
	}
	puts := w.v.puts
	k.Sync()
	if w.v.puts != puts {
		t.Error("saved with nothing changed")
	}
}

func TestARenewedCertificateIsSaved(t *testing.T) {
	w := newWorld(t)
	w.cardSecrets(t)
	ram := w.k.Start()
	put(t, ram, "certs/q2.ts.net.crt", "renewed")
	w.k.Sync()
	m, _ := decode(w.v.data)
	if string(m["certs/q2.ts.net.crt"]) != "renewed" {
		t.Fatal("renewal not saved")
	}
	if exists(filepath.Join(w.card, "certs/q2.ts.net.crt")) {
		t.Fatal("renewal written to the card")
	}
}

func TestVaultNotRunningYetAfterTheMove(t *testing.T) {
	w := newWorld(t)
	w.cardSecrets(t)
	w.k.Start()
	w.k.Stop()

	w.v.err = vault.ErrNoVault // the Vault not started yet
	k := w.keeper(t)
	ram := k.Start()
	if ram == w.card {
		t.Fatal("fell back to the card after the move")
	}
	if exists(filepath.Join(ram, "certs/q2.ts.net.key")) {
		t.Fatal("restored without a Vault")
	}
	put(t, ram, "certs/new.ts.net.key", "got-meanwhile") // a certificate got while it was away
	k.Sync()
	if exists(filepath.Join(w.card, "certs")) {
		t.Fatal("secrets written to the card")
	}

	w.v.err = nil
	k.Sync()
	if read(ram, "certs/q2.ts.net.key") != "key" || read(ram, "certs/new.ts.net.key") != "got-meanwhile" {
		t.Fatal("late restore lost a file")
	}
	m, _ := decode(w.v.data)
	if string(m["certs/new.ts.net.key"]) != "got-meanwhile" || string(m["certs/q2.ts.net.key"]) != "key" {
		t.Fatalf("Vault has %v", m)
	}
}

func TestAFailedSaveKeepsTheCard(t *testing.T) {
	w := newWorld(t)
	w.cardSecrets(t)
	w.v.putErr = vault.ErrTooLarge
	w.k.Start()
	if read(w.card, "certs/q2.ts.net.key") != "key" || exists(filepath.Join(w.keys, Marker)) {
		t.Fatal("card deleted though the save failed")
	}
}

func TestAVaultThatDoesntGiveThemBackKeepsTheCard(t *testing.T) {
	w := newWorld(t)
	w.cardSecrets(t)
	w.v.corrupt = true
	w.k.Start()
	if read(w.card, "certs/q2.ts.net.key") != "key" || exists(filepath.Join(w.keys, Marker)) {
		t.Fatal("card deleted though the Vault didn't give them back")
	}
}

func TestNoSecretsYetStillMarks(t *testing.T) {
	w := newWorld(t)
	ram := w.k.Start()
	if ram == w.card || !exists(filepath.Join(w.keys, Marker)) {
		t.Fatal("a fresh install with a Vault should use it")
	}
	if w.v.puts != 0 {
		t.Error("saved an empty bundle")
	}
}

func TestBadNamesFromTheVaultAreRefused(t *testing.T) {
	for _, name := range []string{"../x", "certs/../../x", "certs/", "certs/a/b", "tailscaled.state", "certs/..", "/etc/passwd"} {
		if _, err := decode(encode(map[string][]byte{name: []byte("x")})); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
	if _, err := decode(encode(map[string][]byte{LogConf: []byte("x"), "certs/a.b-c_d.key": []byte("y")})); err != nil {
		t.Errorf("good names refused: %v", err)
	}
}

func TestOldRAMFoldersAreRemoved(t *testing.T) {
	w := newWorld(t)
	old := filepath.Join(w.tmp, RAMPrefix+"old")
	put(t, old, "certs/x.key", "x")
	w.k.Start()
	if exists(old) {
		t.Error("old RAM folder left")
	}
}

func TestEncodingIsStable(t *testing.T) {
	m := map[string][]byte{"certs/b": []byte("2"), LogConf: []byte("1"), "certs/a": []byte("3")}
	if !bytes.Equal(encode(m), encode(map[string][]byte{"certs/a": []byte("3"), "certs/b": []byte("2"), LogConf: []byte("1")})) {
		t.Fatal("same files, different bytes")
	}
}

func TestAVaultStartedLateIsWaitedFor(t *testing.T) {
	w := newWorld(t)
	w.cardSecrets(t)
	w.k.Start()
	w.k.Stop()

	w.v.err, w.upAfter = vault.ErrNoVault, 5*time.Second // older firmware: the Vault starts 5 s after
	k := w.keeper(t)
	ram := k.Start()
	if read(ram, "certs/q2.ts.net.key") != "key" {
		t.Fatal("didn't wait for the Vault: Tailscale would ask for a new certificate")
	}
	if w.slept != 5*time.Second {
		t.Errorf("waited %v", w.slept)
	}
}

func TestAVaultThatNeverAnswersIsWaitedForOnlySoLong(t *testing.T) {
	w := newWorld(t)
	w.cardSecrets(t)
	w.k.Start()
	w.k.Stop()
	w.v.err = vault.ErrNoVault
	if ram := w.keeper(t).Start(); ram == w.card || w.slept != 20*time.Second {
		t.Fatalf("folder %q after %v", ram, w.slept)
	}
}

func TestNoWaitWithoutTheMarker(t *testing.T) {
	w := newWorld(t)
	w.v.err = vault.ErrNoVault
	w.k.Start()
	if w.slept != 0 {
		t.Fatalf("waited %v with nothing in the Vault", w.slept)
	}
}

func TestARemovedVaultGoesBackToTheCard(t *testing.T) {
	w := newWorld(t)
	w.cardSecrets(t)
	w.k.Start()
	w.k.Stop()
	w.removed, w.v.err = true, vault.ErrNoVault
	if got := w.keeper(t).Start(); got != w.card {
		t.Fatalf("folder %q, want the card's", got)
	}
	if exists(filepath.Join(w.keys, Marker)) || w.slept != 0 {
		t.Fatal("marker kept or waited for a removed Vault")
	}
}
