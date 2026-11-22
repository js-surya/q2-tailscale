package relay

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

// echo is a server that echoes each line, and says "bye" when its client half-closes.
func echo(t *testing.T) netip.AddrPort {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err == io.EOF {
						c.Write([]byte("bye\n"))
						return
					}
					if err != nil {
						return
					}
					c.Write([]byte(line))
				}
			}()
		}
	}()
	return netip.MustParseAddrPort(l.Addr().String())
}

func freePort(t *testing.T) netip.AddrPort {
	t.Helper()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return netip.MustParseAddrPort(l.Addr().String())
}

func dialer(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func TestRelayEchoes(t *testing.T) {
	r := &Relay{Dial: dialer}
	defer r.Close()
	srv, local := echo(t), freePort(t)
	if errs := r.Sync([]Target{{Listen: local, Dial: srv}}); len(errs) != 0 {
		t.Fatal(errs)
	}
	c, err := net.Dial("tcp", local.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hello\n"))
	line, _ := bufio.NewReader(c).ReadString('\n')
	if line != "hello\n" {
		t.Fatalf("got %q", line)
	}
}

func TestHalfClose(t *testing.T) {
	r := &Relay{Dial: dialer}
	defer r.Close()
	srv, local := echo(t), freePort(t)
	r.Sync([]Target{{Listen: local, Dial: srv}})
	c, _ := net.Dial("tcp", local.String())
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("a\n"))
	c.(*net.TCPConn).CloseWrite()
	b, _ := io.ReadAll(c)
	if string(b) != "a\nbye\n" {
		t.Fatalf("got %q", b)
	}
}

func TestSyncClosesRemoved(t *testing.T) {
	r := &Relay{Dial: dialer}
	defer r.Close()
	srv, local := echo(t), freePort(t)
	r.Sync([]Target{{Listen: local, Dial: srv}})
	open, err := net.Dial("tcp", local.String())
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	open.Write([]byte("x\n"))
	bufio.NewReader(open).ReadString('\n')
	r.Sync(nil)
	if _, err := net.DialTimeout("tcp", local.String(), time.Second); err == nil {
		t.Fatal("still listening")
	}
	open.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := open.Read(make([]byte, 8)); err == nil || os.IsTimeout(err) {
		t.Fatalf("an open connection outlived its listener: %v", err)
	}
}

func TestSyncChangesTarget(t *testing.T) {
	r := &Relay{Dial: dialer}
	defer r.Close()
	srv1, srv2, local := echo(t), echo(t), freePort(t)
	r.Sync([]Target{{Listen: local, Dial: srv1}})
	var dialed []string
	r.Dial = func(ctx context.Context, n, a string) (net.Conn, error) {
		dialed = append(dialed, a)
		return dialer(ctx, n, a)
	}
	r.Sync([]Target{{Listen: local, Dial: srv2}})
	c, _ := net.Dial("tcp", local.String())
	c.Write([]byte("z\n"))
	bufio.NewReader(c).ReadString('\n')
	c.Close()
	if len(dialed) != 1 || dialed[0] != srv2.String() {
		t.Fatalf("dialed %v", dialed)
	}
}

func TestBindFailureReported(t *testing.T) {
	r := &Relay{Dial: dialer}
	defer r.Close()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	busy := netip.MustParseAddrPort(taken.Addr().String())
	srv, ok := echo(t), freePort(t)
	errs := r.Sync([]Target{{Listen: busy, Dial: srv}, {Listen: ok, Dial: srv}})
	if errs[busy] == nil || errs[ok] != nil || len(errs) != 1 {
		t.Fatalf("errs %v", errs)
	}
	c, err := net.Dial("tcp", ok.String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	// the port freed later: the next Sync takes it
	taken.Close()
	if errs := r.Sync([]Target{{Listen: busy, Dial: srv}, {Listen: ok, Dial: srv}}); len(errs) != 0 {
		t.Fatalf("errs %v", errs)
	}
}

func TestDialFailureClosesFast(t *testing.T) {
	r := &Relay{Dial: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no route") }}
	defer r.Close()
	local := freePort(t)
	r.Sync([]Target{{Listen: local, Dial: netip.MustParseAddrPort("100.101.1.2:443")}})
	c, _ := net.Dial("tcp", local.String())
	defer c.Close()
	t0 := time.Now()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := c.Read(make([]byte, 1))
	if err == nil || os.IsTimeout(err) || time.Since(t0) > time.Second {
		t.Fatalf("%v after %v", err, time.Since(t0))
	}
}
