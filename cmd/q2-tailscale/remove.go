package main

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// removeScript outlives the app. The App Store's Remove stops the app, then deletes its card folder,
// but not the login it keeps in internal storage: so, after a stop, this watches the card folder for
// tries×gap seconds, and once it's gone with the card still mounted (a Remove, not the card taken out
// or handed to USB storage, which take the folder with them) deletes the login's folder too.
//
// A card pulled during the watch makes the folder look gone while /proc/mounts still lists the card
// until it's unmounted, so the card must also still be readable, and the folder still gone after.
const removeScript = `dir=$1 card=$2 keys=$3 mounts=$4 tries=$5 gap=$6
i=0
while [ "$i" -lt "$tries" ]; do
	if [ ! -e "$dir" ]; then
		ls -a "$card" >/dev/null 2>&1 && grep -q " $card " "$mounts" && [ ! -e "$dir" ] && rm -rf "$keys"
		exit 0
	fi
	sleep "$gap"
	i=$((i + 1))
done
`

// watchRemove is removeScript for the app's card folder dir (its card the folder above) and the login's
// folder keys, the mounts read from mounts; nil when either path is too short to trust with rm -rf.
func watchRemove(dir, keys, mounts string, tries int, gap string) *exec.Cmd {
	deep := func(p string, n int) bool {
		return filepath.IsAbs(p) && filepath.Clean(p) == p && len(strings.Split(strings.Trim(p, "/"), "/")) >= n
	}
	if !deep(keys, 3) || !deep(dir, 2) {
		return nil
	}
	return exec.Command("/bin/sh", "-c", removeScript, "sh", dir, filepath.Dir(dir), keys, mounts,
		strconv.Itoa(tries), gap)
}

// startRemoveWatch starts the watch in a session of its own, so the launcher's KILL, 2 s after its
// TERM, leaves it running: a minute, every second.
func startRemoveWatch(dir, keys string, logf func(string, ...any)) {
	cmd := watchRemove(dir, keys, "/proc/mounts", 60, "1")
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf("remove watch: %v", err)
		return
	}
	cmd.Process.Release()
}
