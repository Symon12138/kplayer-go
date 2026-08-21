package management

import (
	"errors"
	"testing"
	"time"
)

func TestTaskCRUD(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	task, err := ts.Create(TaskSpec{Name: "hourly", Type: TaskTypeInterval, Interval: 3600, MediaID: m.ID, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.ID == "" || task.Type != TaskTypeInterval || task.Interval != 3600 || !task.Enabled {
		t.Fatalf("unexpected task: %+v", task)
	}

	got, err := ts.Get(task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "hourly" || got.MediaID != m.ID {
		t.Fatalf("unexpected get: %+v", got)
	}
	if len(ts.List()) != 1 {
		t.Fatalf("expected 1 task, got %d", len(ts.List()))
	}

	if err := ts.SetEnabled(task.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = ts.Get(task.ID)
	if got.Enabled {
		t.Fatal("expected disabled task")
	}

	// Replace switches to a playlist target
	p, err := NewPlaylistService(s).Create("feed", "", []string{m.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := ts.Replace(task.ID, TaskSpec{Name: "cronjob", Type: TaskTypeCron, Cron: "0 9 * * *", PlaylistID: p.ID, Enabled: true})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if updated.Type != TaskTypeCron || updated.Cron != "0 9 * * *" || updated.PlaylistID != p.ID {
		t.Fatalf("unexpected replaced task: %+v", updated)
	}

	if err := ts.Delete(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Get(task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if len(ts.List()) != 0 {
		t.Fatal("expected empty task list")
	}
}

func TestTaskValidation(t *testing.T) {
	s := newTestStore(t)
	ts := NewTaskService(s)

	cases := []struct {
		name string
		spec TaskSpec
	}{
		{"empty name", TaskSpec{Name: "", Type: TaskTypeInterval, Interval: 10, MediaID: "x"}},
		{"bad interval", TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 0, MediaID: "x"}},
		{"bad cron", TaskSpec{Name: "t", Type: TaskTypeCron, Cron: "not a cron", MediaID: "x"}},
		{"both targets", TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: "x", PlaylistID: "y"}},
		{"no target", TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10}},
		{"bad type", TaskSpec{Name: "t", Type: "bogus", Interval: 10, MediaID: "x"}},
	}
	for _, c := range cases {
		if _, err := ts.Create(c.spec); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: expected ErrInvalid, got %v", c.name, err)
		}
	}
}

func TestTaskRefsMustExist(t *testing.T) {
	s := newTestStore(t)
	ts := NewTaskService(s)

	// missing media reference
	if _, err := ts.Create(TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing media, got %v", err)
	}
	// missing playlist reference
	if _, err := ts.Create(TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, PlaylistID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing playlist, got %v", err)
	}
}

func TestTaskRejectWhenMediaDeleted(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID}); err != nil {
		t.Fatal(err)
	}
	// media referenced by a task cannot be deleted
	if err := ms.Delete(m.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse, got %v", err)
	}
}

func TestTaskSceneTemplateRef(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	sts := NewSceneTemplateService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	tpl, err := sts.Create(SceneTemplateSpec{Name: "watermark", Kind: SceneWatermark})
	if err != nil {
		t.Fatal(err)
	}

	// a missing template reference is rejected
	if _, err := ts.Create(TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID, SceneTemplateID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing template, got %v", err)
	}

	// create with a template reference round-trips through get
	task, err := ts.Create(TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID, SceneTemplateID: tpl.ID})
	if err != nil {
		t.Fatal(err)
	}
	if task.SceneTemplateID != tpl.ID {
		t.Fatalf("expected template id %q, got %q", tpl.ID, task.SceneTemplateID)
	}
	got, err := ts.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SceneTemplateID != tpl.ID {
		t.Fatalf("template id lost after get: %q", got.SceneTemplateID)
	}

	// replace moves the reference to another template
	tpl2, err := sts.Create(SceneTemplateSpec{Name: "clock", Kind: SceneClock})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := ts.Replace(task.ID, TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID, SceneTemplateID: tpl2.ID})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if updated.SceneTemplateID != tpl2.ID {
		t.Fatalf("expected template id %q after replace, got %q", tpl2.ID, updated.SceneTemplateID)
	}

	// replace with a missing template is rejected and leaves the task unchanged
	if _, err := ts.Replace(task.ID, TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID, SceneTemplateID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing template on replace, got %v", err)
	}
	got, _ = ts.Get(task.ID)
	if got.SceneTemplateID != tpl2.ID {
		t.Fatalf("failed replace changed the reference: %q", got.SceneTemplateID)
	}

	// replace with an empty reference clears it
	updated, err = ts.Replace(task.ID, TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID})
	if err != nil {
		t.Fatalf("replace clearing template: %v", err)
	}
	if updated.SceneTemplateID != "" {
		t.Fatalf("expected cleared template id, got %q", updated.SceneTemplateID)
	}
	got, _ = ts.Get(task.ID)
	if got.SceneTemplateID != "" {
		t.Fatalf("template id not cleared after replace: %q", got.SceneTemplateID)
	}
}

func TestTaskPreservesLastRun(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	task, err := ts.Create(TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if task.LastRun != nil {
		t.Fatal("expected nil LastRun initially")
	}
	if err := ts.Update(task.ID, func(t *ScheduleTask) error {
		now := time.Now()
		t.LastRun = &now
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := ts.Get(task.ID)
	if got.LastRun == nil {
		t.Fatal("expected LastRun to persist")
	}
}

func TestTaskPriorityValidationAndDefaults(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	// unknown priorities are rejected
	if _, err := ts.Create(TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID, Priority: "urgent"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown priority, got %v", err)
	}
	if _, err := ts.Replace("nope", TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID, Priority: "urgent"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown priority on replace, got %v", err)
	}

	// the zero value defaults to normal
	task, err := ts.Create(TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if task.Priority != PriorityNormal {
		t.Fatalf("expected default priority normal, got %q", task.Priority)
	}
	got, err := ts.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != PriorityNormal {
		t.Fatalf("expected persisted priority normal, got %q", got.Priority)
	}

	// explicit priorities round-trip
	crit, err := ts.Create(TaskSpec{Name: "c", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID, Priority: PriorityCritical})
	if err != nil {
		t.Fatal(err)
	}
	if crit.Priority != PriorityCritical {
		t.Fatalf("expected critical priority, got %q", crit.Priority)
	}

	// Replace carries the priority
	updated, err := ts.Replace(task.ID, TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID, Priority: PriorityImportant})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Priority != PriorityImportant {
		t.Fatalf("expected important priority after replace, got %q", updated.Priority)
	}
}

func TestTaskInterruptFields(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	// negative durations are rejected
	if _, err := ts.Create(TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID, Interrupt: true, InterruptDuration: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for negative duration, got %v", err)
	}

	// interrupt fields round-trip through create and get
	task, err := ts.Create(TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID, Interrupt: true, InterruptDuration: 30})
	if err != nil {
		t.Fatal(err)
	}
	if !task.Interrupt || task.InterruptDuration != 30 {
		t.Fatalf("unexpected interrupt task: %+v", task)
	}
	got, err := ts.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Interrupt || got.InterruptDuration != 30 {
		t.Fatalf("unexpected persisted interrupt task: %+v", got)
	}

	// Replace overwrites the interrupt configuration
	updated, err := ts.Replace(task.ID, TaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, MediaID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Interrupt || updated.InterruptDuration != 0 {
		t.Fatalf("expected interrupt fields cleared by replace, got %+v", updated)
	}
}
