// Package config reads and writes the app's settings and forwards in its card folder (.tailscale):
//
//	settings      key=value lines: hostname (the device's name on the tailnet), ports (extra ports every
//	              machine listens on, comma-separated, beyond DefaultPorts), connect=off (Tailscale turned
//	              off on the page: signed in, not connected), autostart=off (not started with Wi-Fi),
//	              save (Taildrop's folder on the card), logs=on (Tailscale's own logs sent to it)
//	q2app         Q2 Pod's file for the app; its wifi=1 line starts it with Wi-Fi (AutoStart)
//	forwards.txt  "<local port> <machine or Tailscale IP> <port>" lines, # comments: 127.0.0.1:<local port>
//	              goes to that machine's port
//
// Files are written whole, to a .tmp beside them and renamed, so a card pulled mid-write keeps the old one.
package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// DefaultPorts every machine listens on: web (80, 443 and Tailscale Serve), Navidrome (4533), Jellyfin
// (8096, 8920) and Plex (32400).
var DefaultPorts = []uint16{80, 443, 4533, 8096, 8920, 32400}

// DefaultHostname is the device's name on the tailnet unless the settings say otherwise.
const DefaultHostname = "q2-pod"

// Settings are the user's choices.
type Settings struct {
	Hostname string
	Ports    []uint16 // beyond DefaultPorts
	Off      bool     // Tailscale turned off: signed in, not connected
	Manual   bool     // not started with Wi-Fi: only when opened (q2app without wifi=1)
	Save     string   // where Taildrop files go, relative to the card; "" is DefaultSave
	Logs     bool     // Tailscale's logs uploaded to it (and buffered on the card); off unless chosen
	Remote   bool     // q2remote passed through the tailnet (developer firmware only); off unless chosen
}

// DefaultSave is the folder on the card Taildrop files go to unless the settings say otherwise.
const DefaultSave = "musics/Taildrop"

// CardRoot is the card: the folder the app's folder is in.
func CardRoot(appDir string) string { return filepath.Dir(appDir) }

// ValidSave is whether rel names a folder on the card: relative, clean, no part starting with a dot
// (.., or a hidden folder such as the apps' .tailscale and .spotify). "" is the card itself.
func ValidSave(rel string) bool {
	if rel == "" {
		return true
	}
	if filepath.IsAbs(rel) || filepath.Clean(rel) != rel {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

// SaveDir is the folder Taildrop files go to now: the chosen one if it's valid and there, else
// DefaultSave, made if missing.
func SaveDir(appDir string, s Settings) (string, error) {
	root := CardRoot(appDir)
	if s.Save != "" && ValidSave(s.Save) {
		dir := filepath.Join(root, s.Save)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir, nil
		}
	}
	dir := filepath.Join(root, DefaultSave)
	return dir, os.MkdirAll(dir, 0o755)
}

// Forward sends 127.0.0.1:LocalPort to Host:Port; Host is a MagicDNS name or a Tailscale IP.
type Forward struct {
	LocalPort uint16
	Host      string
	Port      uint16
}

// ValidHostname is a DNS label in lower case: 1 to 63 of a-z0-9 and -, not starting or ending with -.
func ValidHostname(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%q is not a port (1 to 65535)", s)
	}
	return uint16(n), nil
}

// ParsePorts reads a comma-separated port list, duplicates dropped, in its order.
func ParsePorts(s string) ([]uint16, error) {
	var out []uint16
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, f := range strings.Split(s, ",") {
		p, err := parsePort(f)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// LoadSettings reads dir/settings; a missing file, or a value that doesn't parse, is its default.
func LoadSettings(dir string) Settings {
	s := Settings{Hostname: DefaultHostname}
	b, err := os.ReadFile(filepath.Join(dir, "settings"))
	if err != nil {
		return s
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "hostname":
			if v = strings.TrimSpace(v); ValidHostname(v) {
				s.Hostname = v
			}
		case "ports":
			if p, err := ParsePorts(v); err == nil {
				s.Ports = p
			}
		case "connect":
			s.Off = strings.TrimSpace(v) == "off"
		case "autostart":
			s.Manual = strings.TrimSpace(v) == "off"
		case "save":
			s.Save = strings.TrimSpace(v)
		case "logs":
			s.Logs = strings.TrimSpace(v) == "on"
		case "remote":
			s.Remote = strings.TrimSpace(v) == "on"
		}
	}
	return s
}

// SaveSettings writes dir/settings.
func SaveSettings(dir string, s Settings) error {
	ports := make([]string, len(s.Ports))
	for i, p := range s.Ports {
		ports[i] = strconv.Itoa(int(p))
	}
	text := "hostname=" + s.Hostname + "\nports=" + strings.Join(ports, ",") + "\n"
	if s.Off {
		text += "connect=off\n"
	}
	if s.Manual {
		text += "autostart=off\n"
	}
	if s.Save != "" {
		text += "save=" + s.Save + "\n"
	}
	if s.Logs {
		text += "logs=on\n"
	}
	if s.Remote {
		text += "remote=on\n"
	}
	return writeWhole(filepath.Join(dir, "settings"), []byte(text))
}

// AutoStart is whether dir/q2app has wifi=1: Q2 Pod starts the app when Wi-Fi connects.
func AutoStart(dir string) bool {
	b, _ := os.ReadFile(filepath.Join(dir, "q2app"))
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, _ := strings.Cut(strings.TrimRight(line, "\r"), "="); strings.TrimSpace(k) == "wifi" {
			return strings.TrimSpace(v) == "1"
		}
	}
	return false
}

// SetAutoStart puts wifi=1 in dir/q2app, or takes the wifi line out, leaving the other lines as they
// were. Q2 Pod reads it at its next scan of the card's apps.
func SetAutoStart(dir string, on bool) error {
	path := filepath.Join(dir, "q2app")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if k, _, _ := strings.Cut(line, "="); strings.TrimSpace(k) == "wifi" || line == "" {
			continue
		}
		out = append(out, line)
	}
	if on {
		out = append(out, "wifi=1")
	}
	text := strings.Join(out, "\n") + "\n"
	if text == string(b) {
		return nil
	}
	return writeWhole(path, []byte(text))
}

// AllPorts are DefaultPorts and the extra ones, sorted, once each.
func AllPorts(s Settings) []uint16 {
	out := slices.Clone(DefaultPorts)
	for _, p := range s.Ports {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// LoadForwards reads dir/forwards.txt: the forwards, and an error for each line that isn't one (by line
// number). A missing file is none.
func LoadForwards(dir string) ([]Forward, []error) {
	b, err := os.ReadFile(filepath.Join(dir, "forwards.txt"))
	if err != nil {
		return nil, nil
	}
	var out []Forward
	var errs []error
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			errs = append(errs, fmt.Errorf("forwards.txt line %d: want <local port> <machine> <port>", n))
			continue
		}
		lp, err1 := parsePort(f[0])
		p, err2 := parsePort(f[2])
		if err1 != nil || err2 != nil {
			errs = append(errs, fmt.Errorf("forwards.txt line %d: a port is 1 to 65535", n))
			continue
		}
		out = append(out, Forward{lp, f[1], p})
	}
	return out, errs
}

// SaveForwards writes dir/forwards.txt, a header comment first.
func SaveForwards(dir string, fs []Forward) error {
	var b strings.Builder
	b.WriteString("# <local port> <machine or Tailscale IP> <port>: 127.0.0.1:<local port> reaches that machine's port\n")
	for _, f := range fs {
		fmt.Fprintf(&b, "%d %s %d\n", f.LocalPort, f.Host, f.Port)
	}
	return writeWhole(filepath.Join(dir, "forwards.txt"), []byte(b.String()))
}

func writeWhole(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
