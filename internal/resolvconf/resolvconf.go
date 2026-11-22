// Package resolvconf puts the Tailscale name server first in the Q2's resolv.conf and takes it out again.
//
// On the Q2 /etc/resolv.conf links to /tmp/resolv.conf, which busybox udhcpc's default.script rewrites on
// each lease: it removes the lines ending "# wlan0" and appends its own. Our lines end "# q2-tailscale", so
// a renew keeps them, first, and a stale pair from a run that was killed is found and replaced. Files are
// written in place, through the link, as the DHCP client writes them.
package resolvconf

import (
	"bytes"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"strings"
	"time"
)

// Mark ends every line this package writes.
const Mark = " # q2-tailscale"

// Rewrite is b with our lines first: our name server, and the MagicDNS search domain when suffix isn't
// empty; every other line kept in order, any earlier line of ours dropped.
func Rewrite(b []byte, suffix string) []byte {
	var out bytes.Buffer
	out.WriteString("nameserver 127.0.0.1" + Mark + "\n")
	if suffix != "" {
		out.WriteString("search " + suffix + Mark + "\n")
	}
	out.Write(Strip(b))
	return out.Bytes()
}

// Strip is b without our lines.
func Strip(b []byte) []byte {
	var out bytes.Buffer
	for _, line := range lines(b) {
		if !strings.HasSuffix(line, Mark) {
			out.WriteString(line + "\n")
		}
	}
	return out.Bytes()
}

// Upstreams are the IPv4 name servers in b, port 53, other than ours and any on loopback (a question
// sent there would come back to us).
func Upstreams(b []byte) []netip.AddrPort {
	var out []netip.AddrPort
	for _, line := range lines(b) {
		if strings.HasSuffix(line, Mark) {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		ip, err := netip.ParseAddr(f[1])
		if err != nil || !ip.Is4() || ip.IsLoopback() {
			continue
		}
		out = append(out, netip.AddrPortFrom(ip, 53))
	}
	return out
}

func lines(b []byte) []string {
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// Apply writes our lines first in the file at path (created if missing), through a link.
func Apply(path, suffix string) error {
	_, err := ApplyChanged(path, suffix)
	return err
}

// settle is how long the file must have gone unchanged before it is read for a rewrite: udhcpc's
// default.script empties it and then appends a line at a time, and a rewrite from a read in between
// would lose the lines still to come.
var settle = 150 * time.Millisecond

// ApplyChanged is Apply, writing only when the file isn't already as it should be; changed says
// whether it wrote. It reads a settled file: one unchanged for settle, and unchanged across the read
// (a writer emptying it between a look and the read would leave a read of nothing).
func ApplyChanged(path, suffix string) (changed bool, err error) {
	old, err := settled(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	want := Rewrite(old, suffix)
	if bytes.Equal(old, want) {
		return false, nil
	}
	return true, write(path, want)
}

// settled reads path once it has gone settle unchanged and didn't change while being read; a writer
// that never stops gets a read anyway, a second and a half on.
func settled(path string) ([]byte, error) {
	var b []byte
	var err error
	for i := 0; i < 10; i++ {
		before, serr := os.Stat(path)
		if serr != nil {
			return os.ReadFile(path)
		}
		if age := time.Since(before.ModTime()); age >= 0 && age < settle {
			time.Sleep(settle - age)
			continue
		}
		b, err = os.ReadFile(path)
		after, serr := os.Stat(path)
		if err == nil && serr == nil && after.ModTime().Equal(before.ModTime()) && after.Size() == int64(len(b)) {
			return b, nil
		}
	}
	if b == nil && err == nil {
		b, err = os.ReadFile(path)
	}
	return b, err
}

// Restore takes our lines out of the file at path; a missing file is left missing.
func Restore(path string) error {
	old, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if want := Strip(old); !bytes.Equal(old, want) {
		return write(path, want)
	}
	return nil
}

func write(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
