#!/bin/sh
# licenses.sh OUT: the licence texts for everything in the card app's binary, shipped beside it as
# .tailscale/LICENSES.txt (BSD-3-Clause and MIT ask for their notices with a binary, Apache-2.0 for its
# licence): this project's own licence and NOTICE, q2-subsonic's (code copied from it), Go's (its
# standard library is in the binary), then every module compiled in, from the module cache.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
out=$1
section() { printf '\n%s\n%s\n\n' "================================================================================" "$1"; }
golicense() {
    g=$(go env GOROOT)
    for f in "$g/LICENSE" "$g/../LICENSE"; do [ -f "$f" ] && { cat "$f"; return; }; done
    echo "missing: Go's LICENSE (BSD-3-Clause, https://go.dev/LICENSE)" >&2; exit 1
}
{
    echo "Tailscale for Q2 Pod: licences of the software in this app"
    section "q2-tailscale (this app)"
    cat "$ROOT/LICENSE"; echo; cat "$ROOT/NOTICE"
    section "q2-subsonic (https://github.com/andsuor/q2-subsonic)"
    cat "$ROOT/third_party/q2-subsonic/LICENSE"
    section "Go (https://go.dev): the standard library"
    golicense
    cd "$ROOT"
    tags=$(sed 's/#.*//' "$HERE/build-tags" | tr -s ' \n' ' ' | xargs | tr ' ' ',') # as build.sh builds
    CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
        go list -tags "$tags" -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' \
        "./cmd/q2-tailscale" | sort -u | while read -r path version dir; do
        lic=$(ls "$dir" | grep -i -E '^(licen[cs]e|copying)' | head -1 || true)
        [ -n "$lic" ] || { echo "missing: a licence for $path" >&2; exit 1; }
        section "$path $version"
        cat "$dir/$lic"
        for n in $(ls "$dir" | grep -i -E '^notice' || true); do echo; cat "$dir/$n"; done
    done
} > "$out"
