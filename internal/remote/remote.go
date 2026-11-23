// Package remote passes q2remote, Q2 Pod's developer shell, through the tailnet: a developer option,
// offered only on a developer firmware (the only one with q2remote) and off unless chosen. The app's
// Tailscale runs in userspace, so nothing from the tailnet reaches the Q2's own programs unless the
// app hands it on; this hands on q2remote's port, and nothing else. q2remote still asks for its key.
package remote

import (
	"context"
	"net"
	"os"

	"q2-tailscale/internal/forward"
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
	return (&forward.Forwarder{Listen: f.Listen, Port: Port, Target: f.Target, Name: "q2remote over the tailnet", Logf: f.Logf}).Run(ctx)
}
