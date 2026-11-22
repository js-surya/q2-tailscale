package taildrop

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is a Source: files waiting by name, with contents; batches handed out once each.
type fake struct {
	mu       sync.Mutex
	batches  [][]File
	content  map[string]string
	deleted  []string
	awaits   int
	awaitErr error
}

func (f *fake) Await(ctx context.Context, d time.Duration) ([]File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.awaits++
	if f.awaitErr != nil {
		return nil, f.awaitErr
	}
	if len(f.batches) == 0 {
		return nil, nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b, nil
}

func (f *fake) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	c, ok := f.content[name]
	if !ok {
		return nil, errors.New("no such file")
	}
	return io.NopCloser(strings.NewReader(c)), nil
}

func (f *fake) Delete(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	es, _ := os.ReadDir(dir)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestUnique(t *testing.T) {
	dir := t.TempDir()
	if got := Unique(dir, "a.flac"); got != "a.flac" {
		t.Fatalf("free: %q", got)
	}
	os.WriteFile(filepath.Join(dir, "a.flac"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "a (1).flac"), nil, 0o644)
	if got := Unique(dir, "a.flac"); got != "a (2).flac" {
		t.Fatalf("taken: %q", got)
	}
	os.WriteFile(filepath.Join(dir, "README"), nil, 0o644)
	if got := Unique(dir, "README"); got != "README (1)" {
		t.Fatalf("no ext: %q", got)
	}
}

// once runs one batch through r.
func once(t *testing.T, r *Receiver, f *fake) {
	t.Helper()
	r.Src = f
	if err := r.take(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRenameStaged(t *testing.T) {
	stage, dest := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(stage, "song.flac"), []byte("music"), 0o644)
	f := &fake{batches: [][]File{{{Name: "song.flac", Size: 5}}}}
	var got []string
	r := &Receiver{
		Staged: func(n string) string { return filepath.Join(stage, n) },
		Dest:   func() (string, error) { return dest, nil },
		Got:    func(n []string) { got = n },
	}
	once(t, r, f)
	if read(t, filepath.Join(dest, "song.flac")) != "music" {
		t.Fatal("not moved")
	}
	if _, err := os.Stat(filepath.Join(stage, "song.flac")); err == nil {
		t.Fatal("staged copy left")
	}
	if !reflect.DeepEqual(f.deleted, []string{"song.flac"}) || !reflect.DeepEqual(got, []string{"song.flac"}) {
		t.Fatalf("deleted %v got %v", f.deleted, got)
	}
}

func TestCopyFallback(t *testing.T) {
	dest := t.TempDir()
	f := &fake{batches: [][]File{{{Name: "song.flac"}}}, content: map[string]string{"song.flac": "music"}}
	r := &Receiver{Staged: func(string) string { return "" }, Dest: func() (string, error) { return dest, nil }}
	once(t, r, f)
	if read(t, filepath.Join(dest, "song.flac")) != "music" {
		t.Fatal("not copied")
	}
	if n := names(t, dest); !reflect.DeepEqual(n, []string{"song.flac"}) {
		t.Fatalf("left behind: %v", n)
	}
	if len(f.deleted) != 1 {
		t.Fatalf("deleted %v", f.deleted)
	}
}

func TestStagedMissingFallsBackToCopy(t *testing.T) {
	dest := t.TempDir()
	f := &fake{batches: [][]File{{{Name: "song.flac"}}}, content: map[string]string{"song.flac": "music"}}
	r := &Receiver{Staged: func(n string) string { return filepath.Join(t.TempDir(), n) }, Dest: func() (string, error) { return dest, nil }}
	once(t, r, f)
	if read(t, filepath.Join(dest, "song.flac")) != "music" {
		t.Fatal("not copied")
	}
}

func TestClash(t *testing.T) {
	dest := t.TempDir()
	os.WriteFile(filepath.Join(dest, "a.flac"), []byte("old"), 0o644)
	f := &fake{batches: [][]File{{{Name: "a.flac"}}}, content: map[string]string{"a.flac": "new"}}
	var got []string
	r := &Receiver{Staged: func(string) string { return "" }, Dest: func() (string, error) { return dest, nil }, Got: func(n []string) { got = n }}
	once(t, r, f)
	if read(t, filepath.Join(dest, "a.flac")) != "old" || read(t, filepath.Join(dest, "a (1).flac")) != "new" {
		t.Fatal("overwrote")
	}
	if !reflect.DeepEqual(got, []string{"a (1).flac"}) {
		t.Fatalf("got %v", got)
	}
}

func TestFailedFileLeftWaiting(t *testing.T) {
	dest := t.TempDir()
	f := &fake{batches: [][]File{{{Name: "gone.flac"}}}, content: map[string]string{}}
	var got []string
	r := &Receiver{Src: f, Staged: func(string) string { return "" }, Dest: func() (string, error) { return dest, nil }, Got: func(n []string) { got = n }}
	if err := r.take(context.Background()); err == nil {
		t.Fatal("a batch of nothing saved isn't an error")
	}
	if len(f.deleted) != 0 || got != nil {
		t.Fatalf("deleted %v got %v", f.deleted, got)
	}
	if n := names(t, dest); n != nil {
		t.Fatalf("left behind: %v", n)
	}
}

func TestBackoff(t *testing.T) {
	f := &fake{awaitErr: errors.New("not running")}
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	r := &Receiver{Src: f, Dest: func() (string, error) { return t.TempDir(), nil },
		Sleep: func(ctx context.Context, d time.Duration) {
			waits = append(waits, d)
			if len(waits) == 8 {
				cancel()
			}
		}}
	r.Run(ctx)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	if !reflect.DeepEqual(waits, want) {
		t.Fatalf("waits %v", waits)
	}
}

// A file that can't be placed comes back from every Await at once: a batch of nothing but failures is
// an error, so Run waits between tries instead of spinning.
func TestStuckFileBacksOff(t *testing.T) {
	f := &fake{content: map[string]string{}}
	for i := 0; i < 20; i++ {
		f.batches = append(f.batches, []File{{Name: ".hidden"}})
	}
	var waits int
	ctx, cancel := context.WithCancel(context.Background())
	r := &Receiver{Src: f, Dest: func() (string, error) { return t.TempDir(), nil },
		Sleep: func(context.Context, time.Duration) {
			if waits++; waits == 3 {
				cancel()
			}
		}}
	r.Run(ctx)
	if f.awaits != 3 {
		t.Fatalf("%d awaits for 3 waits", f.awaits)
	}
}
