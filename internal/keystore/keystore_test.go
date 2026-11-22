package keystore

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// q2 is a card's state folder and an internal flash (its /mnt/data) to place the keys between.
func q2(t *testing.T) (card, flash string) {
	t.Helper()
	root := t.TempDir()
	card, data := filepath.Join(root, "mmc", ".tailscale", "state"), filepath.Join(root, "data")
	for _, d := range []string{card, data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return card, filepath.Join(data, "apps", "tailscale")
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestNotAQ2KeepsItOnTheCard(t *testing.T) {
	card := t.TempDir()
	got, err := Place(card, filepath.Join(t.TempDir(), "missing", "apps", "tailscale"))
	if err != nil || got.Path != filepath.Join(card, File) || got.Moved {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestAFreshQ2KeepsItInFlashOnlyRootCanRead(t *testing.T) {
	card, flash := q2(t)
	got, err := Place(card, flash)
	if err != nil || got.Path != filepath.Join(flash, File) || got.Moved {
		t.Fatalf("got %+v, %v", got, err)
	}
	if fi, err := os.Stat(flash); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("flash folder %v, %v", fi, err)
	}
	if entries, _ := os.ReadDir(flash); len(entries) != 0 {
		t.Fatalf("left behind %v", entries) // the write probe is gone
	}
}

func TestACardLoginMovesToFlashWhole(t *testing.T) {
	card, flash := q2(t)
	write(t, filepath.Join(card, File), `{"_machinekey":"secret"}`)
	got, err := Place(card, flash)
	if err != nil || got.Path != filepath.Join(flash, File) || !got.Moved {
		t.Fatalf("got %+v, %v", got, err)
	}
	b, err := os.ReadFile(got.Path)
	if err != nil || !bytes.Equal(b, []byte(`{"_machinekey":"secret"}`)) {
		t.Fatalf("flash holds %q, %v", b, err)
	}
	if fi, _ := os.Stat(got.Path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if exists(filepath.Join(card, File)) {
		t.Fatal("the card still holds the keys")
	}
}

func TestFlashWinsAndALeftoverCardCopyGoes(t *testing.T) {
	card, flash := q2(t)
	if err := os.MkdirAll(flash, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(flash, File), "new")
	write(t, filepath.Join(card, File), "old")
	got, err := Place(card, flash)
	if err != nil || got.Path != filepath.Join(flash, File) || got.Moved {
		t.Fatalf("got %+v, %v", got, err)
	}
	if b, _ := os.ReadFile(got.Path); string(b) != "new" || exists(filepath.Join(card, File)) {
		t.Fatalf("flash %q, card copy left: %v", b, exists(filepath.Join(card, File)))
	}
}

func TestAnEmptyFlashFileTakesTheCardsLogin(t *testing.T) {
	card, flash := q2(t)
	if err := os.MkdirAll(flash, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(flash, File), "")
	write(t, filepath.Join(card, File), "keys")
	got, err := Place(card, flash)
	if b, _ := os.ReadFile(got.Path); err != nil || !got.Moved || string(b) != "keys" {
		t.Fatalf("got %+v %q, %v", got, b, err)
	}
}

func TestUnwritableFlashKeepsTheCardsLoginAndSaysSo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	card, flash := q2(t)
	write(t, filepath.Join(card, File), "keys")
	data := filepath.Dir(filepath.Dir(flash)) // /mnt/data: no apps/ can be made in it
	if err := os.Chmod(data, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(data, 0o755) })
	got, err := Place(card, flash)
	if err == nil || got.Path != filepath.Join(card, File) || got.Moved {
		t.Fatalf("got %+v, %v", got, err)
	}
	if b, _ := os.ReadFile(got.Path); string(b) != "keys" {
		t.Fatalf("card lost its keys: %q", b)
	}
}

func TestAnOlderFlashFolderMovesToTheNewOneAndGoes(t *testing.T) {
	card, flash := q2(t)
	older := filepath.Join(filepath.Dir(filepath.Dir(flash)), "q2-tailscale")
	if err := os.MkdirAll(older, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(older, File), "keys")
	got, err := Place(card, flash, older)
	if err != nil || got.Path != filepath.Join(flash, File) || !got.Moved {
		t.Fatalf("got %+v, %v", got, err)
	}
	if b, _ := os.ReadFile(got.Path); string(b) != "keys" || exists(older) {
		t.Fatalf("flash %q, older folder left: %v", b, exists(older))
	}
}
