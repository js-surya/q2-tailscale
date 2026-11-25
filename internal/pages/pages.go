// Package pages draws the app's page on the Q2 (q2-pod docs/librespot.md, Q2SPOT/1): the status, sign-in,
// the tailnet's machines, the forwards and the settings.
package pages

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"q2-tailscale/internal/config"
	"q2-tailscale/internal/node"
	"q2-tailscale/internal/q2spot"
)

// AppView is what the pages read of the running app.
type AppView interface {
	Status() node.Status
	ForwardProblem(port uint16) string
	Warnings() []node.Warning // what's wrong while connected, nil when nothing is
	Received() string         // "Received song.flac" for a while after Taildrop files came, else ""
}

// Deps are the pages' ties to the app: its view, the card folder, sign-out, stopping the app (after the
// reply goes), whether a local port is free, and the clock.
type Deps struct {
	App      AppView
	Dir      string
	Logout   func(context.Context) error
	Reauth   func(context.Context) error // a new sign-in link, connected meanwhile (Renew Sign-In)
	Stop     func()
	PortFree func(uint16) bool
	Now      func() time.Time
	SetOn    func(context.Context, bool) error // Tailscale on or off, the choice kept (app.SetOn)
	Caps     func() string                     // the firmware's /tmp/q2-addon.caps (FirmwareCaps)
	Restart  func()                            // the app starts over, its changes undone first (Send Logs)
	Version  string                            // the app's, for About
	Dev      func() bool                       // a developer firmware (q2remote there): its option shown
	Shared   func() ([]SharedApp, string)      // app pages on the tailnet now, and a note (HTTPS off, not yet connected)
}

// SharedApp is a running card app's page on the tailnet, and the address to open it at.
type SharedApp struct{ Name, Address string }

// FirmwareCaps reads the words Q2 Pod writes for card apps: what its pages can draw.
func FirmwareCaps() string {
	b, _ := os.ReadFile("/tmp/q2-addon.caps")
	return string(b)
}

// toggle is an on/off row: a switch where the firmware draws them (caps: switch), else an action row
// whose subtitle says On or Off.
func toggle(caps, uri, title string, on bool) q2spot.Row {
	if slices.Contains(strings.Fields(caps), "switch") {
		state := "off"
		if on {
			state = "on"
		}
		return q2spot.Row{Kind: "switch", URI: uri, Title: title, Subtitle: state}
	}
	return q2spot.Row{Kind: "action", URI: uri, Title: title, Subtitle: onOff(on)}
}

const signOutWindow = 10 * time.Second

// PortFree is whether 127.0.0.1:port can be listened on now.
func PortFree(port uint16) bool {
	l, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(int(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func ok(rows ...q2spot.Row) q2spot.Reply { return q2spot.Reply{Status: "ok", Rows: rows} }
func say(msg string) q2spot.Reply        { return q2spot.Reply{Status: "ok", Message: msg} }
func fail(format string, a ...any) q2spot.Reply {
	return q2spot.Reply{Status: "error", Message: fmt.Sprintf(format, a...)}
}

// Handler answers the page's requests.
func Handler(d Deps) q2spot.Handler {
	var mu sync.Mutex
	var signOutAt time.Time
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.PortFree == nil {
		d.PortFree = PortFree
	}
	if d.Caps == nil {
		d.Caps = FirmwareCaps
	}
	return func(op string, args []string) q2spot.Reply {
		arg := func(i int) string {
			if i < len(args) {
				return args[i]
			}
			return ""
		}
		switch op {
		case "menu":
			return menu(d.App.Status(), d.App.Warnings(), d.App.Received(), toggle(d.Caps(), "power", "Tailscale", !config.LoadSettings(d.Dir).Off), d.Now())
		case "power":
			on := config.LoadSettings(d.Dir).Off // pressed: the other way
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := d.SetOn(ctx, on); err != nil {
				return fail("Couldn't turn Tailscale %s: %v", onOff(on), err)
			}
			return say("Tailscale " + strings.ToLower(onOff(on)))
		case "autostart":
			s := config.LoadSettings(d.Dir)
			s.Manual = !s.Manual
			if err := config.SetAutoStart(d.Dir, !s.Manual); err != nil {
				return fail("Couldn't change it: %v", err)
			}
			if err := config.SaveSettings(d.Dir, s); err != nil {
				return fail("Couldn't save: %v", err)
			}
			if s.Manual {
				return say("Starts when you open it")
			}
			return say("Starts with Wi-Fi")
		case "machines":
			return machines(d.App.Status())
		case "forwards":
			return forwards(d)
		case "addfwd":
			var rows []q2spot.Row
			for _, p := range d.App.Status().Peers {
				rows = append(rows, q2spot.Row{Kind: "list", URI: "addfwd2", Title: p.Name, Subtitle: p.IP.String(), Context: p.FQDN})
			}
			if rows == nil {
				return ok(q2spot.Row{Kind: "info", Title: "No machines", Subtitle: "Sign in first"})
			}
			return ok(rows...)
		case "addfwd2":
			host := arg(0)
			var rows []q2spot.Row
			for _, p := range config.DefaultPorts {
				rows = append(rows, q2spot.Row{Kind: "action", URI: "fwadd", Title: "Port " + strconv.Itoa(int(p)), Subtitle: portName(p),
					Context: host + " " + strconv.Itoa(int(p))})
			}
			rows = append(rows, q2spot.Row{Kind: "field", URI: "fwaddport", Title: "Other port…", Subtitle: "1 to 65535", Context: host})
			return ok(rows...)
		case "fwadd":
			host, port, _ := strings.Cut(arg(0), " ")
			return addForward(d, host, port)
		case "fwaddport":
			return addForward(d, arg(0), arg(1))
		case "fwdel":
			p, err := strconv.ParseUint(arg(0), 10, 16)
			if err != nil {
				return fail("No such forward")
			}
			fs, _ := config.LoadForwards(d.Dir)
			fs = slices.DeleteFunc(fs, func(f config.Forward) bool { return f.LocalPort == uint16(p) })
			if err := config.SaveForwards(d.Dir, fs); err != nil {
				return fail("Couldn't save: %v", err)
			}
			return say("Forward removed")
		case "settings":
			s := config.LoadSettings(d.Dir)
			ports := "None"
			if len(s.Ports) > 0 {
				ps := make([]string, len(s.Ports))
				for i, p := range s.Ports {
					ps[i] = strconv.Itoa(int(p))
				}
				ports = strings.Join(ps, ", ")
			}
			save := s.Save
			if save == "" {
				save = config.DefaultSave
			}
			rows := []q2spot.Row{toggle(d.Caps(), "autostart", "Start with Wi-Fi", config.AutoStart(d.Dir)),
				toggle(d.Caps(), "logs", "Send Logs to Tailscale", s.Logs),
				q2spot.Row{Kind: "list", URI: "saveto", Title: "Save To", Subtitle: save},
				q2spot.Row{Kind: "field", URI: "setports", Title: "Extra Ports:", Subtitle: ports},
				q2spot.Row{Kind: "field", URI: "sethost", Title: "Device Name:", Subtitle: s.Hostname}}
			rows = append(rows, toggle(d.Caps(), "apps", "Q2 Apps on Tailscale", s.Apps))
			if s.Apps {
				rows = append(rows, toggle(d.Caps(), "https", "Use HTTPS", s.HTTPS),
					q2spot.Row{Kind: "list", URI: "sharedapps", Title: "Apps Shared Now"})
			}
			if d.Dev != nil && d.Dev() { /* a developer firmware: its option, nowhere else */
				rows = append(rows, toggle(d.Caps(), "remote", "q2remote over Tailscale", s.Remote))
			}
			rows = append(rows, q2spot.Row{Kind: "list", URI: "about", Title: "About"},
				q2spot.Row{Kind: "action", URI: "signout", Title: "Sign Out"})
			return ok(rows...)
		case "remote": // q2remote through the tailnet, set at the app's start: saved, then it starts over
			if d.Dev == nil || !d.Dev() {
				return fail("Only on a developer firmware")
			}
			s := config.LoadSettings(d.Dir)
			s.Remote = !s.Remote
			if err := config.SaveSettings(d.Dir, s); err != nil {
				return fail("Couldn't save: %v", err)
			}
			if d.Restart != nil {
				go func() { time.Sleep(300 * time.Millisecond); d.Restart() }() // the reply goes first
			}
			return say("Restarting with q2remote over Tailscale " + onOff(s.Remote))
		case "apps", "https": // set at the app's start, as Send Logs: saved, then it starts over
			s := config.LoadSettings(d.Dir)
			msg := ""
			if op == "apps" {
				s.Apps = !s.Apps
				msg = "Restarting with Q2 apps on Tailscale " + onOff(s.Apps)
			} else {
				s.HTTPS = !s.HTTPS
				msg = "Restarting with HTTPS " + onOff(s.HTTPS)
			}
			if err := config.SaveSettings(d.Dir, s); err != nil {
				return fail("Couldn't save: %v", err)
			}
			if d.Restart != nil {
				go func() { time.Sleep(300 * time.Millisecond); d.Restart() }() // the reply goes first
			}
			return say(msg)
		case "sharedapps":
			var apps []SharedApp
			note := "Not connected yet"
			if d.Shared != nil {
				apps, note = d.Shared()
			}
			var rows []q2spot.Row
			for _, a := range apps {
				rows = append(rows, q2spot.Row{Kind: "info", Title: a.Name, Subtitle: a.Address})
			}
			if len(apps) == 0 && note == "" {
				note = "None running: open an app with a page, such as EQ Studio"
			}
			if note != "" {
				rows = append(rows, q2spot.Row{Kind: "info", Title: note})
			}
			rows = append(rows, q2spot.Row{Kind: "info", Title: "Only your own devices", Subtitle: "Each app still asks for its PIN"})
			return ok(rows...)
		case "logs": // Tailscale's logs are set at the app's start: saved, then it starts over
			s := config.LoadSettings(d.Dir)
			s.Logs = !s.Logs
			if err := config.SaveSettings(d.Dir, s); err != nil {
				return fail("Couldn't save: %v", err)
			}
			if d.Restart != nil {
				go func() { time.Sleep(300 * time.Millisecond); d.Restart() }() // the reply goes first
			}
			return say("Restarting with logs " + onOff(s.Logs))
		case "saveto":
			return saveTo(d.Dir, arg(0))
		case "savehere":
			rel := arg(0)
			if rel == "" || !config.ValidSave(rel) || !isDir(filepath.Join(config.CardRoot(d.Dir), rel)) {
				return fail("No such folder")
			}
			s := config.LoadSettings(d.Dir)
			s.Save = rel
			if rel == config.DefaultSave {
				s.Save = ""
			}
			if err := config.SaveSettings(d.Dir, s); err != nil {
				return fail("Couldn't save: %v", err)
			}
			return say("Files go to " + rel)
		case "setports":
			ports, err := config.ParsePorts(arg(0))
			if err != nil {
				return fail("%v", err)
			}
			s := config.LoadSettings(d.Dir)
			s.Ports = ports
			if err := config.SaveSettings(d.Dir, s); err != nil {
				return fail("Couldn't save: %v", err)
			}
			return say("Ports saved")
		case "sethost":
			name := strings.TrimSpace(arg(0))
			if !config.ValidHostname(name) {
				return fail("Use a-z, 0-9 and -")
			}
			s := config.LoadSettings(d.Dir)
			s.Hostname = name
			if err := config.SaveSettings(d.Dir, s); err != nil {
				return fail("Couldn't save: %v", err)
			}
			return say("Restart Tailscale to apply")
		case "renew":
			if d.Reauth == nil {
				return fail("Renewing isn't available")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := d.Reauth(ctx); err != nil {
				return fail("Couldn't renew: %v", err)
			}
			return say("Now open Renew from Your Phone")
		case "signout":
			mu.Lock()
			now, armed := d.Now(), signOutAt
			if armed.IsZero() || now.Sub(armed) > signOutWindow {
				signOutAt = now
				mu.Unlock()
				return say("Press again to sign out")
			}
			signOutAt = time.Time{}
			mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := d.Logout(ctx); err != nil {
				return fail("Couldn't sign out: %v", err)
			}
			return say("Signed out")
		case "about":
			return about(d.Version)
		case "stop":
			go func() { time.Sleep(20 * time.Millisecond); d.Stop() }() // after the reply
			return ok()
		}
		return q2spot.Reply{Status: "unsupported"}
	}
}

// saveTo is the Save To page for the card's folder rel: Save here (marked when it's the choice now),
// then a row per folder in it, by name, hidden ones (.tailscale, .spotify, …) left out. The card itself
// ("") lists its folders only; the default folder is among its parent's even before it's made.
func saveTo(dir, rel string) q2spot.Reply {
	root := config.CardRoot(dir)
	if !config.ValidSave(rel) || (rel != "" && !isDir(filepath.Join(root, rel))) {
		return fail("No such folder")
	}
	var rows []q2spot.Row
	if rel != "" {
		cur := config.LoadSettings(dir).Save
		if cur == "" {
			cur = config.DefaultSave
		}
		title := "Save here"
		if rel == cur {
			title = "✓ Save here"
		}
		rows = append(rows, q2spot.Row{Kind: "action", URI: "savehere", Title: title, Subtitle: rel, Context: rel})
	}
	es, _ := os.ReadDir(filepath.Join(root, rel))
	var subs []string
	for _, e := range es {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			subs = append(subs, e.Name())
		}
	}
	if parent, name := filepath.Split(config.DefaultSave); filepath.Clean(parent) == filepath.Clean(rel+"/.") && !slices.Contains(subs, name) {
		os.MkdirAll(filepath.Join(root, config.DefaultSave), 0o755)
		subs = append(subs, name)
	}
	slices.SortFunc(subs, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	for _, sub := range subs {
		rows = append(rows, q2spot.Row{Kind: "list", URI: "saveto", Title: sub, Context: filepath.Join(rel, sub)})
	}
	return ok(rows...)
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// about is the app's version and credits; the full notices are in NOTICE and LICENSES.txt.
func about(version string) q2spot.Reply {
	row := func(title, sub string) q2spot.Row { return q2spot.Row{Kind: "info", Title: title, Subtitle: sub} }
	return ok(
		row("Version", version),
		row("Made by", "Syntax-Error"),
		row("Thanks", "DiamondBond & the Q2 Pod community"),
		row("Thanks — andsuor", "QR code & launcher, from q2-subsonic"),
		row("Tailscale", "© Tailscale Inc. · BSD-3-Clause"),
		row("Not an official Tailscale app", "Tailscale is a trademark of Tailscale Inc."),
		row("Licenses", ".tailscale/LICENSES.txt"))
}

func portName(p uint16) string {
	switch p {
	case 80:
		return "Web"
	case 443:
		return "Web (https)"
	case 4533:
		return "Navidrome"
	case 8096:
		return "Jellyfin"
	case 8920:
		return "Jellyfin (https)"
	case 32400:
		return "Plex"
	}
	return ""
}

func onOff(on bool) string {
	if on {
		return "On"
	}
	return "Off"
}

// menu is the app's first page: the switch, the state, what Taildrop just brought, sign-in while signed
// out, then the lists.
func menu(st node.Status, warnings []node.Warning, received string, power q2spot.Row, now time.Time) q2spot.Reply {
	on := power.Subtitle == "on" || power.Subtitle == "On"
	status := q2spot.Row{Kind: "info"}
	r := q2spot.Reply{Status: "ok"}
	r.Rows = append(r.Rows, power)
	switch {
	case !on && st.State != "NeedsLogin":
		status.Title, status.Subtitle = "Off", "Signed in · not connected"
	case st.State == "Running":
		status.Title, status.Subtitle = "Connected", st.Self+" · "+st.SelfIP.String()
	case st.State == "NeedsLogin":
		status.Title, status.Subtitle = "Signed out", "Sign in to join your tailnet"
		r.Message = "refresh"
	case st.State == "Stopped":
		status.Title = "Stopped"
		r.Message = "refresh"
	default:
		status.Title = "Connecting…"
		r.Message = "refresh"
	}
	r.Rows = append(r.Rows, status)
	if received != "" {
		r.Rows = append(r.Rows, q2spot.Row{Kind: "info", Title: received, Subtitle: "Run Scan music now to see it in Library"})
	}
	r.Message = "refresh" // asked again while on screen: the state and the warnings as they come
	if on {
		for _, w := range warnings {
			r.Rows = append(r.Rows, q2spot.Row{Kind: "info", Title: "Warning: " + w.Title, Subtitle: w.Text})
		}

	}
	switch {
	case st.State == "NeedsLogin":
		r.Rows = append(r.Rows, q2spot.Row{Kind: "remote", URI: "signin", Title: "Sign In from Your Phone", Subtitle: "Scan the QR code"})
	case st.State == "Running" && st.AuthURL != "": // renewing: the code is up
		r.Rows = append(r.Rows, q2spot.Row{Kind: "remote", URI: "signin", Title: "Renew from Your Phone", Subtitle: "Scan the QR code"})
	case st.State == "Running":
		if _, soon := st.ExpiresSoon(now); soon && on {
			r.Rows = append(r.Rows, q2spot.Row{Kind: "action", URI: "renew", Title: "Renew Sign-In", Subtitle: "Keeps this Q2 on your tailnet"})
		}
	}
	r.Rows = append(r.Rows,
		q2spot.Row{Kind: "list", URI: "machines", Title: "Machines", Subtitle: fmt.Sprintf("%d on the tailnet", len(st.Peers))},
		q2spot.Row{Kind: "list", URI: "forwards", Title: "Forwards"},
		q2spot.Row{Kind: "list", URI: "settings", Title: "Settings"})
	return r
}

func machines(st node.Status) q2spot.Reply {
	peers := slices.Clone(st.Peers)
	slices.SortFunc(peers, func(a, b node.Peer) int { return strings.Compare(a.Name, b.Name) })
	var rows []q2spot.Row
	for _, p := range peers {
		state := "offline"
		if p.Online {
			state = "online"
		}
		rows = append(rows, q2spot.Row{Kind: "info", Title: p.Name, Subtitle: p.IP.String() + " · " + state})
	}
	if rows == nil {
		return ok(q2spot.Row{Kind: "info", Title: "No machines", Subtitle: "Sign in, or add one to your tailnet"})
	}
	return ok(rows...)
}

func forwards(d Deps) q2spot.Reply {
	fs, _ := config.LoadForwards(d.Dir)
	var rows []q2spot.Row
	for _, f := range fs {
		sub := fmt.Sprintf("→ %s:%d", f.Host, f.Port)
		if p := d.App.ForwardProblem(f.LocalPort); p != "" {
			sub += " · " + p
		}
		rows = append(rows, q2spot.Row{Kind: "action", URI: "fwdel", Title: fmt.Sprintf("127.0.0.1:%d", f.LocalPort), Subtitle: sub,
			Context: strconv.Itoa(int(f.LocalPort))})
	}
	rows = append(rows, q2spot.Row{Kind: "list", URI: "addfwd", Title: "Add Forward"})
	return ok(rows...)
}

// addForward saves host:port on a local port: the same number if free, else the first free from 10000.
func addForward(d Deps, host, port string) q2spot.Reply {
	p, err := strconv.ParseUint(strings.TrimSpace(port), 10, 16)
	if err != nil || p == 0 || host == "" {
		return fail("A port is 1 to 65535")
	}
	fs, _ := config.LoadForwards(d.Dir)
	used := func(lp uint16) bool {
		return slices.ContainsFunc(fs, func(f config.Forward) bool { return f.LocalPort == lp }) || !d.PortFree(lp)
	}
	local := uint16(p)
	for lp := 10000; used(local); lp++ {
		if lp > 65535 {
			return fail("No free local port")
		}
		local = uint16(lp)
	}
	fs = append(fs, config.Forward{LocalPort: local, Host: host, Port: uint16(p)})
	if err := config.SaveForwards(d.Dir, fs); err != nil {
		return fail("Couldn't save: %v", err)
	}
	return say(fmt.Sprintf("Forward added · 127.0.0.1:%d", local))
}
