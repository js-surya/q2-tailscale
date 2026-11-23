package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"q2-tailscale/internal/node"
	"q2-tailscale/internal/share"
)

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != version {
		t.Fatalf("printed %q, want %q", got, version)
	}
}

func TestUnknownFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

// The Q2 has 128 MB and no swap: the heap is collected at half its live size again (GOGC 50, not 100)
// and kept under a 16 MiB soft limit, unless GOGC or GOMEMLIMIT says otherwise.
func TestMemoryTuned(t *testing.T) {
	var gc []int
	var limit []int64
	set := func(env map[string]string) {
		gc, limit = nil, nil
		tuneMemory(func(k string) string { return env[k] },
			func(p int) int { gc = append(gc, p); return 100 },
			func(l int64) int64 { limit = append(limit, l); return 0 })
	}
	set(nil)
	if len(gc) != 1 || gc[0] != 50 || len(limit) != 1 || limit[0] != 16<<20 {
		t.Fatalf("gc %v limit %v", gc, limit)
	}
	set(map[string]string{"GOGC": "200"})
	if len(gc) != 0 || len(limit) != 1 {
		t.Fatalf("GOGC set: gc %v limit %v", gc, limit)
	}
	set(map[string]string{"GOMEMLIMIT": "40MiB"})
	if len(gc) != 1 || len(limit) != 0 {
		t.Fatalf("GOMEMLIMIT set: gc %v limit %v", gc, limit)
	}
}

// Tailscale's logs are switched off at the start, uploads and the card's buffers both, unless Send Logs
// is on.
func TestLogsOffUnlessChosen(t *testing.T) {
	var disabled, noSupport int
	setLogs(false, func() { disabled++ }, func() { noSupport++ })
	if disabled != 1 || noSupport != 1 {
		t.Fatalf("off: disable %d, no-logs %d", disabled, noSupport)
	}
	setLogs(true, func() { disabled++ }, func() { noSupport++ })
	if disabled != 1 || noSupport != 1 {
		t.Fatalf("on: disable %d, no-logs %d", disabled, noSupport)
	}
}

// A restart starts the same program again in this process, so the launcher still waits on its pid.
func TestRestartRunsItselfAgain(t *testing.T) {
	var got string
	var gotArgs []string
	err := restartSelf(func() (string, error) { return "/mnt/mmc/.tailscale/q2-tailscale", nil },
		func(path string, args, env []string) error { got, gotArgs = path, args; return nil },
		[]string{"q2-tailscale", "-dir", "/mnt/mmc/.tailscale"})
	if err != nil || got != "/mnt/mmc/.tailscale/q2-tailscale" || len(gotArgs) != 3 || gotArgs[2] != "/mnt/mmc/.tailscale" {
		t.Fatalf("%v %q %q", err, got, gotArgs)
	}
}

// Tailscale's debug lines ([v1], [v2], after any "name: " prefixes) stay out of the log unless asked
// for; everything else is kept.
func TestDebugLinesLeftOut(t *testing.T) {
	var got []string
	logf := func(format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }
	quiet := dropDebug(logf)
	for _, l := range []string{
		"[v1] using fake (no-op) tun device",
		"wg: [v2] Routine: receive incoming receiveDERP - stopped",
		"magicsock: [v1] derp-1 does not know about peer, removing",
	} {
		quiet("%s", l)
	}
	keep := []string{
		"magicsock: derp-1 connected",
		"health(warnable=warming-up): error: Tailscale is starting. Please wait.",
		"[unexpected] something odd",
		"peer [v1] in the middle stays",
	}
	for _, l := range keep {
		quiet("%s", l)
	}
	quiet("[v%d] formatted %s", 1, "too")
	if strings.Join(got, "\n") != strings.Join(keep, "\n") {
		t.Fatalf("logged:\n%s", strings.Join(got, "\n"))
	}
}

func TestDebugFlagKeepsThem(t *testing.T) {
	var got []string
	logf := func(format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }
	tailscaleLogf(logf, true)("[v1] kept")
	tailscaleLogf(logf, false)("[v1] dropped")
	if len(got) != 1 || got[0] != "[v1] kept" {
		t.Fatalf("logged %q", got)
	}
}

// Apps Shared Now: each app at the node's Tailscale IP over HTTP, or its HTTPS name once HTTPS is in
// use, and the note when HTTPS was chosen but couldn't be used. HTTPS is used only with the certificate.
func TestSharedAppsAddresses(t *testing.T) {
	st := node.Status{State: "Running", SelfIP: netip.MustParseAddr("100.64.0.5"), CertDomain: "q2-pod.tail1234.ts.net"}
	apps := []share.App{{ID: "eq-studio", Name: "EQ Studio", Port: 8020}}
	httpsChosen.Store(false)
	if got, note := sharedApps(st, apps, ""); got[0].Address != "http://100.64.0.5:8020" || note != "" {
		t.Fatalf("%+v %q", got, note)
	}
	if note := httpsReady(context.Background(), "", nil); !strings.Contains(note, "HTTPS is off") || httpsChosen.Load() {
		t.Fatalf("%q", note)
	}
	failing := func(context.Context, string) error { return errors.New("x509: certificate signed by unknown authority") }
	if note := httpsReady(context.Background(), st.CertDomain, failing); !strings.Contains(note, "plain HTTP") || httpsChosen.Load() {
		t.Fatalf("%q", note)
	}
	if got, note := sharedApps(st, apps, "Couldn't get the HTTPS certificate"); got[0].Address != "http://100.64.0.5:8020" || note == "" {
		t.Fatalf("%+v %q", got, note)
	}
	if note := httpsReady(context.Background(), st.CertDomain, func(context.Context, string) error { return nil }); note != "" || !httpsChosen.Load() {
		t.Fatalf("%q", note)
	}
	if got, _ := sharedApps(st, apps, ""); got[0].Address != "https://q2-pod.tail1234.ts.net:8020" {
		t.Fatalf("%+v", got)
	}
	if _, note := sharedApps(node.Status{State: "Starting"}, apps, ""); note != "Not connected yet" {
		t.Fatalf("%q", note)
	}
}
