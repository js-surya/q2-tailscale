package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// removeFixture is a card (mounted per mounts) with the app's folder on it, and a login in keys.
func removeFixture(t *testing.T, mounted bool) (dir, keys, mounts string) {
	t.Helper()
	root := t.TempDir()
	card := filepath.Join(root, "mmc")
	dir = filepath.Join(card, ".tailscale")
	keys = filepath.Join(root, "data", "apps", "tailscale")
	for _, d := range []string{dir, keys} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(keys, "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	mounts = filepath.Join(root, "mounts")
	line := "/dev/root / ext4 rw 0 0\n"
	if mounted {
		line += "/dev/mmcblk0p1 " + card + " exfat rw 0 0\n"
	}
	if err := os.WriteFile(mounts, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, keys, mounts
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestRemoveTakesTheLoginToo(t *testing.T) {
	dir, keys, mounts := removeFixture(t, true)
	cmd := watchRemove(dir, keys, mounts, 40, "0.05")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the App Store deletes the folder after the app has stopped
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if exists(keys) {
		t.Fatal("login left behind after Remove")
	}
}

func TestAStopKeepsTheLogin(t *testing.T) {
	dir, keys, mounts := removeFixture(t, true)
	if err := watchRemove(dir, keys, mounts, 3, "0.05").Run(); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(keys, "tailscaled.state")) {
		t.Fatal("login deleted on an ordinary stop")
	}
}

func TestACardTakenOutKeepsTheLogin(t *testing.T) {
	dir, keys, mounts := removeFixture(t, false)
	if err := os.RemoveAll(dir); err != nil { // the folder is gone with the card, not removed
		t.Fatal(err)
	}
	if err := watchRemove(dir, keys, mounts, 3, "0.05").Run(); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(keys, "tailscaled.state")) {
		t.Fatal("login deleted with the card out")
	}
}

func TestNoWatchOnAShallowPath(t *testing.T) {
	for _, keys := range []string{"", "/", "/mnt", "/mnt/data", "relative/apps/tailscale"} {
		if watchRemove("/mnt/mmc/.tailscale", keys, "/proc/mounts", 1, "1") != nil {
			t.Errorf("watch for keys %q", keys)
		}
	}
	if watchRemove("/", "/mnt/data/apps/tailscale", "/proc/mounts", 1, "1") != nil {
		t.Error("watch for an app folder that is the root")
	}
}
