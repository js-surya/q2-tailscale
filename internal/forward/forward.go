// Package forward hands each connection on one of the node's tailnet ports to a program on the Q2. The
// app's Tailscale runs in userspace, so nothing from the tailnet reaches the Q2's own programs unless the
// app hands it on: q2remote's port (package remote) and the pages of running card apps (package share).
package forward

import (
	"context"
	"io"
	"net"
	"sync"
)

// Forwarder listens on Port (Listen: tsnet's, plain or HTTPS) and pipes each connection Allow lets in
// (nil: every one) to Target. Name begins its log lines.
type Forwarder struct {
	Listen func(network, addr string) (net.Listener, error)
	Port   string
	Target string
	Allow  func(context.Context, net.Conn) bool
	Name   string
	Logf   func(string, ...any)
}

// Run listens until ctx ends; the error is the listener's, when it couldn't open.
func (f *Forwarder) Run(ctx context.Context) error {
	l, err := f.Listen("tcp", ":"+f.Port)
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

func (f *Forwarder) logf(format string, a ...any) {
	if f.Logf != nil {
		f.Logf(f.Name+": "+format, a...)
	}
}

func (f *Forwarder) pipe(ctx context.Context, c net.Conn) {
	defer c.Close()
	if f.Allow != nil && !f.Allow(ctx, c) {
		f.logf("refused %v", c.RemoteAddr())
		return
	}
	var d net.Dialer
	up, err := d.DialContext(ctx, "tcp", f.Target)
	if err != nil {
		f.logf("%v", err)
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
