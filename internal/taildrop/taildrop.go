// Package taildrop moves files sent to the Q2 with Taildrop out of Tailscale's staging folder into the
// music folder the user chose. tsnet keeps each incoming file in its state folder until it is whole; the
// Receiver waits for whole ones, renames each into place (the staging folder is on the same card) or,
// failing that, copies it through a hidden .part name, then lets Tailscale forget it. A file never
// overwrites another: a clash gets " (1)", " (2)", … before its extension.
package taildrop

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File is one whole file waiting in Tailscale's staging folder.
type File struct {
	Name string
	Size int64
}

// Source is Tailscale's LocalAPI for waiting files.
type Source interface {
	Await(ctx context.Context, d time.Duration) ([]File, error) // waits up to d for files; none is nil, nil
	Open(ctx context.Context, name string) (io.ReadCloser, error)
	Delete(ctx context.Context, name string) error
}

// Receiver moves waiting files into Dest's folder.
type Receiver struct {
	Src    Source
	Staged func(name string) string // the staged file's path, "" when not known
	Dest   func() (string, error)   // the folder files go to now
	Got    func(names []string)     // after a batch: the names saved, in order
	Logf   func(string, ...any)
	Sleep  func(ctx context.Context, d time.Duration) // a wait between failed awaits; ctx-aware sleep when nil
}

// awaitFor is how long one Await waits for a file before asking again.
const awaitFor = 60 * time.Second

// After a failed Await the next is tried retryFirst on, the wait doubling to retryMax.
const (
	retryFirst = time.Second
	retryMax   = time.Minute
)

// Run moves files as they come until ctx is done.
func (r *Receiver) Run(ctx context.Context) {
	wait := time.Duration(0)
	for ctx.Err() == nil {
		if err := r.take(ctx); err != nil {
			wait = min(max(2*wait, retryFirst), retryMax)
			r.logf("taildrop: %v; again in %v", err, wait)
			r.sleep(ctx, wait)
			continue
		}
		wait = 0
	}
}

// take waits for one batch and moves it. A file that can't be moved is logged and left waiting, for the
// next batch to try again; an error is the wait's, or a batch of which none could be moved.
func (r *Receiver) take(ctx context.Context) error {
	files, err := r.Src.Await(ctx, awaitFor)
	if err != nil || len(files) == 0 {
		return err
	}
	dir, err := r.Dest()
	if err != nil {
		return fmt.Errorf("folder: %v", err)
	}
	var got []string
	for _, f := range files {
		name, err := r.place(ctx, dir, f.Name)
		if err != nil {
			r.logf("taildrop: %s: %v", f.Name, err)
			continue
		}
		if err := r.Src.Delete(ctx, f.Name); err != nil {
			r.logf("taildrop: forgetting %s: %v", f.Name, err)
		}
		got = append(got, name)
	}
	if got == nil {
		// all failed: they're handed back at once by the next Await, so Run waits first
		return fmt.Errorf("%d waiting files couldn't be saved", len(files))
	}
	if r.Got != nil {
		r.Got(got)
	}
	return nil
}

// place puts the waiting file name in dir under a free name, which it returns.
func (r *Receiver) place(ctx context.Context, dir, name string) (string, error) {
	if base := filepath.Base(name); base != name || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("not a plain file name")
	}
	out := Unique(dir, name)
	dest := filepath.Join(dir, out)
	if r.Staged != nil {
		if src := r.Staged(name); src != "" && os.Rename(src, dest) == nil {
			return out, nil
		}
	}
	in, err := r.Src.Open(ctx, name)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp := filepath.Join(dir, "."+out+".part")
	w, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(w, in)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return out, nil
}

// Unique is name if dir has no such file, else the first free "base (n).ext".
func Unique(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for n := 0; ; n++ {
		try := name
		if n > 0 {
			try = fmt.Sprintf("%s (%d)%s", base, n, ext)
		}
		if _, err := os.Lstat(filepath.Join(dir, try)); os.IsNotExist(err) {
			return try
		}
	}
}

func (r *Receiver) logf(format string, a ...any) {
	if r.Logf != nil {
		r.Logf(format, a...)
	}
}

func (r *Receiver) sleep(ctx context.Context, d time.Duration) {
	if r.Sleep != nil {
		r.Sleep(ctx, d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
