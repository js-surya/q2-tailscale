package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// buildTags reads contrib/shanlingq2/build-tags as build.sh does: # starts a comment, a word is a tag.
func buildTags(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../contrib/shanlingq2/build-tags")
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, line := range strings.Split(string(b), "\n") {
		line, _, _ = strings.Cut(line, "#")
		tags = append(tags, strings.Fields(line)...)
	}
	return tags
}

// The release build asks no router for a port (UPnP, NAT-PMP, PCP): those probes wake the Q2's Wi-Fi
// for a mapping a hotspot never gives; hole punching and DERP connect without it.
func TestNoPortMapperInTheBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	tags := strings.Join(buildTags(t), ",")
	cmd := exec.Command("go", "list", "-deps", "-tags", tags, ".")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=mipsle", "GOMIPS=softfloat", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, p := range strings.Fields(string(out)) {
		if p == "tailscale.com/net/portmapper" || p == "tailscale.com/feature/portmapper" {
			t.Errorf("the build has %s", p)
		}
	}
}
