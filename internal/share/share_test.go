package share

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// A Q2's /tmp and card: an app's status file with its page's address, the launcher's pid file and its
// q2app with the name it shows.
type q2 struct{ tmp, card string }

func newQ2(t *testing.T) q2 {
	d := t.TempDir()
	q := q2{filepath.Join(d, "tmp"), filepath.Join(d, "mmc")}
	os.MkdirAll(q.tmp, 0o755)
	os.MkdirAll(q.card, 0o755)
	return q
}

func (q q2) app(id, name, state string, pid int) {
	os.MkdirAll(filepath.Join(q.card, "."+id), 0o755)
	if name != "" {
		os.WriteFile(filepath.Join(q.card, "."+id, "q2app"), []byte("name="+name+"\nqr=1\n"), 0o644)
	}
	os.WriteFile(filepath.Join(q.tmp, "q2-"+id+".state"), []byte(state), 0o644)
	if pid != 0 {
		os.WriteFile(filepath.Join(q.tmp, "q2-"+id), []byte(strconv.Itoa(pid)+"\n"), 0o644)
	}
}

func alive(pids ...int) func(int) bool {
	return func(p int) bool {
		for _, q := range pids {
			if p == q {
				return true
			}
		}
		return false
	}
}

// A running app's page is found by its status file's addr, named from its q2app.
func TestFindsRunningAppsWithPages(t *testing.T) {
	q := newQ2(t)
	q.app("eq-studio", "EQ Studio", "hold=1\naddr=192.168.1.20:8020\npin=123456\n", 41)
	q.app("filebrowser", "", "addr=192.168.1.20:8080\n", 42) // no q2app name: its id
	got := Running(q.tmp, q.card, alive(41, 42))
	want := []App{{ID: "eq-studio", Name: "EQ Studio", Port: 8020}, {ID: "filebrowser", Name: "filebrowser", Port: 8080}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
	}
}

// Left out: an app that has stopped (its pid gone, or no pid file), one without a page or Wi-Fi (no
// addr, or an empty one), Tailscale itself, ports the app uses itself, and a second app on a taken port.
func TestLeavesOut(t *testing.T) {
	q := newQ2(t)
	q.app("stopped", "Stopped", "addr=10.0.0.2:8001\n", 50)
	q.app("nopid", "No pid", "addr=10.0.0.2:8002\n", 0)
	q.app("nopage", "No page", "hold=1\n", 51)
	q.app("nowifi", "No Wi-Fi", "addr=\n", 52)
	q.app("tailscale", "Tailscale", "addr=10.0.0.2:8003\n", 53)
	q.app("dns", "DNS", "addr=10.0.0.2:53\n", 54)
	q.app("shell", "Shell", "addr=10.0.0.2:2302\n", 55)
	q.app("bad", "Bad", "addr=10.0.0.2:http\n", 56)
	q.app("a-first", "First", "addr=10.0.0.2:9000\n", 57)
	q.app("b-second", "Second", "addr=10.0.0.2:9000\n", 58)
	got := Running(q.tmp, q.card, alive(51, 52, 53, 54, 55, 56, 57, 58))
	if len(got) != 1 || got[0].ID != "a-first" {
		t.Fatalf("got %+v", got)
	}
}

type fakeTailnet struct {
	mu        sync.Mutex
	listeners map[string]net.Listener // ":8020" -> a local stand-in
}

func (f *fakeTailnet) listen(_, addr string) (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.listeners[addr] = l
	f.mu.Unlock()
	return l, nil
}

func (f *fakeTailnet) addr(port string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.listeners[":"+port]; l != nil {
		return l.Addr().String()
	}
	return ""
}

func waitFor(t *testing.T, what string, ok func() bool) {
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal(what)
}

// While an app runs its port is passed on to it, and taken back when it stops.
func TestManagerFollowsApps(t *testing.T) {
	page, _ := net.Listen("tcp", "127.0.0.1:0") // the app's page on the Q2
	defer page.Close()
	go func() {
		for {
			c, err := page.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			c.Write([]byte("page heard " + line))
			c.Close()
		}
	}()
	port := page.Addr().(*net.TCPAddr).Port
	var mu sync.Mutex
	apps := []App{{ID: "eq-studio", Name: "EQ Studio", Port: uint16(port)}}
	tn := &fakeTailnet{listeners: map[string]net.Listener{}}
	m := &Manager{Listen: tn.listen, Every: 20 * time.Millisecond, Target: "127.0.0.1",
		Scan: func() []App { mu.Lock(); defer mu.Unlock(); return apps }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	p := strconv.Itoa(port)
	waitFor(t, "not passed on", func() bool { return tn.addr(p) != "" })
	c, err := net.Dial("tcp", tn.addr(p))
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte("hi\n"))
	if got, _ := bufio.NewReader(c).ReadString('\n'); got != "page heard hi\n" {
		t.Fatalf("got %q", got)
	}
	c.Close()
	if s := m.Shared(); len(s) != 1 || s[0].Name != "EQ Studio" {
		t.Fatalf("shared %+v", s)
	}

	mu.Lock()
	apps = nil
	mu.Unlock()
	waitFor(t, "still shared", func() bool { return len(m.Shared()) == 0 })
	waitFor(t, "still listening", func() bool { _, err := net.Dial("tcp", tn.addr(p)); return err != nil })
}
