package management

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeFiles creates files relative to base, creating parent directories as
// needed.
func writeFiles(t *testing.T, base string, names []string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(base, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanDirectoryFiltersExtensions(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, []string{"a.mp4", "b.mkv", "note.txt", "song.mp3"})

	// empty Extensions falls back to DefaultVideoExts
	got, err := ScanDirectory(context.Background(), root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 matching files, got %d: %+v", len(got), got)
	}
	names := map[string]bool{}
	for _, m := range got {
		names[m.Name] = true
	}
	if !names["a.mp4"] || !names["b.mkv"] {
		t.Fatalf("unexpected matches: %+v", names)
	}
}

func TestScanDirectoryCustomExtensions(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, []string{"clip.MOV", "notes.txt", "a.mp4"})

	// entries may omit the leading dot and are matched case-insensitively
	got, err := ScanDirectory(context.Background(), root, ScanOptions{
		Extensions: []string{"MOV", ".txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 matches, got %d: %+v", len(got), got)
	}
	names := map[string]bool{}
	for _, m := range got {
		names[m.Name] = true
	}
	if !names["clip.MOV"] || !names["notes.txt"] {
		t.Fatalf("unexpected matches: %+v", names)
	}
}

func TestScanDirectoryRecursion(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, []string{"top.mp4", filepath.Join("sub", "deep.mkv")})

	// zero value IncludeSubdirs: top-level only, subdirectories skipped
	top, err := ScanDirectory(context.Background(), root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].Name != "top.mp4" {
		t.Fatalf("expected only top.mp4, got %+v", top)
	}

	// recursive: both files found
	all, err := ScanDirectory(context.Background(), root, ScanOptions{IncludeSubdirs: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 files recursively, got %d: %+v", len(all), all)
	}
}

func TestScanDirectoryCancellation(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, []string{"a.mp4"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ScanDirectory(ctx, root, ScanOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestScanDirectoryRootErrors(t *testing.T) {
	tmp := t.TempDir()

	// missing root
	if _, err := ScanDirectory(context.Background(), filepath.Join(tmp, "missing"), ScanOptions{}); err == nil {
		t.Fatal("expected error for missing root")
	}

	// root is a regular file, not a directory
	f := filepath.Join(tmp, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanDirectory(context.Background(), f, ScanOptions{}); err == nil {
		t.Fatal("expected error for non-directory root")
	}

	// all configured extensions are blank -> ErrInvalid
	if _, err := ScanDirectory(context.Background(), tmp, ScanOptions{Extensions: []string{"  ", ""}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestScanKeepsIDOnUpdate(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	root := t.TempDir()
	writeFiles(t, root, []string{"a.mp4"})

	first, err := ms.Scan(context.Background(), root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Added) != 1 {
		t.Fatalf("expected 1 added, got %d", len(first.Added))
	}
	id := first.Added[0].ID
	if id == "" {
		t.Fatal("expected a generated id on add")
	}

	// a second scan of the same path updates in place, keeping the id
	second, err := ms.Scan(context.Background(), root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Added) != 0 {
		t.Fatalf("expected 0 added on second scan, got %d", len(second.Added))
	}
	if len(second.Updated) != 1 {
		t.Fatalf("expected 1 updated on second scan, got %d", len(second.Updated))
	}
	if second.Updated[0].ID != id {
		t.Fatalf("expected updated entry to keep id %q, got %q", id, second.Updated[0].ID)
	}
	if second.Skipped != 0 {
		t.Fatalf("expected 0 skipped, got %d", second.Skipped)
	}
	if _, err := ms.Get(id); err != nil {
		t.Fatalf("expected media %q to still be retrievable: %v", id, err)
	}
}
