package dnsd

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type table struct {
	suffix string
	names  map[string]netip.Addr
}

func (t table) Lookup(name string) (netip.Addr, bool) { a, ok := t.names[name]; return a, ok }
func (t table) Suffix() string                        { return t.suffix }

var tail = table{"tail1234.ts.net", map[string]netip.Addr{"nas.tail1234.ts.net": netip.MustParseAddr("100.101.1.2")}}

// upstream is a fake resolver on 127.0.0.1 answering every A question with addr.
func upstream(t *testing.T, addr string) (netip.AddrPort, *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	hits := new(atomic.Int32)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, _ := p.Start(buf[:n])
			q, _ := p.Question()
			hits.Add(1)
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true})
			b.StartQuestions()
			b.Question(q)
			b.StartAnswers()
			b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 30},
				dnsmessage.AResource{A: netip.MustParseAddr(addr).As4()})
			out, _ := b.Finish()
			pc.WriteTo(out, from)
		}
	}()
	return netip.MustParseAddrPort(pc.LocalAddr().String()), hits
}

func start(t *testing.T, s *Server) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.ListenAndServe(ctx, addr)
	time.Sleep(50 * time.Millisecond)
	return addr
}

func query(t *testing.T, server, name string, typ dnsmessage.Type) dnsmessage.Message {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 42, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	c, err := net.Dial("udp", server)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write(q)
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf[:n]); err != nil {
		t.Fatal(err)
	}
	if m.Header.ID != 42 {
		t.Fatalf("id %d", m.Header.ID)
	}
	return m
}

func a(t *testing.T, m dnsmessage.Message) netip.Addr {
	t.Helper()
	if len(m.Answers) != 1 {
		t.Fatalf("%d answers, rcode %v", len(m.Answers), m.Header.RCode)
	}
	return netip.AddrFrom4(m.Answers[0].Body.(*dnsmessage.AResource).A)
}

func TestTailnetFQDN(t *testing.T) {
	srv := start(t, &Server{Table: tail, Upstream: func() []netip.AddrPort { return nil }})
	if got := a(t, query(t, srv, "NAS.tail1234.ts.net.", dnsmessage.TypeA)); got.String() != "100.101.1.2" {
		t.Fatal(got)
	}
}

func TestShortName(t *testing.T) {
	srv := start(t, &Server{Table: tail})
	if got := a(t, query(t, srv, "nas.", dnsmessage.TypeA)); got.String() != "100.101.1.2" {
		t.Fatal(got)
	}
}

func TestAAAAEmpty(t *testing.T) {
	srv := start(t, &Server{Table: tail})
	m := query(t, srv, "nas.tail1234.ts.net.", dnsmessage.TypeAAAA)
	if m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Fatalf("rcode %v, %d answers", m.Header.RCode, len(m.Answers))
	}
}

func TestUnknownTailnetNameNXDOMAIN(t *testing.T) {
	srv := start(t, &Server{Table: tail})
	t0 := time.Now()
	m := query(t, srv, "gone.tail1234.ts.net.", dnsmessage.TypeA)
	if m.Header.RCode != dnsmessage.RCodeNameError || time.Since(t0) > time.Second {
		t.Fatalf("rcode %v after %v", m.Header.RCode, time.Since(t0))
	}
}

func TestNonTailnetForwarded(t *testing.T) {
	up, hits := upstream(t, "93.184.216.34")
	srv := start(t, &Server{Table: tail, Upstream: func() []netip.AddrPort { return []netip.AddrPort{up} }})
	if got := a(t, query(t, srv, "example.com.", dnsmessage.TypeA)); got.String() != "93.184.216.34" || hits.Load() != 1 {
		t.Fatalf("%v, %d hits", got, hits.Load())
	}
}

func TestNotSignedInForwardsEverything(t *testing.T) {
	up, _ := upstream(t, "1.2.3.4")
	srv := start(t, &Server{Table: table{}, Upstream: func() []netip.AddrPort { return []netip.AddrPort{up} }})
	if got := a(t, query(t, srv, "nas.tail1234.ts.net.", dnsmessage.TypeA)); got.String() != "1.2.3.4" {
		t.Fatal(got)
	}
}

func TestUpstreamFollowsFile(t *testing.T) {
	up1, hits1 := upstream(t, "1.1.1.1")
	up2, hits2 := upstream(t, "2.2.2.2")
	cur := []netip.AddrPort{up1}
	var mu sync.Mutex
	srv := start(t, &Server{Table: tail, Upstream: func() []netip.AddrPort { mu.Lock(); defer mu.Unlock(); return cur }})
	a(t, query(t, srv, "one.example.", dnsmessage.TypeA))
	mu.Lock()
	cur = []netip.AddrPort{up2}
	mu.Unlock()
	if got := a(t, query(t, srv, "two.example.", dnsmessage.TypeA)); got.String() != "2.2.2.2" || hits1.Load() != 1 || hits2.Load() != 1 {
		t.Fatalf("%v, hits %d %d", got, hits1.Load(), hits2.Load())
	}
}

func TestDeadUpstreamFallsThroughThenServfail(t *testing.T) {
	dead := netip.MustParseAddrPort("127.0.0.1:9") // discard: nothing answers
	up, _ := upstream(t, "5.5.5.5")
	srv := start(t, &Server{Table: tail, Timeout: 200 * time.Millisecond,
		Upstream: func() []netip.AddrPort { return []netip.AddrPort{dead, up} }})
	if got := a(t, query(t, srv, "x.example.", dnsmessage.TypeA)); got.String() != "5.5.5.5" {
		t.Fatal(got)
	}
	srv2 := start(t, &Server{Table: tail, Timeout: 200 * time.Millisecond, Upstream: func() []netip.AddrPort { return []netip.AddrPort{dead} }})
	if m := query(t, srv2, "x.example.", dnsmessage.TypeA); m.Header.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("rcode %v", m.Header.RCode)
	}
}

func TestTCPQuery(t *testing.T) {
	srv := start(t, &Server{Table: tail})
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("nas.tail1234.ts.net."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	c, err := net.Dial("tcp", srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write(binary.BigEndian.AppendUint16(nil, uint16(len(q))))
	c.Write(q)
	var l [2]byte
	if _, err := c.Read(l[:]); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := net.Conn(c).Read(buf); err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf); err != nil || len(m.Answers) != 1 {
		t.Fatalf("%v, %+v", err, m.Answers)
	}
}

// Each forwarded question reports whether an upstream answered: the app's warning when the Wi-Fi's
// name servers don't (Tailscale's "DNS unavailable").
func TestForwardResultReported(t *testing.T) {
	dead := netip.MustParseAddrPort("127.0.0.1:9")
	up, _ := upstream(t, "5.5.5.5")
	var mu sync.Mutex
	var got []bool
	ups := []netip.AddrPort{dead}
	srv := start(t, &Server{Table: tail, Timeout: 200 * time.Millisecond,
		Upstream:  func() []netip.AddrPort { mu.Lock(); defer mu.Unlock(); return ups },
		Forwarded: func(ok bool) { mu.Lock(); got = append(got, ok); mu.Unlock() }})
	query(t, srv, "x.example.", dnsmessage.TypeA)
	mu.Lock()
	ups = []netip.AddrPort{up}
	mu.Unlock()
	query(t, srv, "y.example.", dnsmessage.TypeA)
	query(t, srv, "nas.tail1234.ts.net.", dnsmessage.TypeA) // the tailnet's own: not forwarded
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, []bool{false, true}) {
		t.Fatalf("reported %v", got)
	}
}
