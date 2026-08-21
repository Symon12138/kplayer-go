package management

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// ConfigSnapshot is one point-in-time copy of the configuration document:
// the business collections as they were when the snapshot was created, the
// operator who captured it and a free-form description. DataHash is the
// sha256 of the canonical JSON of Data, so snapshot content can be verified
// or compared without diffing whole documents.
type ConfigSnapshot struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"createdAt"`
	Operator    string    `json:"operator,omitempty"`
	Description string    `json:"description,omitempty"`
	DataHash    string    `json:"dataHash"`
	Data        *Data     `json:"data"`
}

// defaultMaxSnapshots is the default cap on stored snapshots: creating a
// snapshot past the cap evicts the oldest ones (rolling history).
const defaultMaxSnapshots = 20

// ConfigSnapshotService provides configuration version management over a
// Store: create snapshots, list and fetch them, delete them, and restore
// the whole document from one.
type ConfigSnapshotService struct {
	store        *Store
	maxSnapshots int
}

// ConfigSnapshotServiceOption configures a ConfigSnapshotService.
type ConfigSnapshotServiceOption func(*ConfigSnapshotService)

// WithMaxSnapshots caps the number of snapshots kept in the store. When a
// Create pushes the collection past the cap, the oldest snapshots are
// dropped (rolling history, mirroring the webhook dispatcher's
// WithMaxHistory). Defaults to 20; a non-positive value keeps the history
// unbounded.
func WithMaxSnapshots(n int) ConfigSnapshotServiceOption {
	return func(cs *ConfigSnapshotService) { cs.maxSnapshots = n }
}

// NewConfigSnapshotService returns a ConfigSnapshotService backed by store.
func NewConfigSnapshotService(store *Store, opts ...ConfigSnapshotServiceOption) *ConfigSnapshotService {
	cs := &ConfigSnapshotService{store: store, maxSnapshots: defaultMaxSnapshots}
	for _, opt := range opts {
		opt(cs)
	}
	return cs
}

// snapshotPayload returns the document content a snapshot stores: a deep
// copy of d with the version-management collections removed and the
// volatile root UpdatedAt zeroed.
//
// The ConfigSnapshots collection must be excluded because the payload is
// serialized as the whole document: an embedded snapshot would contain a
// snapshot containing a snapshot, recursing without bound and ballooning
// the store file on every Create. ConfigTemplates are the
// version-management counterpart of snapshots and are excluded for the same
// reason, so a snapshot holds only the business collections and restoring
// it never erases snapshot history or templates. Root UpdatedAt is a write
// timestamp bumped on every store write, so it is zeroed to keep the
// DataHash stable for identical content.
func snapshotPayload(d *Data) (*Data, error) {
	payload, err := d.clone()
	if err != nil {
		return nil, err
	}
	payload.ConfigSnapshots = nil
	payload.ConfigTemplates = nil
	payload.UpdatedAt = time.Time{}
	return payload, nil
}

// Create captures the current document as a snapshot and stores it. The
// snapshot's Data is the payload of snapshotPayload and DataHash is the
// sha256 of its canonical JSON (struct fields marshal in declaration order
// and map keys are sorted, so the same content always hashes the same,
// across processes and store reopenings). Creating a snapshot past the
// configured cap evicts the oldest snapshots. It returns the stored
// snapshot with its generated ID and timestamp.
func (cs *ConfigSnapshotService) Create(operator, description string) (*ConfigSnapshot, error) {
	doc, err := cs.store.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("config snapshot: snapshot store: %w", err)
	}
	payload, err := snapshotPayload(doc)
	if err != nil {
		return nil, fmt.Errorf("config snapshot: build payload: %w", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("config snapshot: marshal payload: %w", err)
	}
	sum := sha256.Sum256(raw)
	snap := &ConfigSnapshot{
		ID:          newID(),
		CreatedAt:   time.Now(),
		Operator:    operator,
		Description: description,
		DataHash:    hex.EncodeToString(sum[:]),
		Data:        payload,
	}
	err = cs.store.Update(func(d *Data) error {
		d.ConfigSnapshots = append(d.ConfigSnapshots, snap)
		d.ConfigSnapshots = rollSnapshots(d.ConfigSnapshots, cs.maxSnapshots)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// List returns all snapshots, newest first: CreatedAt descending, with the
// ID breaking ties (ascending) for a deterministic order.
func (cs *ConfigSnapshotService) List() []*ConfigSnapshot {
	out := make([]*ConfigSnapshot, 0)
	cs.store.View(func(d *Data) {
		out = append(out, d.ConfigSnapshots...)
	})
	sort.Slice(out, func(i, j int) bool { return snapshotNewer(out[i], out[j]) })
	return out
}

// Get returns the snapshot with the given id.
func (cs *ConfigSnapshotService) Get(id string) (*ConfigSnapshot, error) {
	var found *ConfigSnapshot
	cs.store.View(func(d *Data) {
		for _, snap := range d.ConfigSnapshots {
			if snap.ID == id {
				found = snap
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("config snapshot %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Delete removes the snapshot with the given id.
func (cs *ConfigSnapshotService) Delete(id string) error {
	return cs.store.Update(func(d *Data) error {
		for i, snap := range d.ConfigSnapshots {
			if snap.ID == id {
				d.ConfigSnapshots = append(d.ConfigSnapshots[:i], d.ConfigSnapshots[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("config snapshot %q: %w", id, ErrNotFound)
	})
}

// Restore rolls the whole document back to the state captured in the
// snapshot with the given id: a copy of the snapshot's Data replaces the
// live document via Store.Replace. Because a snapshot never stores the
// version-management collections (see snapshotPayload), the current
// ConfigSnapshots and ConfigTemplates are carried over into the restored
// document: a rollback restores the business collections but never erases
// snapshot history or templates. The restored document is a fresh write,
// so root UpdatedAt is stamped with the current time. A missing snapshot
// returns ErrNotFound and leaves the document untouched.
func (cs *ConfigSnapshotService) Restore(id string) error {
	var snap *ConfigSnapshot
	cs.store.View(func(d *Data) {
		for _, s := range d.ConfigSnapshots {
			if s.ID == id {
				snap = s
				return
			}
		}
	})
	if snap == nil {
		return fmt.Errorf("config snapshot %q: %w", id, ErrNotFound)
	}
	// Clone so the carry-over below never mutates the stored payload: the
	// snapshot must keep its DataHash matching its Data.
	restored, err := snap.Data.clone()
	if err != nil {
		return fmt.Errorf("config snapshot: clone payload: %w", err)
	}
	restored.UpdatedAt = time.Now()
	cs.store.View(func(d *Data) {
		restored.ConfigSnapshots = d.ConfigSnapshots
		restored.ConfigTemplates = d.ConfigTemplates
	})
	return cs.store.Replace(restored)
}

// snapshotNewer reports whether a sorts before b in List order (newest
// first): CreatedAt descending, with the ID breaking ties (ascending) for a
// deterministic order.
func snapshotNewer(a, b *ConfigSnapshot) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID < b.ID
	}
	return a.CreatedAt.After(b.CreatedAt)
}

// rollSnapshots evicts the oldest snapshots so the collection holds at most
// max entries, preserving the order of the survivors. A max <= 0 keeps the
// history unbounded. It mirrors the webhook dispatcher's rolling history:
// only the newest max snapshots are kept.
func rollSnapshots(snaps []*ConfigSnapshot, max int) []*ConfigSnapshot {
	for max > 0 && len(snaps) > max {
		oldest := 0
		for i := 1; i < len(snaps); i++ {
			// snapshotNewer is a total order (IDs are unique), so the
			// entry no other entry is newer than is the oldest.
			if !snapshotNewer(snaps[i], snaps[oldest]) {
				oldest = i
			}
		}
		snaps = append(snaps[:oldest], snaps[oldest+1:]...)
	}
	return snaps
}
