// Package keystore decides where the node's keys (tailscaled.state) are kept: in the Q2's internal
// flash, /mnt/data, as Q2 Pod keeps Wi-Fi passwords, so a card taken out or copied holds no way into
// the tailnet. The card is exFAT, with no permissions. A login on the card from before moves to the
// flash once, whole and read back before the card's copy goes.
package keystore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// File is the keys' file, as tsnet names it in its state folder.
const File = "tailscaled.state"

// Where is where the keys are kept; Moved when a login on the card was moved to the flash.
type Where struct {
	Path  string
	Moved bool
}

// Place picks the keys' file between the card's state folder and flash, the app's own folder in the
// Q2's internal storage, <data>/apps/<id> (the app deletes it itself after the App Store's Remove:
// startRemoveWatch). Not on a Q2 (no <data>) the card keeps them. On a Q2 flash takes them, made only root
// can read: a login found first in an older flash folder, else on the card, is moved there, written
// whole and read back before the old copy goes; copies left elsewhere once flash has them go too. An
// error leaves them where they were.
func Place(card, flash string, older ...string) (Where, error) {
	cardFile, flashFile := filepath.Join(card, File), filepath.Join(flash, File)
	onCard := Where{Path: cardFile}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(flash))); err != nil {
		return onCard, nil // no internal storage here: not a Q2
	}
	if err := os.MkdirAll(flash, 0o700); err != nil {
		return onCard, fmt.Errorf("internal storage: %w", err)
	}
	if err := os.Chmod(flash, 0o700); err != nil {
		return onCard, fmt.Errorf("internal storage: %w", err)
	}
	from := make([]string, 0, len(older)+1) // where a login may be, newest place first
	for _, o := range older {
		from = append(from, filepath.Join(o, File))
	}
	from = append(from, cardFile)
	gone := func() { // the copies from before the move: flash's is the one in use
		for _, f := range from {
			os.Remove(f)
		}
		for _, o := range older {
			os.Remove(o) // only once empty
		}
	}
	if fi, err := os.Stat(flashFile); err == nil && fi.Size() > 0 {
		gone()
		return Where{Path: flashFile}, nil
	}
	for i, f := range from {
		keys, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) || (err == nil && len(keys) == 0) {
			continue
		}
		stay := Where{Path: f}
		if err != nil {
			return stay, err
		}
		if err := writeWhole(flashFile, keys); err != nil {
			os.Remove(flashFile)
			return stay, fmt.Errorf("internal storage: %w", err)
		}
		if back, err := os.ReadFile(flashFile); err != nil || !bytes.Equal(back, keys) {
			os.Remove(flashFile)
			return stay, errors.New("internal storage: the keys read back differently")
		}
		gone()
		if _, err := os.Stat(from[i]); err == nil {
			return Where{Path: flashFile, Moved: true}, fmt.Errorf("the old copy stayed: %s", from[i])
		}
		return Where{Path: flashFile, Moved: true}, nil
	}
	if err := probe(flash); err != nil {
		return onCard, fmt.Errorf("internal storage: %w", err)
	}
	return Where{Path: flashFile}, nil // not signed in yet: it starts in flash
}

// writeWhole writes path through a temporary file, synced, then renamed over it.
func writeWhole(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// probe checks dir takes a file: a full or read-only flash fails here, not at the first sign-in.
func probe(dir string) error {
	p := filepath.Join(dir, ".probe")
	if err := writeWhole(p, []byte{0}); err != nil {
		return err
	}
	return os.Remove(p)
}
