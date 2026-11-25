// Package secrets keeps Tailscale's secrets that live outside the login (the HTTPS certificate and its
// private keys, the log key) in the Q2 Vault instead of plain files on the card.
//
// With a Vault, Tailscale's folder (tsnet's Dir) is a root-only folder in RAM (/tmp): the secrets are
// written there from the Vault at start and saved back when they change; the netmap cache stays in RAM
// only; received files and the log buffers stay on the card through links to the card's state folder.
// The login itself stays in internal storage (keystore), so Tailscale starts even without the Vault.
//
// Without a Vault (not installed, or never used yet and not answering) nothing changes: Tailscale's
// folder is the card's state folder, as before. Once the secrets have moved into the Vault, a marker in
// internal storage says so, and Tailscale never writes them to the card again: a Vault not running yet
// at start is asked again later.
package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"q2-tailscale/internal/vault"
)

// LogConf is tsnet's log key file in its folder; CertsDir its certificates' folder.
const (
	LogConf  = "tailscaled.log.conf"
	CertsDir = "certs"
	// Marker, in the login's internal folder, says the secrets are in the Vault.
	Marker = "secrets-in-vault"
	// RAMPrefix is the RAM folder's name in /tmp, followed by a random part.
	RAMPrefix = "q2-tailscale.var."
)

// onCard are the names in Tailscale's folder that stay on the card (linked from the RAM folder): received
// files, which can be large, and the log buffers.
var onCard = []string{"files", "tailscaled.log1.txt", "tailscaled.log2.txt"}

// Vault is the Vault client (vault.Get and vault.Put); a fake in tests.
type Vault interface {
	Get() ([]byte, error)
	Put([]byte) error
}

// Client is the real Vault.
type Client struct{}

func (Client) Get() ([]byte, error) { return vault.Get() }
func (Client) Put(b []byte) error   { return vault.Put(b) }

// Keeper moves the secrets into the Vault and keeps them in step.
type Keeper struct {
	Vault Vault
	Card  string // the card's state folder (.tailscale/state)
	Keys  string // the login's internal folder, for Marker
	Tmp   string // where the RAM folder goes (/tmp)
	Logf  func(string, ...any)

	ram      string // the RAM folder, "" when Tailscale's folder is Card
	restored bool   // the Vault's copy has been read (or there was none)
	saved    []byte // the bundle last saved, to save only changes
	warned   bool
}

// usable is whether err from the Vault means it answered for this app: a saved bundle or none yet.
func usable(err error) bool { return err == nil || errors.Is(err, vault.ErrNone) }

// Start chooses Tailscale's folder and returns it: the RAM folder with the secrets from the Vault, or
// the card's state folder as before when there's no Vault to use.
func (k *Keeper) Start() string {
	marked := exists(filepath.Join(k.Keys, Marker))
	b, err := k.Vault.Get()
	if !marked && !usable(err) {
		return k.Card // no Vault in use: as before
	}
	ram, err2 := k.makeRAM()
	if err2 != nil {
		k.Logf("secrets: RAM folder: %v; keeping them on the card", err2)
		return k.Card
	}
	k.ram = ram
	if usable(err) {
		k.restore(b)
		k.restored = true
		if err == nil {
			k.saved = b
		}
		k.moveFromCard(marked)
	} else {
		k.Logf("secrets: the Vault isn't answering (%v); the certificate loads once it is", err)
	}
	return ram
}

// Sync reads the Vault's copy if that couldn't be done at start, and saves the secrets when they
// changed. Called now and then and at the stop; nothing to do when Tailscale's folder is the card's.
func (k *Keeper) Sync() {
	if k.ram == "" {
		return
	}
	if !k.restored {
		b, err := k.Vault.Get()
		if !usable(err) {
			return
		}
		k.restore(b) // keeps files written meanwhile (a certificate got while the Vault was away)
		k.restored = true
		if err == nil {
			k.saved = b
		}
		k.moveFromCard(true)
	}
	k.save()
}

// save puts the RAM folder's secrets in the Vault when they differ from the last saved.
func (k *Keeper) save() bool {
	m, err := collect(k.ram)
	if err != nil {
		k.Logf("secrets: read: %v", err)
		return false
	}
	if len(m) == 0 {
		return true
	}
	b := encode(m)
	if bytes.Equal(b, k.saved) {
		return true
	}
	if err := k.Vault.Put(b); err != nil {
		if !k.warned {
			k.Logf("secrets: saving to the Vault: %v", err)
			k.warned = true
		}
		return false
	}
	k.saved, k.warned = b, false
	return true
}

// moveFromCard takes secrets still on the card (Tailscale before the Vault, or a run without it) into
// the RAM folder, saves them to the Vault, checks the Vault gives them back, marks the move, and only
// then deletes them and the netmap cache from the card. Files already in the RAM folder win.
func (k *Keeper) moveFromCard(marked bool) {
	m, err := collect(k.Card)
	if err != nil {
		k.Logf("secrets: card: %v", err)
		return
	}
	for name, b := range m {
		p := filepath.Join(k.ram, name)
		if !exists(p) {
			if err := writeFile(p, b); err != nil {
				k.Logf("secrets: %v", err)
				return
			}
		}
	}
	if len(m) > 0 || !marked {
		if !k.save() {
			return // kept on the card; tried again at the next start
		}
		if got, err := k.Vault.Get(); len(k.saved) > 0 && (err != nil || !bytes.Equal(got, k.saved)) {
			k.Logf("secrets: the Vault didn't give them back; kept on the card")
			return
		}
		if !marked {
			if err := os.WriteFile(filepath.Join(k.Keys, Marker), nil, 0o600); err != nil {
				k.Logf("secrets: marker: %v; kept on the card", err)
				return
			}
		}
	}
	for name := range m {
		os.Remove(filepath.Join(k.Card, name))
	}
	os.Remove(filepath.Join(k.Card, CertsDir)) // only when empty
	os.RemoveAll(filepath.Join(k.Card, "profile-data"))
	if len(m) > 0 {
		k.Logf("secrets: moved %d from the card into the Vault", len(m))
	}
}

// makeRAM makes a new root-only folder in Tmp (random name, so nothing in /tmp can stand in for it),
// after removing old ones of ours, and links the card's names in.
func (k *Keeper) makeRAM() (string, error) {
	old, _ := filepath.Glob(filepath.Join(k.Tmp, RAMPrefix+"*"))
	for _, p := range old {
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() && ownedByUs(fi) {
			os.RemoveAll(p)
		}
	}
	ram, err := os.MkdirTemp(k.Tmp, RAMPrefix)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(ram, 0o700); err != nil {
		os.RemoveAll(ram)
		return "", err
	}
	// A link to a missing folder can't be made into one (MkdirAll stops at it): the card's files folder
	// is made first; the log buffers are files, created through their links.
	if err := os.MkdirAll(filepath.Join(k.Card, "files"), 0o700); err != nil {
		os.RemoveAll(ram)
		return "", err
	}
	for _, name := range onCard {
		if err := os.Symlink(filepath.Join(k.Card, name), filepath.Join(ram, name)); err != nil {
			os.RemoveAll(ram)
			return "", err
		}
	}
	return ram, nil
}

// Stop saves any change and removes the RAM folder.
func (k *Keeper) Stop() {
	if k.ram == "" {
		return
	}
	k.Sync()
	os.RemoveAll(k.ram)
}

// restore writes the Vault's bundle into the RAM folder, keeping files already there.
func (k *Keeper) restore(b []byte) {
	if len(b) == 0 {
		return
	}
	m, err := decode(b)
	if err != nil {
		k.Logf("secrets: the Vault's copy: %v", err)
		return
	}
	for name, data := range m {
		p := filepath.Join(k.ram, name)
		if exists(p) {
			continue
		}
		if err := writeFile(p, data); err != nil {
			k.Logf("secrets: %v", err)
		}
	}
}

// bundle is what's sealed in the Vault: each secret file by its name in Tailscale's folder.
type bundle struct {
	V     int               `json:"v"`
	Files map[string][]byte `json:"files"`
}

func encode(m map[string][]byte) []byte {
	b, _ := json.Marshal(bundle{V: 1, Files: m}) // map keys sorted: the same files, the same bytes
	return b
}

func decode(b []byte) (map[string][]byte, error) {
	var bu bundle
	if err := json.Unmarshal(b, &bu); err != nil {
		return nil, err
	}
	if bu.V != 1 {
		return nil, fmt.Errorf("version %d", bu.V)
	}
	for name := range bu.Files {
		if !validName(name) {
			return nil, fmt.Errorf("bad name %q", name)
		}
	}
	return bu.Files, nil
}

// validName is LogConf or certs/<a plain file name>: nothing else is ever written from the Vault.
func validName(name string) bool {
	if name == LogConf {
		return true
	}
	base, ok := strings.CutPrefix(name, CertsDir+"/")
	if !ok || base == "" || base == "." || base == ".." || len(base) > 255 {
		return false
	}
	for _, r := range base {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// collect reads the secret files in dir: LogConf and certs/*; none is an empty map.
func collect(dir string) (map[string][]byte, error) {
	m := map[string][]byte{}
	names := []string{LogConf}
	ents, err := os.ReadDir(filepath.Join(dir, CertsDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range ents {
		if e.Type().IsRegular() {
			names = append(names, CertsDir+"/"+e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if !validName(name) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		m[name] = b
	}
	return m, nil
}

func writeFile(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func ownedByUs(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
