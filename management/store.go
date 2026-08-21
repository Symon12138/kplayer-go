package management

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sentinel errors shared by the management services.
var (
	// ErrNotFound is returned when a requested entity does not exist.
	ErrNotFound = errors.New("management: not found")
	// ErrExists is returned when creating an entity that conflicts with an
	// existing one (for example a media item whose path is already tracked).
	ErrExists = errors.New("management: already exists")
	// ErrInUse is returned when deleting an entity that is still referenced
	// by another entity (for example media referenced by a playlist).
	ErrInUse = errors.New("management: entity is in use")
	// ErrInvalid is returned when an entity fails validation.
	ErrInvalid = errors.New("management: invalid entity")
	// ErrNoPlayer is returned by the scheduler when a task fires but no
	// Player has been attached.
	ErrNoPlayer = errors.New("management: no player attached")
	// ErrAlreadyRunning is returned when starting a scheduler that is
	// already running.
	ErrAlreadyRunning = errors.New("management: already running")
	// ErrProbeUnavailable is returned by the media probe when the ffprobe
	// binary cannot be found; callers should treat it as a soft failure.
	ErrProbeUnavailable = errors.New("management: ffprobe not available")

	// errNoop is an internal sentinel an Update callback can return to signal
	// that it made no net change to the document. Store.Update treats it as a
	// success that is neither persisted nor bumps UpdatedAt, so operations
	// such as adding an already-present playlist item stop rewriting the
	// store file.
	errNoop = errors.New("management: no change")
)

// Data is the root JSON document persisted by Store. It holds every
// collection of the management backend: media, playlists, alarms, output
// groups, scheduled tasks, output failovers, health policies, cache tasks,
// scene templates, industry templates, smart rules, suggestions, webhook
// subscriptions, webhook deliveries, audit logs, users, login sessions,
// config snapshots, config templates, nodes, instances, remote commands and
// playback events.
// The
// document is intentionally one file: for a single-machine console the
// dataset is small and a single atomic rewrite keeps the on-disk state
// trivially consistent.
type Data struct {
	Media                []*Media               `json:"media"`
	Playlists            []*Playlist            `json:"playlists"`
	Alarms               []*Alarm               `json:"alarms"`
	OutputGroups         []*OutputGroup         `json:"outputGroups"`
	Tasks                []*ScheduleTask        `json:"tasks"`
	Failovers            []*OutputFailover      `json:"outputFailovers"`
	HealthPolicies       []*HealthPolicy        `json:"healthPolicies"`
	CacheTasks           []*CacheTask           `json:"cacheTasks"`
	SceneTemplates       []*SceneTemplate       `json:"sceneTemplates"`
	WebhookSubscriptions []*WebhookSubscription `json:"webhookSubscriptions"`
	WebhookDeliveries    []*WebhookDelivery     `json:"webhookDeliveries"`
	AuditLogs            []*AuditEntry          `json:"auditLogs"`
	Users                []*User                `json:"users"`
	Sessions             []*Session             `json:"sessions"`
	ConfigSnapshots      []*ConfigSnapshot      `json:"configSnapshots"`
	ConfigTemplates      []*ConfigTemplate      `json:"configTemplates"`
	Nodes                []*Node                `json:"nodes"`
	Instances            []*Instance            `json:"instances"`
	RemoteCommands       []*RemoteCommand       `json:"remoteCommands"`
	PlayEvents           []*PlayEvent           `json:"playEvents"`
	IndustryTemplates    []*IndustryTemplate    `json:"industryTemplates"`
	SmartRules           []*SmartRule           `json:"smartRules"`
	Suggestions          []*Suggestion          `json:"suggestions"`
	UpdatedAt            time.Time              `json:"updated_at"`
}

// clone returns a deep copy of d. JSON round-tripping keeps clone correct
// even when new fields are added to the model types.
func (d *Data) clone() (*Data, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	out := &Data{}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Store is a thread-safe, JSON-persisted data container.
//
// All access goes through View (read-only snapshot under RLock) or Update
// (copy-on-write mutation under Lock). Update applies fn to a deep copy of
// the document; only when fn succeeds is the copy written to disk atomically
// and swapped in, so a failing update leaves both memory and disk untouched.
type Store struct {
	mu    sync.RWMutex
	path  string
	data  *Data
	dirty bool
}

// OpenStore opens (or creates) the JSON store at path. If the file does not
// exist yet it is created with an empty document; if it exists but is not
// valid JSON an error is returned and the file is left untouched.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, data: &Data{}}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("management: read store %q: %w", path, err)
		}
		// New store: persist an initial empty document so the file exists
		// right away.
		if err := s.save(); err != nil {
			return nil, err
		}
		return s, nil
	}

	if len(raw) > 0 {
		if err := json.Unmarshal(raw, s.data); err != nil {
			return nil, fmt.Errorf("management: parse store %q: %w", path, err)
		}
	}
	return s, nil
}

// Path returns the file path backing the store.
func (s *Store) Path() string { return s.path }

// View invokes fn with a read lock held. fn must not retain the document
// beyond the call; use Snapshot for data that outlives the callback.
func (s *Store) View(fn func(d *Data)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.data)
}

// Snapshot returns a deep copy of the whole document. It is safe to retain.
func (s *Store) Snapshot() (*Data, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.clone()
}

// Update applies fn to a copy of the document under the write lock. If fn
// returns an error the update is rolled back (memory and disk untouched).
// On success the copy is persisted atomically and becomes the live document.
// If fn returns errNoop the callback made no net change, so the update is a
// no-op: nothing is persisted, UpdatedAt is not touched and the document is
// left as-is.
func (s *Store) Update(fn func(d *Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	copy, err := s.data.clone()
	if err != nil {
		return fmt.Errorf("management: clone store data: %w", err)
	}
	if err := fn(copy); err != nil {
		if errors.Is(err, errNoop) {
			return nil
		}
		return err
	}
	copy.UpdatedAt = time.Now()
	if err := writeFileAtomic(s.path, copy); err != nil {
		return err
	}
	s.data = copy
	s.dirty = false
	return nil
}

// Replace atomically swaps the whole document for d and persists it to disk
// in one step, under the write lock. It is meant for configuration rollback
// and snapshot restore: d is written exactly as given (its own UpdatedAt is
// preserved, unlike Update which stamps the current time) and becomes the
// live document, so the caller must not mutate d after the call. A nil
// document is rejected with ErrInvalid and leaves the store untouched.
func (s *Store) Replace(d *Data) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if d == nil {
		return ErrInvalid
	}
	if err := writeFileAtomic(s.path, d); err != nil {
		return err
	}
	s.data = d
	s.dirty = false
	return nil
}

// Save persists the current document to disk atomically. It is a no-op when
// the document is not dirty (nothing changed since the last write). It takes
// the write lock so concurrent Save calls are serialized and a redundant
// rewrite after a clean update is skipped.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	if err := writeFileAtomic(s.path, s.data); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// markDirty flags the document as out of sync with disk, so a subsequent Save
// will persist it. Currently no public mutation bypasses Update (which
// persists immediately), but this keeps Save correct for any future path that
// mutates the live document directly.
func (s *Store) markDirty() {
	s.dirty = true
}

// save persists the document without taking the lock; callers must hold it.
func (s *Store) save() error {
	return writeFileAtomic(s.path, s.data)
}

// writeFileAtomic writes data to path via a temp file in the same directory,
// fsync, then rename. A crash mid-write leaves either the old or the new
// file, never a truncated mix. On failure the temp file is removed.
func writeFileAtomic(path string, d *Data) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("management: mkdir %q: %w", dir, err)
	}

	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("management: marshal store: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("management: create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(raw); err != nil {
		return fmt.Errorf("management: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("management: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("management: close temp file: %w", err)
	}
	if err := renameFile(tmpName, path); err != nil {
		return fmt.Errorf("management: rename temp file onto %q: %w", path, err)
	}
	committed = true
	return nil
}

// renameFile atomically replaces path with tmp, briefly retrying transient
// failures. On Windows, renaming over a destination that was just replaced
// can intermittently fail with "Access is denied": the OS or an antivirus
// scanner holds the freshly-written target open for a moment. A short, bounded
// retry rides that out; a genuinely permanent error still surfaces. The
// operation is serialized by the Store write lock, so at most one rename for a
// given path is in flight at a time and the retries are never racing each other.
func renameFile(tmp, path string) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = os.Rename(tmp, path)
		if err == nil {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

// newID returns a random 32-character hex identifier.
func newID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is unrecoverable in practice; fall back to a
		// time-based id so the process can keep serving.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
