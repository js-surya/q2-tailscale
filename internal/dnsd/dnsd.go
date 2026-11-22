// Package dnsd is the Q2's name server while Tailscale runs (127.0.0.1:53, first in resolv.conf): it
// answers the tailnet's MagicDNS names with the machines' Tailscale IPs and passes every other question
// to the Wi-Fi's own name servers.
package dnsd

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Table is the tailnet as the name server sees it: a machine's IPv4 by its full name (lower case, no
// trailing dot), and the MagicDNS suffix ("tail1234.ts.net"), "" while there is none (signed out).
type Table interface {
	Lookup(name string) (netip.Addr, bool)
	Suffix() string
}

// Server answers DNS on UDP and TCP. Upstream gives the name servers other questions go to, read
// afresh for each question (the DHCP client rewrites them); Timeout is each one's wait (2 s if 0).
type Server struct {
	Table    Table
	Upstream func() []netip.AddrPort
	Timeout  time.Duration
	// Forwarded, when set, hears after each forwarded question whether an upstream answered it.
	Forwarded func(ok bool)
}

const ttl = 60

// ListenAndServe serves on addr (UDP and TCP) until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return err
	}
	go func() { <-ctx.Done(); pc.Close(); l.Close() }()
	go s.serveTCP(l)
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			if out := s.answer(q); out != nil {
				pc.WriteTo(out, from)
			}
		}()
	}
}

func (s *Server) serveTCP(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			for {
				var n [2]byte
				if _, err := io.ReadFull(c, n[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(n[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				out := s.answer(q)
				if out == nil {
					return
				}
				c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...))
			}
		}()
	}
}

// answer is the reply to one question: the tailnet's own, or the upstream's; nil for a message that
// isn't a question.
func (s *Server) answer(q []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(q)
	if err != nil || h.Response {
		return nil
	}
	question, err := p.Question()
	if err != nil {
		return nil
	}
	name := strings.TrimSuffix(strings.ToLower(question.Name.String()), ".")
	suffix := ""
	if s.Table != nil {
		suffix = s.Table.Suffix()
	}
	if suffix != "" {
		if !strings.Contains(name, ".") && name != "" {
			name += "." + suffix // a short name, as MagicDNS takes it
		}
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return s.tailnet(h, question, name)
		}
	}
	out, err := s.forward(q)
	if s.Forwarded != nil {
		s.Forwarded(err == nil)
	}
	if err == nil {
		return out
	}
	return reply(h, question, dnsmessage.RCodeServerFailure, nil)
}

// tailnet answers a question about a name under the MagicDNS suffix: A from the table, NXDOMAIN for an
// unknown name, no answers for any other type (so a resolver asking AAAA too takes the A).
func (s *Server) tailnet(h dnsmessage.Header, q dnsmessage.Question, name string) []byte {
	ip, ok := s.Table.Lookup(name)
	if !ok || !ip.Is4() {
		return reply(h, q, dnsmessage.RCodeNameError, nil)
	}
	if q.Type != dnsmessage.TypeA {
		return reply(h, q, dnsmessage.RCodeSuccess, nil)
	}
	return reply(h, q, dnsmessage.RCodeSuccess, &ip)
}

func reply(h dnsmessage.Header, q dnsmessage.Question, rcode dnsmessage.RCode, a *netip.Addr) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: a != nil || rcode == dnsmessage.RCodeNameError,
		RecursionDesired: h.RecursionDesired, RecursionAvailable: true, RCode: rcode})
	b.EnableCompression()
	b.StartQuestions()
	b.Question(q)
	if a != nil {
		b.StartAnswers()
		b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: ttl}, dnsmessage.AResource{A: a.As4()})
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

var errNoUpstream = errors.New("dnsd: no upstream answered")

// forward sends q to each upstream in turn over UDP and returns the first answer with q's ID.
func (s *Server) forward(q []byte) ([]byte, error) {
	if s.Upstream == nil {
		return nil, errNoUpstream
	}
	wait := s.Timeout
	if wait == 0 {
		wait = 2 * time.Second
	}
	for _, up := range s.Upstream() {
		c, err := net.Dial("udp", up.String())
		if err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(wait))
		buf := make([]byte, 4096)
		if _, err = c.Write(q); err == nil {
			var n int
			for {
				if n, err = c.Read(buf); err != nil || (n >= 2 && buf[0] == q[0] && buf[1] == q[1]) {
					break
				}
			}
			if err == nil {
				c.Close()
				return buf[:n], nil
			}
		}
		c.Close()
	}
	return nil, errNoUpstream
}
