// Package remote passes q2remote, Q2 Pod's developer shell, through the tailnet: a developer option,
// offered only on a developer firmware (the only one with q2remote) and off unless chosen. The app's
// Tailscale runs in userspace, so nothing from the tailnet reaches the Q2's own programs unless the
// app hands it on; this hands on q2remote's port, and nothing else. q2remote still asks for its key.
package remote

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
)

// Binary is q2remote: developer firmware builds have it, release builds never do.
const Binary = "/usr/bin/q2remote"

// Port is q2remote's, on the Q2 and on the tailnet.
const Port = "2302"

// Available is whether this firmware is a developer build: q2remote there.
func Available(stat func(string) (os.FileInfo, error)) bool {
	_, err := stat(Binary)
	return err == nil
}

// Forwarder hands each connection on the tailnet's port (Listen: tsnet's) to q2remote on the Q2 (Target).
type Forwarder struct {
	Listen func(network, addr string) (net.Listener, error)
	Target string
	Logf   func(string, ...any)
}

// Run listens until ctx ends; the error is the listener's, when it couldn't open.
func (f *Forwarder) Run(ctx context.Context) error {
	l, err := f.Listen("tcp", ":"+Port)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			return nil
		}
		go f.pipe(ctx, c)
	}
}

func (f *Forwarder) pipe(ctx context.Context, c net.Conn) {
	defer c.Close()
	var d net.Dialer
	up, err := d.DialContext(ctx, "tcp", f.Target)
	if err != nil {
		if f.Logf != nil {
			f.Logf("q2remote over the tailnet: %v", err)
		}
		return
	}
	defer up.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	half := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go half(up, c)
	go half(c, up)
	wg.Wait()
}
