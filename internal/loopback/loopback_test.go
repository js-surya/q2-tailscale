package loopback

import (
	"errors"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// fake records each ip command and answers "addr show" with show.
type fake struct {
	calls  [][]string
	show   string
	fail   map[string]bool
	denied bool // every add refused, as in a sandbox without CAP_NET_ADMIN
}

func (f *fake) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.fail[strings.Join(args, " ")] {
		return []byte("RTNETLINK answers: File exists"), errors.New("exit 2")
	}
	if f.denied && len(args) > 1 && args[0] == "addr" && args[1] == "add" {
		return []byte("ip: RTNETLINK answers: Operation not permitted"), errors.New("exit status 2")
	}
	if len(args) > 1 && args[0] == "-4" && args[1] == "addr" {
		return []byte(f.show), nil
	}
	return nil, nil
}

// busybox ip -4 addr show dev lo, with one address of ours left by a killed run
const show = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet 100.64.0.9/32 scope global lo:q2ts
       valid_lft forever preferred_lft forever
    inet 100.64.0.10/32 scope global lo
       valid_lft forever preferred_lft forever
`

func cmds(f *fake) []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func TestCleanupStale(t *testing.T) {
	f := &fake{show: show}
	m := &Manager{Run: f.run}
	if err := m.CleanupStale(); err != nil {
		t.Fatal(err)
	}
	want := []string{"-4 addr show dev lo", "addr del 100.64.0.9/32 dev lo"}
	if got := cmds(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got) // only the labelled one: another program's 100.64.0.10 is left alone
	}
}

func TestSyncAddsAndRemoves(t *testing.T) {
	f := &fake{}
	m := &Manager{Run: f.run}
	a, b, c := netip.MustParseAddr("100.101.1.2"), netip.MustParseAddr("100.101.1.3"), netip.MustParseAddr("100.101.1.4")
	if err := m.Sync([]netip.Addr{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync([]netip.Addr{b, c, b}); err != nil {
		t.Fatal(err)
	}
	got := cmds(f)
	want := []string{
		"addr add 100.101.1.2/32 dev lo label lo:q2ts", "addr add 100.101.1.3/32 dev lo label lo:q2ts",
		"addr add 100.101.1.4/32 dev lo label lo:q2ts", "addr del 100.101.1.2/32 dev lo",
	}
	sort.Strings(got[:2])
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if !reflect.DeepEqual(m.Have(), []netip.Addr{b, c}) {
		t.Fatalf("have %v", m.Have())
	}
}

func TestRejectsNonCGNAT(t *testing.T) {
	f := &fake{}
	m := &Manager{Run: f.run}
	err := m.Sync([]netip.Addr{netip.MustParseAddr("192.168.1.5"), netip.MustParseAddr("100.128.0.1"), netip.MustParseAddr("fd7a:115c:a1e0::1")})
	if err == nil || len(f.calls) != 0 {
		t.Fatalf("err %v, calls %q", err, cmds(f))
	}
}

func TestAddAlreadyThereCounts(t *testing.T) {
	// an address already on lo (added before, its del lost) is ours to keep and remove later
	a := netip.MustParseAddr("100.101.1.2")
	f := &fake{fail: map[string]bool{"addr add 100.101.1.2/32 dev lo label lo:q2ts": true}}
	m := &Manager{Run: f.run}
	if err := m.Sync([]netip.Addr{a}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Have(), []netip.Addr{a}) {
		t.Fatalf("have %v", m.Have())
	}
}

func TestCloseRemovesAll(t *testing.T) {
	f := &fake{}
	m := &Manager{Run: f.run}
	m.Sync([]netip.Addr{netip.MustParseAddr("100.101.1.2"), netip.MustParseAddr("100.101.1.3")})
	f.calls = nil
	m.Close()
	got := cmds(f)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"addr del 100.101.1.2/32 dev lo", "addr del 100.101.1.3/32 dev lo"}) || len(m.Have()) != 0 {
		t.Fatalf("got %q", got)
	}
}

// Adds that fail aren't tried again at every reconcile: each is a process the Q2 starts, and 52 of
// them every 3 s starved the name server (in the emulator, where every add is refused). After a
// failure the rest wait; the next try is 30 s on, doubling to 5 min, and a success starts over.
func TestFailedAddsBackOff(t *testing.T) {
	f := &fake{denied: true}
	now := time.Unix(1000, 0)
	m := &Manager{Run: f.run, Now: func() time.Time { return now }}
	want := []netip.Addr{netip.MustParseAddr("100.101.1.2"), netip.MustParseAddr("100.101.1.3"), netip.MustParseAddr("100.101.1.4")}
	adds := func() (n int) {
		for _, c := range f.calls {
			if c[0] == "addr" && c[1] == "add" {
				n++
			}
		}
		return
	}
	if err := m.Sync(want); err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("err %v", err)
	}
	if adds() != 1 {
		t.Fatalf("first sync: %d adds, want 1 (the rest wait)", adds())
	}
	now = now.Add(29 * time.Second)
	if err := m.Sync(want); err != nil || adds() != 1 { // nothing tried, nothing new to say
		t.Fatalf("within 30 s: err %v, %d adds", err, adds())
	}
	now = now.Add(2 * time.Second)
	m.Sync(want)
	if adds() != 2 {
		t.Fatalf("after 30 s: %d adds", adds())
	}
	now = now.Add(59 * time.Second) // the wait doubled to 60 s
	m.Sync(want)
	if adds() != 2 {
		t.Fatalf("within 60 s: %d adds", adds())
	}
	for i := 0; i < 10; i++ { // capped at 5 min
		now = now.Add(5*time.Minute + time.Second)
		m.Sync(want)
	}
	if adds() != 12 { // 2 before, then one per 5 min: without the cap the waits would pass 5 min
		t.Fatalf("capped: %d adds", adds())
	}
	f.denied = false // allowed now: all added at the next try, and the wait starts over
	now = now.Add(5*time.Minute + time.Second)
	if err := m.Sync(want); err != nil || len(m.Have()) != 3 {
		t.Fatalf("err %v have %v", err, m.Have())
	}
}

// A removed address needs no add, so it is never held back by a wait.
func TestRemovalNotHeldByBackOff(t *testing.T) {
	f := &fake{}
	now := time.Unix(1000, 0)
	m := &Manager{Run: f.run, Now: func() time.Time { return now }}
	a, b := netip.MustParseAddr("100.101.1.2"), netip.MustParseAddr("100.101.1.3")
	m.Sync([]netip.Addr{a})
	f.denied = true
	m.Sync([]netip.Addr{a, b}) // b refused: waiting
	m.Sync(nil)
	if len(m.Have()) != 0 {
		t.Fatalf("have %v", m.Have())
	}
}
