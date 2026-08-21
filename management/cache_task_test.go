package management

import (
	"errors"
	"testing"
	"time"
)

func TestCacheTaskCRUD(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewCacheTaskService(s)
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")

	// Create defaults to pending.
	task, err := cs.Create(CacheTaskSpec{MediaID: m1.ID, Note: "prime"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.ID == "" {
		t.Fatal("expected generated id")
	}
	if task.MediaID != m1.ID || task.Status != CachePending || task.Note != "prime" {
		t.Fatalf("unexpected task: %+v", task)
	}
	if task.CompletedAt != nil {
		t.Fatalf("expected nil CompletedAt for pending task, got %v", task.CompletedAt)
	}

	got, err := cs.Get(task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MediaID != m1.ID || got.Status != CachePending {
		t.Fatalf("unexpected get: %+v", got)
	}

	// Update replaces media reference, note and status.
	upd, err := cs.Update(task.ID, CacheTaskSpec{MediaID: m2.ID, Note: "re-prime", Status: CacheFailed})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.MediaID != m2.ID || upd.Note != "re-prime" || upd.Status != CacheFailed {
		t.Fatalf("unexpected update: %+v", upd)
	}
	if upd.CompletedAt == nil {
		t.Fatal("expected CompletedAt set for failed status")
	}

	if err := cs.Delete(task.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(cs.List()) != 0 {
		t.Fatal("expected empty task list")
	}
	if _, err := cs.Get(task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := cs.Delete(task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestCacheTaskMediaReferenceValidation(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewCacheTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	// empty media id is rejected
	if _, err := cs.Create(CacheTaskSpec{MediaID: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty media id, got %v", err)
	}
	// a media id that does not exist is rejected with ErrNotFound wrapped
	if _, err := cs.Create(CacheTaskSpec{MediaID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing media, got %v", err)
	}
	// an unknown status is rejected
	if _, err := cs.Create(CacheTaskSpec{MediaID: m.ID, Status: "paused"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown status, got %v", err)
	}

	task, err := cs.Create(CacheTaskSpec{MediaID: m.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// update onto a missing media is rejected and leaves the task unchanged
	if _, err := cs.Update(task.ID, CacheTaskSpec{MediaID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for update onto missing media, got %v", err)
	}
	if _, err := cs.Update(task.ID, CacheTaskSpec{MediaID: m.ID, Status: "paused"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for update with unknown status, got %v", err)
	}
	got, err := cs.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MediaID != m.ID || got.Status != CachePending {
		t.Fatalf("task changed by rejected update: %+v", got)
	}
}

func TestCacheTaskCreateWithTerminalStatus(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewCacheTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	// a task created directly in a terminal state records CompletedAt
	done, err := cs.Create(CacheTaskSpec{MediaID: m.ID, Status: CacheDone})
	if err != nil {
		t.Fatalf("create done: %v", err)
	}
	if done.Status != CacheDone || done.CompletedAt == nil {
		t.Fatalf("expected done task with CompletedAt, got %+v", done)
	}
	failed, err := cs.Create(CacheTaskSpec{MediaID: m.ID, Status: CacheFailed})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if failed.Status != CacheFailed || failed.CompletedAt == nil {
		t.Fatalf("expected failed task with CompletedAt, got %+v", failed)
	}
}

func TestCacheTaskStatusTransitions(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewCacheTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	task, err := cs.Create(CacheTaskSpec{MediaID: m.ID, Note: "first"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// pending -> running clears any completion time
	task, err = cs.MarkRunning(task.ID)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if task.Status != CacheRunning || task.CompletedAt != nil {
		t.Fatalf("unexpected running task: %+v", task)
	}

	// running -> done records CompletedAt and keeps the note
	task, err = cs.MarkDone(task.ID)
	if err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if task.Status != CacheDone || task.CompletedAt == nil {
		t.Fatalf("unexpected done task: %+v", task)
	}
	if task.Note != "first" {
		t.Fatalf("expected note preserved, got %q", task.Note)
	}
	doneAt := *task.CompletedAt

	// re-marking a done task is allowed and refreshes the timestamps
	time.Sleep(5 * time.Millisecond)
	task, err = cs.MarkDone(task.ID)
	if err != nil {
		t.Fatalf("re-mark done: %v", err)
	}
	if !task.CompletedAt.After(doneAt) {
		t.Fatal("expected CompletedAt refreshed by re-mark")
	}

	// a re-run moves the completed task back to running and clears
	// CompletedAt
	task, err = cs.MarkRunning(task.ID)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if task.Status != CacheRunning || task.CompletedAt != nil {
		t.Fatalf("unexpected re-run task: %+v", task)
	}

	// running -> failed records the failure note
	task, err = cs.MarkFailed(task.ID, "disk full")
	if err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if task.Status != CacheFailed || task.CompletedAt == nil || task.Note != "disk full" {
		t.Fatalf("unexpected failed task: %+v", task)
	}

	// an empty failure note leaves the existing note untouched
	task, err = cs.MarkFailed(task.ID, "")
	if err != nil {
		t.Fatalf("mark failed empty note: %v", err)
	}
	if task.Note != "disk full" {
		t.Fatalf("expected note kept, got %q", task.Note)
	}

	// marking a missing task is ErrNotFound
	if _, err := cs.MarkRunning("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := cs.MarkDone("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := cs.MarkFailed("missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCacheTaskUpdateStatusConsistency(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewCacheTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	task, err := cs.Create(CacheTaskSpec{MediaID: m.ID, Status: CacheDone})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.CompletedAt == nil {
		t.Fatal("expected CompletedAt for done task")
	}

	// a full update with an empty status resets to pending and clears the
	// stale completion time
	task, err = cs.Update(task.ID, CacheTaskSpec{MediaID: m.ID})
	if err != nil {
		t.Fatalf("update to pending: %v", err)
	}
	if task.Status != CachePending || task.CompletedAt != nil {
		t.Fatalf("expected pending task without completion, got %+v", task)
	}

	// an update straight into a terminal state records CompletedAt
	task, err = cs.Update(task.ID, CacheTaskSpec{MediaID: m.ID, Status: CacheDone})
	if err != nil {
		t.Fatalf("update to done: %v", err)
	}
	if task.Status != CacheDone || task.CompletedAt == nil {
		t.Fatalf("expected done task with CompletedAt, got %+v", task)
	}
}

func TestCacheTaskCountByStatus(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewCacheTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	if _, err := cs.Create(CacheTaskSpec{MediaID: m.ID}); err != nil { // pending
		t.Fatalf("create pending: %v", err)
	}
	if _, err := cs.Create(CacheTaskSpec{MediaID: m.ID, Status: CacheRunning}); err != nil {
		t.Fatalf("create running: %v", err)
	}
	if _, err := cs.Create(CacheTaskSpec{MediaID: m.ID, Status: CacheDone}); err != nil {
		t.Fatalf("create done: %v", err)
	}
	if _, err := cs.Create(CacheTaskSpec{MediaID: m.ID, Status: CacheFailed}); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	counts := cs.CountByStatus()
	want := map[CacheStatus]int{CachePending: 1, CacheRunning: 1, CacheDone: 1, CacheFailed: 1}
	for _, st := range []CacheStatus{CachePending, CacheRunning, CacheDone, CacheFailed} {
		if counts[st] != want[st] {
			t.Fatalf("expected %d task(s) with status %s, got %d", want[st], st, counts[st])
		}
	}

	// a second pending task written directly through the store is counted
	if err := s.Update(func(d *Data) error {
		d.CacheTasks = append(d.CacheTasks, &CacheTask{ID: newID(), MediaID: m.ID, Status: CachePending})
		return nil
	}); err != nil {
		t.Fatalf("seed pending task: %v", err)
	}
	// an unrecognized status, possible only in a hand-edited store, is
	// ignored by the count
	if err := s.Update(func(d *Data) error {
		for _, task := range d.CacheTasks {
			if task.Status == CacheRunning {
				task.Status = "weird"
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("corrupt status: %v", err)
	}

	counts = cs.CountByStatus()
	if counts[CachePending] != 2 {
		t.Fatalf("expected 2 pending tasks, got %d", counts[CachePending])
	}
	if counts[CacheRunning] != 0 {
		t.Fatalf("expected 0 running tasks, got %d", counts[CacheRunning])
	}
	if counts[CacheDone] != 1 || counts[CacheFailed] != 1 {
		t.Fatalf("unexpected counts: %+v", counts)
	}
}

func TestCacheTaskListNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewCacheTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	var ids []string
	for i := 0; i < 3; i++ {
		task, err := cs.Create(CacheTaskSpec{MediaID: m.ID})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, task.ID)
		time.Sleep(2 * time.Millisecond)
	}

	list := cs.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(list))
	}
	for i, task := range list {
		if task.ID != ids[len(ids)-1-i] {
			t.Fatalf("expected newest-first order, got %v", task.ID)
		}
	}
}

// TestCacheTaskReadsStoreData verifies the service reads cache tasks written
// directly through Store.Update (the document field the store layer owns).
func TestCacheTaskReadsStoreData(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewCacheTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	if err := s.Update(func(d *Data) error {
		d.CacheTasks = append(d.CacheTasks, &CacheTask{
			ID: "c1", MediaID: m.ID, Status: CacheRunning, Note: "seeded",
		})
		return nil
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	got, err := cs.Get("c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MediaID != m.ID || got.Status != CacheRunning || got.Note != "seeded" {
		t.Fatalf("unexpected seeded task: %+v", got)
	}
	if len(cs.List()) != 1 {
		t.Fatalf("expected 1 task in list, got %d", len(cs.List()))
	}
	if counts := cs.CountByStatus(); counts[CacheRunning] != 1 {
		t.Fatalf("expected 1 running task, got %+v", counts)
	}
}
