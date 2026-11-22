#!/bin/sh
# Installed as /mnt/mmc/.tailscale/run. Q2 Pod runs it to start the app: from its row, and with q2app's
# wifi=1 when Wi-Fi connects (it stops it again once Wi-Fi has been gone 10 s). It must return at once:
# the loop runs in the background, its pid in /tmp/q2-tailscale, and a TERM to that pid stops it.
# Adapted from q2-subsonic's launcher (MIT), but the app runs from the card, not a copy in /tmp: it is
# about 24 MB and the Q2's /tmp is RAM. Q2 Pod stops it before USB storage takes the card.
# The loop restarts the app after a failure, waiting 5 s and doubling to a minute; an exit of 0 (TERM,
# the page's Stop) ends it. A failed run's stderr and exit code are kept in /tmp/q2-tailscale.crash,
# which the next start appends to .tailscale/log.txt (its last 256 KiB kept).
T=${Q2TS_TMP:-/tmp}
pid=$T/q2-tailscale
[ -f "$pid" ] && kill -0 "$(cat "$pid")" 2>/dev/null && exit 0
dir=$(cd "$(dirname "$0")" && pwd)
if [ -s "$T/q2-tailscale.crash" ]; then
    cat "$T/q2-tailscale.crash" >> "$dir/log.txt"
    rm -f "$T/q2-tailscale.crash"
    if [ "$(wc -c < "$dir/log.txt")" -gt 262144 ]; then
        tail -c 262144 "$dir/log.txt" > "$dir/log.txt.tmp" && mv "$dir/log.txt.tmp" "$dir/log.txt"
    fi
fi
(
    child=
    # A TERM keeps a crash for the next start. An app that hasn't ended 2 s after its TERM is killed, so
    # the loop is gone within the 3 s Q2 Pod waits (addon.c's AWAY_MS); the app undoes its changes
    # (resolv.conf, lo) on the TERM, and a KILLed one's are undone at its next start.
    trap '[ -n "$child" ] && kill -TERM $child 2>/dev/null && { (sleep 2; kill -KILL $child) 2>/dev/null & w=$!; wait $child; kill $w 2>/dev/null; }; rm -f "$pid" "$T/q2-tailscale.err" "$T/q2-tailscale.state"; exit 0' TERM INT
    delay=5
    while :; do
        "$dir/q2-tailscale" -dir "$dir" 2>"$T/q2-tailscale.err" &
        child=$!
        wait $child
        code=$? child=
        [ $code = 0 ] && break
        { cat "$T/q2-tailscale.err"; echo "exit $code"; } >"$T/q2-tailscale.crash"
        sleep $delay &
        wait $! # not sleep alone: a TERM interrupts a wait, but waits for a foreground sleep
        [ $delay -lt 60 ] && delay=$((delay * 2))
    done
    rm -f "$pid" "$T/q2-tailscale.err" "$T/q2-tailscale.state"
) </dev/null >/dev/null 2>&1 &
echo $! > "$pid"
