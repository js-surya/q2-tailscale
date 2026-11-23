package forward

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func echoTarget(t *testing.T) net.Listener {
	target, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { target.Close() })
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				c.Write([]byte("heard " + line))
			}()
		}
	}()
	return target
}

func ask(t *testing.T, addr string) string {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte("hello\n"))
	got, _ := bufio.NewReader(c).ReadString('\n')
	return got
}

// What comes in on the port reaches the target and its answer goes back, on the port asked for.
func TestForwardsOnItsPort(t *testing.T) {
	target := echoTarget(t)
	front, _ := net.Listen("tcp", "127.0.0.1:0")
	var asked string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Forwarder{Port: "8020", Target: target.Addr().String(),
		Listen: func(_, addr string) (net.Listener, error) { asked = addr; return front, nil }}).Run(ctx)
	if got := ask(t, front.Addr().String()); got != "heard hello\n" {
		t.Fatalf("got %q", got)
	}
	if asked != ":8020" {
		t.Fatalf("listened on %q", asked)
	}
}

// A connection Allow refuses is closed before the target hears anything.
func TestAllowRefuses(t *testing.T) {
	target := echoTarget(t)
	front, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Forwarder{Port: "1", Target: target.Addr().String(), Allow: func(context.Context, net.Conn) bool { return false },
		Listen: func(string, string) (net.Listener, error) { return front, nil }}).Run(ctx)
	if got := ask(t, front.Addr().String()); got != "" {
		t.Fatalf("refused connection answered %q", got)
	}
}

// Stopping closes the listener; one that won't open is reported.
func TestStopsAndReports(t *testing.T) {
	front, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		done <- (&Forwarder{Port: "1", Target: "127.0.0.1:1", Listen: func(string, string) (net.Listener, error) { return front, nil }}).Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("still running")
	}
	if _, err := net.Dial("tcp", front.Addr().String()); err == nil {
		t.Fatal("listener still open")
	}
	if (&Forwarder{Port: "1", Listen: func(string, string) (net.Listener, error) { return nil, errors.New("no") }}).Run(context.Background()) == nil {
		t.Fatal("no error")
	}
}
