package main

import (
	"bytes"
	"strings"
	"testing"
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
