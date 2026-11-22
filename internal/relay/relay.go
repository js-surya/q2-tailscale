// Package relay listens on local addresses (a tailnet machine's IP on lo, or 127.0.0.1 for a forward) and
// pipes each connection to its target through the tailnet (tsnet's Dial), byte for byte: TLS stays end to
// end between the program and the machine. Only the Q2's own programs are relayed: Linux takes a packet
// for any of its addresses on any interface, so a device on the same Wi-Fi could route a tailnet IP
// through the Q2 and reach the tailnet as the Q2; a connection from an address not the Q2's is closed.
package relay

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Dialer opens a connection through the tailnet: tsnet.Server.Dial.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Target is one listener: connections to Listen go to Dial.
type Target struct{ Listen, Dial netip.AddrPort }

const dialTimeout = 10 * time.Second

type listener struct {
	l     net.Listener
	mu    sync.Mutex
	dial  netip.AddrPort
	conns map[net.Conn]bool
}

// Relay keeps a set of listeners. Own says whether a connection's source is the Q2 itself; nil is
// ownAddr.
type Relay struct {
	Dial Dialer
	Own  func(netip.Addr) bool
	mu   sync.Mutex
	ls   map[netip.AddrPort]*listener
}

// Sync makes the listeners exactly targets: missing ones opened, removed ones closed with their open
// connections, a changed target taken by the next connection. It returns the listeners that couldn't
// open (a port another program holds), by address; the rest work regardless, and a later Sync tries
// those again.
func (r *Relay) Sync(targets []Target) map[netip.AddrPort]error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ls == nil {
		r.ls = map[netip.AddrPort]*listener{}
	}
	want := map[netip.AddrPort]netip.AddrPort{}
	for _, t := range targets {
		want[t.Listen] = t.Dial
	}
	for addr, l := range r.ls {
		if _, ok := want[addr]; !ok {
			l.close()
			delete(r.ls, addr)
		}
	}
	errs := map[netip.AddrPort]error{}
	for addr, dial := range want {
		if l := r.ls[addr]; l != nil {
			l.mu.Lock()
			l.dial = dial
			l.mu.Unlock()
			continue
		}
		nl, err := net.Listen("tcp4", addr.String())
		if err != nil {
			errs[addr] = err
			continue
		}
		l := &listener{l: nl, dial: dial, conns: map[net.Conn]bool{}}
		r.ls[addr] = l
		go r.accept(l)
	}
	return errs
}

// Close closes every listener and connection.
func (r *Relay) Close() { r.Sync(nil) }

func (r *Relay) accept(l *listener) {
	for {
		c, err := l.l.Accept()
		if err != nil {
			return
		}
		l.mu.Lock()
		if l.conns == nil { // closed meanwhile
			l.mu.Unlock()
			c.Close()
			return
		}
		if !r.own(c) {
			l.mu.Unlock()
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetLinger(0) // a reset
			}
			c.Close()
			continue
		}
		l.conns[c] = true
		dial := l.dial
		l.mu.Unlock()
		go func() {
			r.pipe(c, dial)
			l.mu.Lock()
			delete(l.conns, c)
			l.mu.Unlock()
		}()
	}
}

// own: c comes from the Q2 itself.
func (r *Relay) own(c net.Conn) bool {
	ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return false
	}
	if r.Own != nil {
		return r.Own(ap.Addr().Unmap())
	}
	return ownAddr(ap.Addr().Unmap())
}

// ownAddr: a is loopback or one of this host's interface addresses. A program on the Q2 connecting to a
// tailnet IP on lo comes from that IP (or another of the Q2's); a packet from the network can't carry
// one, as Linux drops a packet arriving with a local source address.
func ownAddr(a netip.Addr) bool {
	if a.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, x := range addrs {
		if p, err := netip.ParsePrefix(x.String()); err == nil && p.Addr().Unmap() == a {
			return true
		}
	}
	return false
}

func (l *listener) close() {
	l.l.Close()
	l.mu.Lock()
	for c := range l.conns {
		c.Close()
	}
	l.conns = nil
	l.mu.Unlock()
}

type closeWriter interface{ CloseWrite() error }

// pipe connects c to dial, copying both ways; each side's end of input half-closes the other.
func (r *Relay) pipe(c net.Conn, dial netip.AddrPort) {
	defer c.Close()
	r.mu.Lock()
	d := r.Dial
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	up, err := d(ctx, "tcp", dial.String())
	cancel()
	if err != nil {
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetLinger(0) // a reset, so the program fails at once
		}
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	half := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go half(up, c)
	go half(c, up)
	<-done
	<-done
}
