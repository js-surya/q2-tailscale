package node

import (
	"net/netip"
	"reflect"
	"testing"

	"tailscale.com/health"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

func TestPeersFromStatus(t *testing.T) {
	k1, k2, k3 := key.NewNode().Public(), key.NewNode().Public(), key.NewNode().Public()
	st := &ipnstate.Status{
		BackendState: "Running",
		Self: &ipnstate.PeerStatus{HostName: "q2-pod", DNSName: "q2-pod.tail1234.ts.net.",
			TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.5"), netip.MustParseAddr("fd7a:115c:a1e0::5")}},
		CurrentTailnet: &ipnstate.TailnetStatus{MagicDNSSuffix: "tail1234.ts.net"},
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			k1: {HostName: "NAS box", DNSName: "nas.tail1234.ts.net.", Online: true,
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::2"), netip.MustParseAddr("100.101.1.2")}},
			k2: {HostName: "laptop", DNSName: "Laptop.tail1234.ts.net.", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.101.1.3")}},
			k3: {HostName: "v6only", DNSName: "v6only.tail1234.ts.net.", TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::9")}},
		},
	}
	got := FromIPN(st)
	want := Status{State: "Running", Self: "q2-pod", SelfIP: netip.MustParseAddr("100.64.0.5"), Suffix: "tail1234.ts.net",
		Peers: []Peer{
			{Name: "laptop", FQDN: "laptop.tail1234.ts.net", IP: netip.MustParseAddr("100.101.1.3")},
			{Name: "nas", FQDN: "nas.tail1234.ts.net", IP: netip.MustParseAddr("100.101.1.2"), Online: true},
		}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestNeedsLoginCarriesURL(t *testing.T) {
	got := FromIPN(&ipnstate.Status{BackendState: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/abc"})
	if got.State != "NeedsLogin" || got.AuthURL != "https://login.tailscale.com/a/abc" || len(got.Peers) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestSelfIsTheTailnetNameAfterARenameInTheConsole(t *testing.T) {
	for _, c := range []struct{ host, dns, want string }{
		{"q2-pod", "aw-pod.tail1234.ts.net.", "aw-pod"}, // renamed in the admin console
		{"q2-pod", "Q2-Pod.tail1234.ts.net.", "q2-pod"}, // as MagicDNS has it, lower case
		{"q2-pod", "", "q2-pod"},                        // no tailnet name yet: its own
	} {
		got := FromIPN(&ipnstate.Status{BackendState: "Running", Self: &ipnstate.PeerStatus{HostName: c.host, DNSName: c.dns}})
		if got.Self != c.want {
			t.Errorf("HostName %q DNSName %q: Self %q, want %q", c.host, c.dns, got.Self, c.want)
		}
	}
}

func TestNilStatus(t *testing.T) {
	if got := FromIPN(nil); got.State != "Starting" {
		t.Fatalf("got %+v", got)
	}
}

func TestServicesFromDNSRecords(t *testing.T) {
	peers := []Peer{{Name: "nas", FQDN: "nas.tail1234.ts.net", IP: netip.MustParseAddr("100.101.1.2")}}
	recs := []tailcfg.DNSRecord{
		{Name: "navidrome.tail1234.ts.net.", Value: "100.109.4.217"},       // a Tailscale Service
		{Name: "Jelly.tail1234.ts.net", Type: "A", Value: "100.109.4.218"}, // A, no trailing dot
		{Name: "jelly6.tail1234.ts.net.", Type: "AAAA", Value: "fd7a::1"},  // IPv6 only: the Q2 has none
		{Name: "nas.tail1234.ts.net.", Value: "100.101.1.2"},               // a machine, already a peer
		{Name: "router.lan.", Value: "192.168.1.1"},                        // not a tailnet address
		{Name: "bad.tail1234.ts.net.", Value: "nope"},
	}
	got := Services(recs, peers)
	want := []Peer{
		{Name: "jelly", FQDN: "jelly.tail1234.ts.net", IP: netip.MustParseAddr("100.109.4.218"), Online: true},
		{Name: "navidrome", FQDN: "navidrome.tail1234.ts.net", IP: netip.MustParseAddr("100.109.4.217"), Online: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// Tailscale's health warnings as its apps show them: the ones that matter while connected, the most
// severe first. Starting up and turned off are states the page shows already, and a low one is noise.
func TestWarningsFromHealth(t *testing.T) {
	st := &health.State{Warnings: map[health.WarnableCode]health.UnhealthyState{
		"warming-up":        {WarnableCode: "warming-up", Severity: health.SeverityLow, Title: "Tailscale is starting"},
		"wantrunning-false": {WarnableCode: "wantrunning-false", Severity: health.SeverityLow, Title: "Tailscale is stopped"},
		"dns-read-os-config-failed": {WarnableCode: "dns-read-os-config-failed", Severity: health.SeverityMedium,
			Title: "Failed to read system DNS configuration", Text: "Tailscale failed to fetch the DNS configuration."},
		"no-derp-connection": {WarnableCode: "no-derp-connection", Severity: health.SeverityHigh,
			Title: "Relay server unavailable", Text: "Tailscale could not connect to any relay server."},
		"update-available": {WarnableCode: "update-available", Severity: health.SeverityLow, Title: "Update available"},
	}}
	got := Warnings(st)
	want := []Warning{{Title: "Relay server unavailable", Text: "Tailscale could not connect to any relay server."},
		{Title: "Failed to read system DNS configuration", Text: "Tailscale failed to fetch the DNS configuration."}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if Warnings(nil) != nil || Warnings(&health.State{}) != nil {
		t.Fatal("healthy: warnings")
	}
}

// A tailnet with network flow logs won't run a node that sends none: Tailscale says so in a low-severity
// local-log warning and stops the node, which LogsRequired picks out.
func TestATailnetThatRequiresLogsIsNoticed(t *testing.T) {
	st := &health.State{Warnings: map[health.WarnableCode]health.UnhealthyState{
		"local-log-config-error": {WarnableCode: "local-log-config-error", Severity: health.SeverityLow,
			Title: "Local log misconfiguration", Text: "The local log is misconfigured: tailnet requires logging to be enabled. Remove --no-logs-no-support from tailscaled command line."},
	}}
	if !LogsRequired(st) {
		t.Fatal("not noticed")
	}
	if LogsRequired(nil) || LogsRequired(&health.State{Warnings: map[health.WarnableCode]health.UnhealthyState{
		"local-log-config-error": {WarnableCode: "local-log-config-error", Text: "The local log is misconfigured: disk full"}}}) {
		t.Fatal("noticed without the tailnet asking")
	}
}
