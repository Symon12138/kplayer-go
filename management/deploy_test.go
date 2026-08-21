package management

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestExpandPlaceholders verifies placeholder substitution of a single media
// id entry: single, multiple, repeated and mid-string placeholders are all
// replaced in one pass; values are injected literally; entries without
// placeholders pass through unchanged even when params carries unrelated
// keys; and text that does not match the "${key}" syntax (an empty key or a
// key with characters outside letters/digits/underscores) stays literal.
func TestExpandPlaceholders(t *testing.T) {
	tests := []struct {
		name   string
		entry  string
		params map[string]string
		want   string
	}{
		{name: "single placeholder", entry: "${a}", params: map[string]string{"a": "m1"}, want: "m1"},
		{name: "multiple placeholders", entry: "${a}${b}", params: map[string]string{"a": "1", "b": "2"}, want: "12"},
		{name: "repeated placeholder", entry: "${a}-${a}", params: map[string]string{"a": "x"}, want: "x-x"},
		{name: "mid-string placeholder", entry: "clip-${a}-end", params: map[string]string{"a": "42"}, want: "clip-42-end"},
		{name: "literal id", entry: "m-123", params: map[string]string{"a": "1"}, want: "m-123"},
		{name: "empty entry", entry: "", params: nil, want: ""},
		{name: "empty key stays literal", entry: "${}", params: map[string]string{}, want: "${}"},
		{name: "invalid key stays literal", entry: "${a-b}", params: map[string]string{"a-b": "x"}, want: "${a-b}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandPlaceholders(tt.entry, tt.params)
			if err != nil {
				t.Fatalf("expand %q: %v", tt.entry, err)
			}
			if got != tt.want {
				t.Fatalf("expanded %q, want %q", got, tt.want)
			}
		})
	}
}

// TestExpandPlaceholdersMissingParameter verifies that a placeholder without
// a matching parameter fails with ErrInvalid and names the missing key.
func TestExpandPlaceholdersMissingParameter(t *testing.T) {
	for _, tt := range []struct {
		entry   string
		params  map[string]string
		missing string
	}{
		{entry: "${x}", params: map[string]string{}, missing: "x"},
		{entry: "a-${x}", params: map[string]string{"y": "1"}, missing: "x"},
		{entry: "${x}-${y}", params: map[string]string{"x": "1"}, missing: "y"},
	} {
		_, err := expandPlaceholders(tt.entry, tt.params)
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for %q, got %v", tt.entry, err)
		}
		if !strings.Contains(err.Error(), "missing parameter") || !strings.Contains(err.Error(), tt.missing) {
			t.Fatalf("expected missing-parameter error naming %q, got %v", tt.missing, err)
		}
	}
}

// TestDeploySuccess verifies the happy path: placeholders expand against
// params, literal entries are kept, the playlist is created with the
// expanded media ids in order, one scene template per kind is created
// enabled and without params, and the task is created bound to the new
// playlist.
func TestDeploySuccess(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")
	m3 := mustAddMedia(t, ms, "/v/3.mp4")

	tpl := &IndustryTemplate{
		Name:              "News",
		PlaylistName:      "News Feed",
		MediaPlaceholders: []string{"${lead}", m2.ID, "${tail}"},
		SceneKinds:        []SceneKind{SceneLogo, SceneClock},
		Task:              &IndustryTaskSpec{Name: "news ticker", Type: TaskTypeInterval, Interval: 300, Enabled: true},
	}
	res, err := Deploy(tpl, map[string]string{"lead": m1.ID, "tail": m3.ID}, s)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Playlist: created with the template name, expanded items in order,
	// no description, no loop.
	if res.PlaylistID == "" || res.Playlist == nil || res.PlaylistID != res.Playlist.ID {
		t.Fatalf("expected playlist id/back-reference, got %+v", res)
	}
	p := res.Playlist
	if p.Name != "News Feed" || p.Desc != "" || p.Loop {
		t.Fatalf("unexpected playlist: %+v", p)
	}
	wantItems := []string{m1.ID, m2.ID, m3.ID}
	if !reflect.DeepEqual(p.MediaIDs(), wantItems) {
		t.Fatalf("unexpected playlist items %v, want %v", p.MediaIDs(), wantItems)
	}

	// Scenes: one per kind, enabled, without params, ids back-referenced.
	if len(res.SceneTemplateIDs) != 2 || len(res.Scenes) != 2 {
		t.Fatalf("expected 2 scenes, got %+v", res)
	}
	for i, kind := range []SceneKind{SceneLogo, SceneClock} {
		sc := res.Scenes[i]
		if sc.ID == "" || sc.ID != res.SceneTemplateIDs[i] {
			t.Fatalf("expected scene id back-reference, got %+v", res)
		}
		if sc.Name != "News - "+string(kind) || sc.Kind != kind || !sc.Enabled || sc.Params != nil {
			t.Fatalf("unexpected scene %d: %+v", i, sc)
		}
	}

	// Task: created from the template spec and bound to the new playlist.
	if res.TaskID == "" || res.Task == nil || res.TaskID != res.Task.ID {
		t.Fatalf("expected task id/back-reference, got %+v", res)
	}
	task := res.Task
	if task.Name != "news ticker" || task.Type != TaskTypeInterval || task.Interval != 300 || task.Cron != "" || !task.Enabled {
		t.Fatalf("unexpected task: %+v", task)
	}
	if task.PlaylistID != res.PlaylistID {
		t.Fatalf("task must target the deployed playlist, got %q want %q", task.PlaylistID, res.PlaylistID)
	}

	// Store state matches the result.
	if len(NewPlaylistService(s).List()) != 1 {
		t.Fatalf("expected 1 playlist in store")
	}
	if len(NewSceneTemplateService(s).List()) != 2 {
		t.Fatalf("expected 2 scene templates in store")
	}
	if len(NewTaskService(s).List()) != 1 {
		t.Fatalf("expected 1 task in store")
	}
}

// TestDeployMissingMedia verifies that an expanded id referencing no media
// fails with ErrNotFound. Media validation runs before the playlist is
// created, so the failing deployment creates nothing and the partial result
// carries no resources.
func TestDeployMissingMedia(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	// A placeholder expanding to a missing id.
	tpl := &IndustryTemplate{
		Name:              "News",
		PlaylistName:      "Feed",
		MediaPlaceholders: []string{m.ID, "${ghost}"},
		SceneKinds:        []SceneKind{SceneLogo},
		Task:              &IndustryTaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, Enabled: true},
	}
	res, err := Deploy(tpl, map[string]string{"ghost": "nope"}, s)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if res.Playlist != nil || res.PlaylistID != "" || len(res.Scenes) != 0 || len(res.SceneTemplateIDs) != 0 || res.Task != nil || res.TaskID != "" {
		t.Fatalf("expected empty partial result, got %+v", res)
	}
	if len(NewPlaylistService(s).List()) != 0 || len(NewSceneTemplateService(s).List()) != 0 || len(NewTaskService(s).List()) != 0 {
		t.Fatal("expected no resources after media validation failure")
	}

	// A literal entry referencing a missing id fails the same way.
	tpl2 := &IndustryTemplate{Name: "News2", PlaylistName: "Feed2", MediaPlaceholders: []string{"nope"}}
	if _, err := Deploy(tpl2, nil, s); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for literal missing id, got %v", err)
	}
	if len(NewPlaylistService(s).List()) != 0 {
		t.Fatal("expected no playlist after literal missing id")
	}
}

// TestDeployMissingParameter verifies that a placeholder without a matching
// parameter fails at the deployment level with ErrInvalid, before anything
// is created.
func TestDeployMissingParameter(t *testing.T) {
	s := newTestStore(t)
	tpl := &IndustryTemplate{Name: "News", PlaylistName: "Feed", MediaPlaceholders: []string{"${ghost}"}}
	res, err := Deploy(tpl, map[string]string{"other": "1"}, s)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
	if res.Playlist != nil || len(res.Scenes) != 0 || res.Task != nil {
		t.Fatalf("expected empty partial result, got %+v", res)
	}
	if len(NewPlaylistService(s).List()) != 0 {
		t.Fatal("expected no playlist after missing parameter")
	}
}

// TestDeployPartialDeployment documents the no-rollback semantics: when a
// step after the playlist fails, the error is returned together with the
// partial result and the resources already created stay in the store.
func TestDeployPartialDeployment(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	// Scene creation fails (unknown kind) after the playlist was created:
	// the playlist remains, scenes and task were never reached.
	tpl := &IndustryTemplate{
		Name:              "News",
		PlaylistName:      "Feed",
		MediaPlaceholders: []string{m.ID},
		SceneKinds:        []SceneKind{"bogus"},
		Task:              &IndustryTaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, Enabled: true},
	}
	res, err := Deploy(tpl, nil, s)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
	if res.Playlist == nil || res.PlaylistID == "" || res.PlaylistID != res.Playlist.ID {
		t.Fatalf("expected partial result with the playlist, got %+v", res)
	}
	if len(res.Scenes) != 0 || len(res.SceneTemplateIDs) != 0 || res.Task != nil || res.TaskID != "" {
		t.Fatalf("expected no scenes/task in partial result, got %+v", res)
	}
	playlists := NewPlaylistService(s).List()
	if len(playlists) != 1 || playlists[0].ID != res.PlaylistID {
		t.Fatalf("expected the created playlist to remain, got %d playlists", len(playlists))
	}
	if len(NewSceneTemplateService(s).List()) != 0 || len(NewTaskService(s).List()) != 0 {
		t.Fatal("expected no scenes or tasks to remain")
	}

	// Task creation fails (invalid spec) after the playlist and scenes were
	// created: those stay, no task is created.
	tpl2 := &IndustryTemplate{
		Name:              "Sports",
		PlaylistName:      "Sports Feed",
		MediaPlaceholders: []string{m.ID},
		SceneKinds:        []SceneKind{SceneLogo},
		Task:              &IndustryTaskSpec{Name: "t2", Type: TaskTypeInterval, Interval: 0, Enabled: true},
	}
	res2, err := Deploy(tpl2, nil, s)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
	if res2.Playlist == nil || len(res2.Scenes) != 1 || res2.Task != nil {
		t.Fatalf("expected partial result with playlist and scenes, got %+v", res2)
	}
	if len(NewPlaylistService(s).List()) != 2 {
		t.Fatalf("expected 2 playlists, got %d", len(NewPlaylistService(s).List()))
	}
	if len(NewSceneTemplateService(s).List()) != 1 {
		t.Fatalf("expected 1 scene template, got %d", len(NewSceneTemplateService(s).List()))
	}
	if len(NewTaskService(s).List()) != 0 {
		t.Fatal("expected no tasks to remain")
	}
}

// TestDeployWithoutTask verifies that a template without a task deploys the
// playlist and scenes only, with empty task fields in the result.
func TestDeployWithoutTask(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	tpl := &IndustryTemplate{
		Name:              "News",
		PlaylistName:      "Feed",
		MediaPlaceholders: []string{m.ID},
		SceneKinds:        []SceneKind{SceneLogo},
	}
	res, err := Deploy(tpl, nil, s)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if res.TaskID != "" || res.Task != nil {
		t.Fatalf("expected no task, got %+v", res)
	}
	if res.Playlist == nil || len(res.Scenes) != 1 {
		t.Fatalf("expected playlist and one scene, got %+v", res)
	}
	if len(NewTaskService(s).List()) != 0 {
		t.Fatal("expected no tasks in store")
	}
	if len(NewPlaylistService(s).List()) != 1 || len(NewSceneTemplateService(s).List()) != 1 {
		t.Fatal("expected playlist and scene in store")
	}
}

// TestDeployTwice verifies that deploying the same template twice creates
// two independent playlists. With scene kinds the second deployment stops at
// the scene step: the scene name "<template.Name> - <kind>" already exists,
// so SceneTemplateService.Create fails with ErrExists and the second
// playlist stays behind (partial deployment). Without scene kinds both
// deployments complete and each task is bound to its own playlist.
func TestDeployTwice(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	tpl := &IndustryTemplate{
		Name:              "News",
		PlaylistName:      "Feed",
		MediaPlaceholders: []string{m.ID},
		SceneKinds:        []SceneKind{SceneLogo},
		Task:              &IndustryTaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 10, Enabled: true},
	}
	first, err := Deploy(tpl, nil, s)
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	second, err := Deploy(tpl, nil, s)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate scene, got %v", err)
	}
	if second.Playlist == nil || second.PlaylistID == "" || second.PlaylistID == first.PlaylistID {
		t.Fatalf("expected a second independent playlist, got %+v", second)
	}
	if len(second.Scenes) != 0 || second.Task != nil {
		t.Fatalf("expected no scenes/task from the failed second deploy, got %+v", second)
	}
	playlists := NewPlaylistService(s).List()
	if len(playlists) != 2 || playlists[0].ID == playlists[1].ID {
		t.Fatalf("expected 2 distinct playlists, got %d", len(playlists))
	}
	if len(NewSceneTemplateService(s).List()) != 1 || len(NewTaskService(s).List()) != 1 {
		t.Fatal("expected only the first deployment's scenes and task")
	}

	// Without scene kinds both deployments succeed fully.
	tplNoScenes := &IndustryTemplate{
		Name:              "Loop",
		PlaylistName:      "Loop Feed",
		MediaPlaceholders: []string{m.ID},
		Task:              &IndustryTaskSpec{Name: "loop task", Type: TaskTypeInterval, Interval: 60, Enabled: true},
	}
	a, err := Deploy(tplNoScenes, nil, s)
	if err != nil {
		t.Fatalf("deploy a: %v", err)
	}
	b, err := Deploy(tplNoScenes, nil, s)
	if err != nil {
		t.Fatalf("deploy b: %v", err)
	}
	if a.PlaylistID == "" || b.PlaylistID == "" || a.PlaylistID == b.PlaylistID {
		t.Fatalf("expected two independent playlists, got %q and %q", a.PlaylistID, b.PlaylistID)
	}
	tasks := NewTaskService(s).List()
	var taskA, taskB *ScheduleTask
	for _, task := range tasks {
		switch task.PlaylistID {
		case a.PlaylistID:
			taskA = task
		case b.PlaylistID:
			taskB = task
		}
	}
	if taskA == nil || taskB == nil {
		t.Fatalf("expected a task bound to each playlist, got %d tasks", len(tasks))
	}
	if taskA.PlaylistID != a.PlaylistID || taskB.PlaylistID != b.PlaylistID {
		t.Fatalf("task/playlist bindings mismatch: %q/%q", taskA.PlaylistID, taskB.PlaylistID)
	}
}

// TestDeployEmptyCollections verifies the empty-list boundaries: nil and
// explicit empty MediaPlaceholders/SceneKinds (and no task) deploy an empty
// playlist and nothing else.
func TestDeployEmptyCollections(t *testing.T) {
	s := newTestStore(t)

	for _, tpl := range []*IndustryTemplate{
		{Name: "Empty", PlaylistName: "Empty Feed"},
		{Name: "Empty2", PlaylistName: "Empty Feed 2", MediaPlaceholders: []string{}, SceneKinds: []SceneKind{}},
	} {
		res, err := Deploy(tpl, nil, s)
		if err != nil {
			t.Fatalf("deploy %q: %v", tpl.Name, err)
		}
		if res.Playlist == nil || res.PlaylistID == "" || len(res.Playlist.Items) != 0 {
			t.Fatalf("expected an empty playlist, got %+v", res)
		}
		if res.Playlist.Name != tpl.PlaylistName {
			t.Fatalf("unexpected playlist name %q", res.Playlist.Name)
		}
		if len(res.Scenes) != 0 || len(res.SceneTemplateIDs) != 0 || res.Task != nil || res.TaskID != "" {
			t.Fatalf("expected no scenes or task, got %+v", res)
		}
	}
	if len(NewPlaylistService(s).List()) != 2 {
		t.Fatalf("expected 2 playlists, got %d", len(NewPlaylistService(s).List()))
	}
	if len(NewSceneTemplateService(s).List()) != 0 || len(NewTaskService(s).List()) != 0 {
		t.Fatal("expected no scenes or tasks in store")
	}
}

// TestDeployNilTemplate verifies that a nil template is rejected with
// ErrInvalid.
func TestDeployNilTemplate(t *testing.T) {
	s := newTestStore(t)
	if _, err := Deploy(nil, nil, s); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}
