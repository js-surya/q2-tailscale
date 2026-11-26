// Command q2-tailscale puts a Shanling Q2 running Q2 Pod on its owner's tailnet, as a card app: MagicDNS
// names and Tailscale IPs reach the tailnet's machines from any other card app on the Q2.
// See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
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
	"q2-tailscale/internal/remote"
	"q2-tailscale/internal/secrets"
	"q2-tailscale/internal/share"
	"q2-tailscale/internal/taildrop"

	_ "golang.org/x/crypto/x509roots/fallback" // the Q2 has no root certificates: HTTPS's certificate comes from Let's Encrypt

	"tailscale.com/envknob"
	"tailscale.com/ipn/store"
	"tailscale.com/logtail"
)

const version = "q2-v0.9"

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
	// The certificate and the log key: in the Vault when there is one (Tailscale's folder then in RAM),
	// else on the card as before; the login stays in internal storage either way.
	sec := &secrets.Keeper{Vault: secrets.Client{}, Card: stateDir, Keys: *keysDir, Tmp: filepath.Dir(*statePath),
		Logf: logf, Installed: secrets.VaultInstalled, Wait: 20 * time.Second, Sleep: time.Sleep}
	tsDir := sec.Start()
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sec.Sync()
			}
		}
	}()
	stopSecrets := func() { stop(); <-syncDone; sec.Stop() }
	n := node.NewTSNet(tsDir, settings.Hostname, keys, tailscaleLogf(logf, *tsDebug))
	a := &app.App{Node: n, Loop: &loopback.Manager{}, Relay: &relay.Relay{Dial: n.Dial}, Dir: *dir,
		ResolvPath: *resolv, StatePath: *statePath, DNSAddr: *dns, Logf: logf}
	if keysErr != nil && where.Path != filepath.Join(*keysDir, keystore.File) {
		a.KeysOnCard = keysErr
	}
	go (&taildrop.Receiver{Src: n, Staged: n.Staged, Logf: logf, Got: a.Got,
		Dest: func() (string, error) { return config.SaveDir(*dir, config.LoadSettings(*dir)) }}).Run(ctx)
	apps := &share.Manager{Target: "127.0.0.1", Every: 3 * time.Second, Logf: logf,
		Allow: func(ctx context.Context, c net.Conn) bool { return n.Owner(ctx, c.RemoteAddr().String()) },
		Scan:  func() []share.App { return share.Running(filepath.Dir(*statePath), config.CardRoot(*dir), share.Alive) }}
	var appsNote atomic.Value // why HTTPS was chosen but isn't used, for Apps Shared Now
	appsNote.Store("")
	go func() {
		h := pages.Handler(pages.Deps{App: a, Dir: *dir, Logout: n.Logout, Reauth: n.Reauth,
			Paths: func() map[netip.Addr]string {
				c, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				return n.Paths(c)
			}, Stop: stop, SetOn: a.SetOn, Version: version,
			Restart: func() { again.Store(true); stop() }, Dev: func() bool { return remote.Available(os.Stat) },
			Shared: func() ([]pages.SharedApp, string) { return sharedApps(n.Status(), apps.Shared(), appsNote.Load().(string)) }})
		if err := q2spot.Serve(ctx, *socket, h); err != nil {
			logf("page socket: %v", err)
		}
	}()
	if settings.Remote && remote.Available(os.Stat) { /* q2remote over the tailnet, once connected */
		go func() {
			for ctx.Err() == nil && n.Status().State != "Running" {
				time.Sleep(time.Second)
			}
			if ctx.Err() != nil {
				return
			}
			logf("q2remote over the tailnet on port %s", remote.Port)
			f := &remote.Forwarder{Listen: n.Listen, Target: "127.0.0.1:" + remote.Port, Logf: logf}
			if err := f.Run(ctx); err != nil {
				logf("q2remote over the tailnet: %v", err)
			}
		}()
	}
	if settings.Apps { /* the running card apps' pages, to the owner's devices on the tailnet, once connected */
		go func() {
			for ctx.Err() == nil && n.Status().State != "Running" {
				time.Sleep(time.Second)
			}
			if ctx.Err() != nil {
				return
			}
			apps.Listen = n.Listen
			if settings.HTTPS {
				// HTTPS only with the certificate in hand, got now: a page that can't be opened at all is
				// worse than one over plain HTTP, which the tailnet encrypts anyway.
				if note := httpsReady(ctx, n.Status().CertDomain, n.Cert); note != "" {
					appsNote.Store(note)
					logf("app pages: plain HTTP: %s", note)
				} else {
					apps.Listen = n.ListenTLS
				}
			}
			apps.Run(ctx)
		}()
	}
	err = a.Run(ctx)
	stopSecrets()
	if err != nil {
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

// sharedApps is the Apps Shared Now page: each app and the address to open it at, the node's HTTPS name
// over HTTPS, else its Tailscale IP; and a note when there's something to say instead.
// tls: served over HTTPS (no note); note: why not, when HTTPS was chosen.
func sharedApps(st node.Status, apps []share.App, note string) ([]pages.SharedApp, string) {
	if st.State != "Running" || !st.SelfIP.IsValid() {
		return nil, "Not connected yet"
	}
	scheme, host := "http", st.SelfIP.String()
	if tls := note == "" && st.CertDomain != "" && httpsChosen.Load(); tls {
		scheme, host = "https", st.CertDomain
	}
	out := make([]pages.SharedApp, len(apps))
	for i, a := range apps {
		out[i] = pages.SharedApp{Name: a.Name, Address: fmt.Sprintf("%s://%s:%d", scheme, host, a.Port)}
	}
	return out, note
}

// httpsChosen is whether the pages went to HTTPS (set once, at their start).
var httpsChosen atomic.Bool

// httpsReady is "" when HTTPS can be used for the node's name (domain: "" while HTTPS is off for the
// tailnet), its certificate got with cert within a minute; else what to tell the owner.
func httpsReady(ctx context.Context, domain string, cert func(context.Context, string) error) string {
	if domain == "" {
		return "HTTPS is off for your tailnet: turn it on in Tailscale's admin console, under DNS"
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := cert(ctx, domain); err != nil {
		return "Couldn't get the HTTPS certificate, so plain HTTP: " + err.Error()
	}
	httpsChosen.Store(true)
	return ""
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
