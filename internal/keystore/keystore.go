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

// Place picks the keys' file between the card's state folder and flash, a folder in the Q2's
// internal flash. Not on a Q2 (flash's parent missing) the card keeps them. On a Q2 flash takes them,
// made only root can read, a card copy moved there first; a copy left on the card once flash has
// them goes. An error leaves them on the card: flash couldn't be made, written or read back.
func Place(card, flash string) (Where, error) {
	cardFile, flashFile := filepath.Join(card, File), filepath.Join(flash, File)
	onCard := Where{Path: cardFile}
	if _, err := os.Stat(filepath.Dir(flash)); err != nil {
		return onCard, nil // no internal flash here: not a Q2
	}
	if err := os.MkdirAll(flash, 0o700); err != nil {
		return onCard, fmt.Errorf("internal storage: %w", err)
	}
	if err := os.Chmod(flash, 0o700); err != nil {
		return onCard, fmt.Errorf("internal storage: %w", err)
	}
	if fi, err := os.Stat(flashFile); err == nil && fi.Size() > 0 {
		os.Remove(cardFile) // a copy from before the move: the flash's is the one in use
		return Where{Path: flashFile}, nil
	}
	keys, err := os.ReadFile(cardFile)
	if errors.Is(err, os.ErrNotExist) {
		if err := probe(flash); err != nil {
			return onCard, fmt.Errorf("internal storage: %w", err)
		}
		return Where{Path: flashFile}, nil // not signed in yet: it starts in flash
	}
	if err != nil {
		return onCard, err
	}
	if err := writeWhole(flashFile, keys); err != nil {
		os.Remove(flashFile)
		return onCard, fmt.Errorf("internal storage: %w", err)
	}
	if back, err := os.ReadFile(flashFile); err != nil || !bytes.Equal(back, keys) {
		os.Remove(flashFile)
		return onCard, errors.New("internal storage: the keys read back differently")
	}
	if err := os.Remove(cardFile); err != nil {
		return Where{Path: flashFile, Moved: true}, fmt.Errorf("the card's copy stayed: %w", err)
	}
	return Where{Path: flashFile, Moved: true}, nil
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
