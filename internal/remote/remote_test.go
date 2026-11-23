package remote

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"testing"
	"time"
)

// Only a developer firmware has q2remote: the option is offered there and nowhere else.
func TestAvailableOnlyWithQ2remote(t *testing.T) {
	if !Available(func(string) (os.FileInfo, error) { return nil, nil }) {
		t.Error("q2remote there: not offered")
	}
	if Available(func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist }) {
		t.Error("no q2remote: offered")
	}
}

// What comes in on the tailnet's port reaches q2remote and its answer goes back.
func TestForwardsToQ2remote(t *testing.T) {
	target, _ := net.Listen("tcp", "127.0.0.1:0")
	defer target.Close()
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		c.Write([]byte("q2remote heard " + line))
	}()
	front, _ := net.Listen("tcp", "127.0.0.1:0") // stands in for the tailnet's listener
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Forwarder{Listen: func(string, string) (net.Listener, error) { return front, nil }, Target: target.Addr().String()}).Run(ctx)
	c, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte("hello\n"))
	if got, _ := bufio.NewReader(c).ReadString('\n'); got != "q2remote heard hello\n" {
		t.Fatalf("got %q", got)
	}
}

// Stopping closes the tailnet's listener; a listener that won't open is reported, not fatal.
func TestStopsAndReports(t *testing.T) {
	front, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		done <- (&Forwarder{Listen: func(string, string) (net.Listener, error) { return front, nil }, Target: "127.0.0.1:1"}).Run(ctx)
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
	err := (&Forwarder{Listen: func(string, string) (net.Listener, error) { return nil, errors.New("no") }}).Run(context.Background())
	if err == nil {
		t.Fatal("no error")
	}
}
