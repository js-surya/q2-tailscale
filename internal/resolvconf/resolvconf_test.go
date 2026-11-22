package resolvconf

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const udhcpc = "nameserver 192.168.1.1 # wlan0\nnameserver 8.8.4.4 # wlan0\n"

func TestRewriteOnUdhcpcFile(t *testing.T) {
	got := string(Rewrite([]byte(udhcpc), "tail1234.ts.net"))
	want := "nameserver 127.0.0.1 # q2-tailscale\nsearch tail1234.ts.net # q2-tailscale\n" + udhcpc
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestRewriteWithoutSuffix(t *testing.T) {
	got := string(Rewrite([]byte(udhcpc), ""))
	if got != "nameserver 127.0.0.1 # q2-tailscale\n"+udhcpc {
		t.Fatalf("got %q", got)
	}
}

func TestApplyRemovesStaleLines(t *testing.T) {
	old := "search old.ts.net # q2-tailscale\nnameserver 192.168.1.1 # wlan0\nnameserver 127.0.0.1 # q2-tailscale\n"
	got := string(Rewrite([]byte(old), "tail1234.ts.net"))
	if strings.Count(got, "# q2-tailscale") != 2 || strings.Contains(got, "old.ts.net") {
		t.Fatalf("got %q", got)
	}
}

// udhcpc's default.script on a renew: drop its own lines, append the new servers.
func renew(b []byte, servers ...string) []byte {
	var out []string
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if !strings.HasSuffix(l, "# wlan0") {
			out = append(out, l)
		}
	}
	for _, s := range servers {
		out = append(out, "nameserver "+s+" # wlan0")
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

func TestUdhcpcRewriteKeepsOurLinesFirst(t *testing.T) {
	b := renew(Rewrite([]byte(udhcpc), "tail1234.ts.net"), "10.0.0.1")
	lines := strings.Split(string(b), "\n")
	if lines[0] != "nameserver 127.0.0.1 # q2-tailscale" || lines[2] != "nameserver 10.0.0.1 # wlan0" {
		t.Fatalf("got %q", b)
	}
	if !reflect.DeepEqual(Upstreams(b), []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:53")}) {
		t.Fatalf("upstreams %v", Upstreams(b))
	}
}

func TestStrip(t *testing.T) {
	if got := string(Strip(Rewrite([]byte(udhcpc), "x.ts.net"))); got != udhcpc {
		t.Fatalf("got %q", got)
	}
	if got := string(Strip(nil)); got != "" {
		t.Fatalf("empty: %q", got)
	}
}

func TestUpstreamsSkipsOursAndLoopback(t *testing.T) {
	b := []byte("nameserver 127.0.0.1 # q2-tailscale\nnameserver 10.0.2.3\n# a comment\nnameserver 127.0.0.53\nnameserver ::1\nnameserver bad\nnameserver 1.1.1.1 # wlan0\n")
	want := []netip.AddrPort{netip.MustParseAddrPort("10.0.2.3:53"), netip.MustParseAddrPort("1.1.1.1:53")}
	if got := Upstreams(b); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestApplyThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tmp-resolv.conf")
	os.WriteFile(target, []byte(udhcpc), 0o644)
	link := filepath.Join(dir, "resolv.conf")
	os.Symlink("tmp-resolv.conf", link)
	if err := Apply(link, "tail1234.ts.net"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
	b, _ := os.ReadFile(target)
	if !strings.HasPrefix(string(b), "nameserver 127.0.0.1 # q2-tailscale\n") {
		t.Fatalf("target %q", b)
	}
	if err := Restore(link); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != udhcpc {
		t.Fatalf("restored %q", b)
	}
}

func TestApplyMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	if err := Apply(p, "x.ts.net"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !strings.HasPrefix(string(b), "nameserver 127.0.0.1") {
		t.Fatalf("got %q", b)
	}
}

func TestApplyUnchangedWritesNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	os.WriteFile(p, Rewrite([]byte(udhcpc), "x.ts.net"), 0o644)
	fi1, _ := os.Stat(p)
	os.Chtimes(p, fi1.ModTime().Add(-3600e9), fi1.ModTime().Add(-3600e9))
	fi1, _ = os.Stat(p)
	changed, err := ApplyChanged(p, "x.ts.net")
	fi2, _ := os.Stat(p)
	if err != nil || changed || !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatalf("changed %v err %v", changed, err)
	}
}

// A DHCP client writes the file in steps (emptied, then a line at a time, as udhcpc's default.script
// does): read mid-way, the rest would be lost when our lines go back on. A file changed within the
// settle time is read again once it has settled.
func TestAFileStillBeingWrittenIsReadOnceSettled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	os.WriteFile(path, nil, 0o644) // emptied
	go func() {
		time.Sleep(30 * time.Millisecond)
		f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
		f.WriteString("nameserver 10.0.0.1 # wlan0\n")
		time.Sleep(30 * time.Millisecond)
		f.WriteString("nameserver 10.0.0.2 # wlan0\n")
		f.Close()
	}()
	if _, err := ApplyChanged(path, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	b, _ := os.ReadFile(path)
	if got := string(b); got != "nameserver 127.0.0.1"+Mark+"\nnameserver 10.0.0.1 # wlan0\nnameserver 10.0.0.2 # wlan0\n" {
		t.Fatalf("%q", got)
	}
}
