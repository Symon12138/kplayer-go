package management

import (
	"errors"
	"runtime"
	"testing"
)

func TestMediaAddBatch(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)

	one := &Media{Path: "/v/1.mp4"}
	two := &Media{Path: "/v/2.mp4"}
	if err := ms.AddBatch([]*Media{one, two}); err != nil {
		t.Fatalf("addbatch: %v", err)
	}
	if got := len(ms.List()); got != 2 {
		t.Fatalf("expected 2 media, got %d", got)
	}
	// Each stored entry carries a generated id and metadata.
	for i, m := range []*Media{one, two} {
		if m.ID == "" {
			t.Fatalf("item %d: expected generated id", i)
		}
		if m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
			t.Fatalf("item %d: expected timestamps", i)
		}
		if m.Ext != ".mp4" {
			t.Fatalf("item %d: expected ext .mp4, got %q", i, m.Ext)
		}
	}
}

func TestMediaAddBatchRejectsDuplicate(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	if err := ms.Add(&Media{Path: "/v/a.mp4"}); err != nil {
		t.Fatal(err)
	}
	// A batch containing an already-tracked path fails as a whole.
	err := ms.AddBatch([]*Media{{Path: "/v/b.mp4"}, {Path: "/v/a.mp4"}})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}
	if got := len(ms.List()); got != 1 {
		t.Fatalf("expected batch rollback to leave 1 media, got %d", got)
	}
}

// TestMediaAddBatchRollbackDoesNotPolluteInput ensures that when a batch
// fails part-way, the caller's Media objects are left untouched (no id, no
// timestamps), so a retry after removing the bad entry stays correct.
func TestMediaAddBatchRollbackDoesNotPolluteInput(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)

	first := &Media{Path: "/v/a.mp4"}
	dup := &Media{Path: "/v/a.mp4"} // duplicate of first -> whole batch fails
	if err := ms.AddBatch([]*Media{first, dup}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}

	if got := len(ms.List()); got != 0 {
		t.Fatalf("expected store untouched after failed batch, got %d media", got)
	}
	if first.ID != "" {
		t.Fatalf("caller input polluted: first.ID = %q, want empty", first.ID)
	}
	if !first.CreatedAt.IsZero() || !first.UpdatedAt.IsZero() {
		t.Fatal("caller input polluted: timestamps were set on failed entry")
	}
	if first.Path != "/v/a.mp4" {
		t.Fatalf("caller input polluted: first.Path = %q", first.Path)
	}
}

func TestMediaAddDuplicatePathRejected(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	if err := ms.Add(&Media{Path: "/v/a.mp4"}); err != nil {
		t.Fatal(err)
	}
	if err := ms.Add(&Media{Path: "/v/a.mp4"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate path, got %v", err)
	}
}

// TestMediaPathDedupCaseInsensitiveOnWindows verifies that two paths that
// differ only by case are treated as duplicates on Windows (case-insensitive
// filesystem) but as distinct entries elsewhere.
func TestMediaPathDedupCaseInsensitiveOnWindows(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	if err := ms.Add(&Media{Path: "/V/Test.MP4"}); err != nil {
		t.Fatal(err)
	}
	err := ms.Add(&Media{Path: "/v/test.mp4"})
	if runtime.GOOS == "windows" {
		if !errors.Is(err, ErrExists) {
			t.Fatalf("expected ErrExists for case variant on windows, got %v", err)
		}
	} else if err != nil {
		t.Fatalf("expected case variants to be distinct on non-windows, got %v", err)
	}
}

func TestMediaDeleteInUseByPlaylist(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := NewPlaylistService(s).Create("main", "", []string{m.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err := ms.Delete(m.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse, got %v", err)
	}
}

func TestMediaDelete(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if err := ms.Delete(m.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(ms.List()) != 0 {
		t.Fatal("expected empty media list")
	}
	if _, err := ms.Get(m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
