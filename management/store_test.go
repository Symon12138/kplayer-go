package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// newTestStore opens a fresh store in a temp directory for one test.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// mustAddMedia registers a media entry and fails the test on error.
func mustAddMedia(t *testing.T, ms *MediaService, path string) *Media {
	t.Helper()
	m := &Media{Name: filepath.Base(path), Path: path}
	if err := ms.Add(m); err != nil {
		t.Fatalf("add media %q: %v", path, err)
	}
	return m
}

// TestStoreSaveIsNoopWhenClean verifies that Save does not rewrite the file
// when nothing changed since the last write.
func TestStoreSaveIsNoopWhenClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *Data) error {
		d.Alarms = append(d.Alarms, &Alarm{ID: "a1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("expected Save to be a no-op when the document is clean")
	}
}

// TestStoreSavePersistsDirtyDocument verifies Save writes when the store is
// marked dirty (simulating a future direct-mutation path).
func TestStoreSavePersistsDirtyDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.markDirty()
	if err := s.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("expected Save to persist a dirty document")
	}
}

// TestStoreConcurrentWriters exercises concurrent Update and Save calls to
// ensure they never corrupt memory or the on-disk document.
func TestStoreConcurrentWriters(t *testing.T) {
	s := newTestStore(t)
	const writers = 50

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Update(func(d *Data) error {
				d.Alarms = append(d.Alarms, &Alarm{ID: newID()})
				return nil
			}); err != nil {
				t.Errorf("update: %v", err)
			}
		}()
	}
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Save(); err != nil {
				t.Errorf("save: %v", err)
			}
		}()
	}
	wg.Wait()

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Alarms) != writers {
		t.Fatalf("expected %d alarms in memory, got %d", writers, len(snap.Alarms))
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	disk := &Data{}
	if err := json.Unmarshal(raw, disk); err != nil {
		t.Fatalf("store file corrupted by concurrent writes: %v", err)
	}
	if len(disk.Alarms) != writers {
		t.Fatalf("expected %d alarms on disk, got %d", writers, len(disk.Alarms))
	}
}

// TestStoreFailedUpdateRollsBack verifies that an Update whose callback
// returns an error leaves both memory and disk untouched: the working copy is
// discarded and no write happens.
func TestStoreFailedUpdateRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *Data) error {
		d.Alarms = append(d.Alarms, &Alarm{ID: "a1", Title: "one"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The callback mutates its working copy, then fails.
	err = s.Update(func(d *Data) error {
		d.Alarms = append(d.Alarms, &Alarm{ID: "a2", Title: "two"})
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the failing update to return an error")
	}

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Alarms) != 1 || snap.Alarms[0].ID != "a1" {
		t.Fatalf("memory mutated by failed update: %+v", snap.Alarms)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed update rewrote the store file on disk")
	}
}

// TestStoreNoopUpdateDoesNotRewrite verifies the errNoop path: an update that
// reports "no net change" is a success but must not rewrite the store file.
func TestStoreNoopUpdateDoesNotRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *Data) error {
		d.Alarms = append(d.Alarms, &Alarm{ID: "a1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Update(func(d *Data) error {
		return errNoop
	}); err != nil {
		t.Fatalf("errNoop update returned error: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("errNoop update rewrote the store file")
	}
}

// TestStoreReopenRoundTrip verifies that everything written through Update
// survives a close/reopen cycle: OpenStore must reload the full document.
func TestStoreReopenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *Data) error {
		d.Media = append(d.Media, &Media{ID: "m1", Name: "clip", Path: "/v/clip.mp4"})
		d.Playlists = append(d.Playlists, &Playlist{
			ID:    "p1",
			Name:  "fav",
			Items: []*PlaylistItem{{MediaID: "m1"}},
		})
		d.Alarms = append(d.Alarms, &Alarm{ID: "al1", Title: "warn", Status: AlarmStatusActive})
		d.Tasks = append(d.Tasks, &ScheduleTask{
			ID:   "t1",
			Name: "nightly",
			Type: TaskTypeCron,
			Cron: "0 2 * * *",
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	snap, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Media) != 1 || snap.Media[0].ID != "m1" || snap.Media[0].Name != "clip" {
		t.Fatalf("media did not round-trip: %+v", snap.Media)
	}
	if len(snap.Playlists) != 1 || snap.Playlists[0].Name != "fav" ||
		len(snap.Playlists[0].Items) != 1 || snap.Playlists[0].Items[0].MediaID != "m1" {
		t.Fatalf("playlist did not round-trip: %+v", snap.Playlists)
	}
	if len(snap.Alarms) != 1 || snap.Alarms[0].Title != "warn" || !snap.Alarms[0].IsActive() {
		t.Fatalf("alarm did not round-trip: %+v", snap.Alarms)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].Cron != "0 2 * * *" || snap.Tasks[0].Type != TaskTypeCron {
		t.Fatalf("task did not round-trip: %+v", snap.Tasks)
	}
}

// TestStoreFreshOpenIsEmptyAndReopen verifies that opening a brand-new path
// creates an empty document on disk immediately, and that reopening it yields
// the same empty document (no stale data).
func TestStoreFreshOpenIsEmptyAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Media)+len(snap.Playlists)+len(snap.Alarms)+len(snap.Tasks) != 0 {
		t.Fatalf("expected a fresh store to be empty, got %+v", snap)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected fresh store file to be created: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen fresh store: %v", err)
	}
	snap2, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap2.Media)+len(snap2.Playlists)+len(snap2.Alarms)+len(snap2.Tasks) != 0 {
		t.Fatal("fresh store reopened with stale data")
	}
}

// TestStoreCreatesNestedParentDir verifies writeFileAtomic creates the store
// file's parent directory chain when it does not exist yet.
func TestStoreCreatesNestedParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deep", "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open with missing parent dirs: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("parent dirs were not created: %v", err)
	}
	if err := s.Update(func(d *Data) error {
		d.Alarms = append(d.Alarms, &Alarm{ID: "a1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Alarms) != 1 {
		t.Fatalf("data lost after reopen through created parent dir: %+v", snap.Alarms)
	}
}

// TestStoreBareFilenamePath covers the "empty parent directory" case: a bare
// filename has filepath.Dir == ".", and writes must still go through the
// current working directory correctly.
func TestStoreBareFilenamePath(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Error(err)
		}
	}()

	s, err := OpenStore("store.json") // filepath.Dir("store.json") == "."
	if err != nil {
		t.Fatalf("open bare-filename store: %v", err)
	}
	if err := s.Update(func(d *Data) error {
		d.Alarms = append(d.Alarms, &Alarm{ID: "a1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "store.json")); err != nil {
		t.Fatalf("bare-filename store file missing: %v", err)
	}
	reopened, err := OpenStore("store.json")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Alarms) != 1 {
		t.Fatalf("bare-filename store failed round-trip: %+v", snap.Alarms)
	}
}

// TestStoreAtomicWriteLeavesNoTempFiles verifies the atomic write path cleans
// up its temp file after a successful rename (no ".tmp-*" litter).
func TestStoreAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Update(func(d *Data) error {
			d.Alarms = append(d.Alarms, &Alarm{ID: newID()})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("leftover temp file after atomic write: %s", e.Name())
		}
	}
}

// TestStoreReplaceSwapsDocument verifies Replace installs the given document
// as the live one: both View and Snapshot must observe the replacement and
// the previous contents must be gone.
func TestStoreReplaceSwapsDocument(t *testing.T) {
	s := newTestStore(t)
	if err := s.Update(func(d *Data) error {
		d.Media = append(d.Media, &Media{ID: "m1", Name: "old", Path: "/v/old.mp4"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	replacement := &Data{Media: []*Media{{ID: "m2", Name: "new", Path: "/v/new.mp4"}}}
	if err := s.Replace(replacement); err != nil {
		t.Fatalf("replace: %v", err)
	}

	s.View(func(d *Data) {
		if len(d.Media) != 1 || d.Media[0].ID != "m2" {
			t.Fatalf("View did not observe the replaced document: %+v", d.Media)
		}
	})
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Media) != 1 || snap.Media[0].ID != "m2" || snap.Media[0].Name != "new" {
		t.Fatalf("Snapshot did not observe the replaced document: %+v", snap.Media)
	}
}

// TestStoreReplaceNilRejects verifies Replace(nil) reports ErrInvalid and
// leaves both the live document and the store file untouched.
func TestStoreReplaceNilRejects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *Data) error {
		d.Media = append(d.Media, &Media{ID: "m1", Path: "/v/a.mp4"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Replace(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Replace(nil) = %v, want ErrInvalid", err)
	}

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Media) != 1 || snap.Media[0].ID != "m1" {
		t.Fatalf("Replace(nil) changed the live document: %+v", snap.Media)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Replace(nil) rewrote the store file")
	}
}

// TestStoreReplacePersistsToDisk verifies Replace writes the new document
// through the same atomic persistence path as Update, so the file bytes
// reflect it and a reopened store serves it.
func TestStoreReplacePersistsToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	replacement := &Data{Users: []*User{{ID: "u1", Username: "root"}}}
	if err := s.Replace(replacement); err != nil {
		t.Fatalf("replace: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"u1"`)) {
		t.Fatalf("store file does not contain the replaced content: %s", raw)
	}
	disk := &Data{}
	if err := json.Unmarshal(raw, disk); err != nil {
		t.Fatalf("store file after Replace is not valid JSON: %v", err)
	}
	if len(disk.Users) != 1 || disk.Users[0].ID != "u1" {
		t.Fatalf("store file does not hold the replaced document: %+v", disk.Users)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Users) != 1 || snap.Users[0].Username != "root" {
		t.Fatalf("reopened store lost the replaced document: %+v", snap.Users)
	}
}

// TestStoreReplaceConcurrentWithUpdate races Replace against concurrent
// Updates. Every operation is serialized by the write lock, so nothing may
// be lost or corrupted: the replaced media must survive (updates never touch
// it) and memory and disk must agree exactly at the end.
func TestStoreReplaceConcurrentWithUpdate(t *testing.T) {
	s := newTestStore(t)
	const updaters = 25

	var wg sync.WaitGroup
	for i := 0; i < updaters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Update(func(d *Data) error {
				d.Alarms = append(d.Alarms, &Alarm{ID: newID()})
				return nil
			}); err != nil {
				t.Errorf("update: %v", err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.Replace(&Data{Media: []*Media{{ID: "m-r", Name: "replaced", Path: "/v/replaced.mp4"}}}); err != nil {
			t.Errorf("replace: %v", err)
		}
	}()
	wg.Wait()

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Media) != 1 || snap.Media[0].ID != "m-r" {
		t.Fatalf("replaced media lost under concurrency: %+v", snap.Media)
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	disk := &Data{}
	if err := json.Unmarshal(raw, disk); err != nil {
		t.Fatalf("store file corrupted by concurrent replace/update: %v", err)
	}
	if len(disk.Media) != 1 || disk.Media[0].ID != "m-r" {
		t.Fatalf("replaced media not persisted under concurrency: %+v", disk.Media)
	}
	if len(disk.Alarms) != len(snap.Alarms) {
		t.Fatalf("memory and disk disagree after concurrent replace/update: %d vs %d alarms",
			len(snap.Alarms), len(disk.Alarms))
	}
}

// TestStoreLegacyFileWithoutConfigCollections verifies that a store file
// written before the config snapshot/template collections existed (no
// configSnapshots/configTemplates keys) still opens and deserializes: the
// absent keys leave the new fields nil instead of failing. The fields are
// inspected by name via reflection so this test does not depend on the
// concrete ConfigSnapshot/ConfigTemplate types.
func TestStoreLegacyFileWithoutConfigCollections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	legacy := "{\n  \"media\": [],\n  \"users\": [],\n  \"updated_at\": \"2026-01-01T00:00:00Z\"\n}\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	v := reflect.ValueOf(snap).Elem()
	for _, name := range []string{"ConfigSnapshots", "ConfigTemplates"} {
		f := v.FieldByName(name)
		if !f.IsValid() {
			t.Fatalf("Data has no %s field", name)
		}
		if !f.IsNil() {
			t.Fatalf("expected %s to be nil after opening a legacy file, got %v", name, f)
		}
	}
}
