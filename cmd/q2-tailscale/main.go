// Command q2-tailscale puts a Shanling Q2 running Q2 Pod on its owner's tailnet, as a card app: MagicDNS
// names and Tailscale IPs reach the tailnet's machines from any other card app on the Q2.
// See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"q2-tailscale/internal/app"
	"q2-tailscale/internal/config"
	"q2-tailscale/internal/keystore"
	"q2-tailscale/internal/loopback"
	"q2-tailscale/internal/node"
	"q2-tailscale/internal/pages"
	"q2-tailscale/internal/q2spot"
	"q2-tailscale/internal/relay"
	"q2-tailscale/internal/taildrop"

	"tailscale.com/envknob"
	"tailscale.com/ipn/store"
	"tailscale.com/logtail"
)

const version = "q2-v0.2"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is main without the process: the exit code it returns. 0 is a stop (TERM, INT, the page's Stop),
// which ends the launcher's loop; anything else is a failure it restarts.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("q2-tailscale", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", defaultDir(), "the app's card folder (.tailscale)")
	socket := fs.String("socket", "/tmp/q2-tailscale.browse.sock", "the page's Q2SPOT/1 socket")
	statePath := fs.String("state", "/tmp/q2-tailscale.state", "Q2 Pod's status file for the app")
	resolv := fs.String("resolv", "/etc/resolv.conf", "the resolver configuration to put our name server in")
	dns := fs.String("dns", "127.0.0.1:53", "where the name server listens")
	keysDir := fs.String("keys", "/mnt/data/apps/tailscale", "the Q2's internal storage for the login (keystore)")
	logPath := fs.String("log", "/tmp/q2-tailscale.log", "the log, capped at 256 KiB (one older copy kept)")
	tsDebug := fs.Bool("debug", false, "keep Tailscale's debug lines ([v1], [v2]) in the log")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}

	tuneMemory(os.Getenv, debug.SetGCPercent, debug.SetMemoryLimit)
	logf := cappedLog(*logPath, 256<<10)
	logf("%s starting in %s", version, *dir)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	settings := config.LoadSettings(*dir)
	setLogs(settings.Logs, logtail.Disable, envknob.SetNoLogsNoSupport)
	logf("logs to Tailscale %s", map[bool]string{true: "on", false: "off"}[settings.Logs])
	var again atomic.Bool // Send Logs flipped: start over once everything is undone
	stateDir := filepath.Join(*dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		logf("state folder: %v", err)
	}
	where, keysErr := keystore.Place(stateDir, *keysDir, filepath.Join(filepath.Dir(filepath.Dir(*keysDir)), "q2-tailscale"))
	if keysErr != nil {
		logf("login: %v", keysErr)
	} else if where.Moved {
		logf("login moved to %s", where.Path)
	}
	if where.Path == filepath.Join(*keysDir, keystore.File) { // a Remove takes the login too
		go func() {
			<-ctx.Done() // at the TERM itself: the launcher KILLs the app 2 s on
			if !again.Load() {
				startRemoveWatch(*dir, *keysDir, logf)
			}
		}()
	}
	keys, err := store.NewFileStore(logf, where.Path)
	if err != nil {
		logf("login store: %v", err)
		fmt.Fprintln(stderr, err)
		return 1
	}
	n := node.NewTSNet(stateDir, settings.Hostname, keys, tailscaleLogf(logf, *tsDebug))
	a := &app.App{Node: n, Loop: &loopback.Manager{}, Relay: &relay.Relay{Dial: n.Dial}, Dir: *dir,
		ResolvPath: *resolv, StatePath: *statePath, DNSAddr: *dns, Logf: logf}
	if keysErr != nil && where.Path != filepath.Join(*keysDir, keystore.File) {
		a.KeysOnCard = keysErr
	}
	go (&taildrop.Receiver{Src: n, Staged: n.Staged, Logf: logf, Got: a.Got,
		Dest: func() (string, error) { return config.SaveDir(*dir, config.LoadSettings(*dir)) }}).Run(ctx)
	go func() {
		h := pages.Handler(pages.Deps{App: a, Dir: *dir, Logout: n.Logout, Stop: stop, SetOn: a.SetOn, Version: version,
			Restart: func() { again.Store(true); stop() }})
		if err := q2spot.Serve(ctx, *socket, h); err != nil {
			logf("page socket: %v", err)
		}
	}()
	if err := a.Run(ctx); err != nil {
		logf("%v", err)
		fmt.Fprintln(stderr, err)
		return 1
	}
	os.Remove(*socket)
	logf("stopped")
	if again.Load() {
		logf("restarting")
		if err := restartSelf(os.Executable, syscall.Exec, os.Args); err != nil {
			logf("restart: %v", err)
			return 1 // the launcher starts it again
		}
	}
	return 0
}

// setLogs switches Tailscale's own logs off unless on: logtail's uploads and its buffers on the card
// (Disable) and what the node tells the tailnet about it (no-logs-no-support). A tailnet with network
// flow logs then won't run the node, which the page explains (node.LogsRequired).
func setLogs(on bool, disable, noLogsNoSupport func()) {
	if !on {
		disable()
		noLogsNoSupport()
	}
}

// restartSelf runs this program again in this process (exec), so the launcher's wait on its pid holds.
func restartSelf(executable func() (string, error), exec func(string, []string, []string) error, args []string) error {
	path, err := executable()
	if err != nil {
		return err
	}
	return exec(path, args, os.Environ())
}

// tuneMemory keeps the heap small on the Q2 (128 MB, no swap): collected at half its live size again
// (GOGC 50: Go's default 100 lets it double before a collection, the peaks after a transfer) and under a
// 16 MiB soft limit (the app holds about 12 MB; the limit is soft, so a bigger live heap is still
// allowed, collected more often). GOGC or GOMEMLIMIT in the environment wins.
func tuneMemory(getenv func(string) string, setGC func(int) int, setLimit func(int64) int64) {
	if getenv("GOGC") == "" {
		setGC(50)
	}
	if getenv("GOMEMLIMIT") == "" {
		setLimit(16 << 20)
	}
}

// tailscaleLogf is the logf Tailscale gets: its debug lines left out (dropDebug) unless debug.
func tailscaleLogf(logf func(string, ...any), debug bool) func(string, ...any) {
	if debug {
		return logf
	}
	return dropDebug(logf)
}

// dropDebug passes on what Tailscale logs but its debug lines: those whose text, past any "name: "
// prefixes (wg: , magicsock: ), starts [v1] or [v2]. About 250 lines an hour on the Q2, each a wake
// and a write, and they bury the lines that matter.
func dropDebug(logf func(string, ...any)) func(string, ...any) {
	return func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		for {
			if strings.HasPrefix(s, "[v1] ") || strings.HasPrefix(s, "[v2] ") {
				return
			}
			name, rest, ok := strings.Cut(s, ": ")
			if !ok || name == "" || strings.ContainsAny(name, " []") {
				break
			}
			s = rest
		}
		logf("%s", fmt.Sprintf(format, args...))
	}
}

// cappedLog writes timestamped lines to path; past max bytes the file becomes path.1 and starts over.
func cappedLog(path string, max int64) func(string, ...any) {
	var mu sync.Mutex
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if fi, err := os.Stat(path); err == nil && fi.Size() > max {
			os.Rename(path, path+".1")
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			return
		}
		fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
		f.Close()
	}
}

// defaultDir is the folder the binary is in: the card app's own folder.
func defaultDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
