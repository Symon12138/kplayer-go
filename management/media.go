package management

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Media is one entry of the media library: a video file discovered on disk
// (or registered manually) with its basic file metadata.
// MediaSortOrder names the playback order of a directory media entry.
type MediaSortOrder string

const (
	// SortByName plays a directory's files in file-name order (default).
	SortByName MediaSortOrder = "name"
	// SortByTime plays a directory's files in modification-time order.
	SortByTime MediaSortOrder = "time"
	// SortByRandom plays a directory's files in random order each run.
	SortByRandom MediaSortOrder = "random"
)

// Media is one entry of the media library: a video file discovered on disk
// (or registered manually) or a directory whose video files play as a
// continuous queue.
type Media struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Ext     string    `json:"ext"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`

	// IsDir marks a directory entry: playback expands to its video files in
	// SortBy order, playing one after another.
	IsDir  bool           `json:"isDir,omitempty"`
	SortBy MediaSortOrder `json:"sortBy,omitempty"`

	// AudioPath and SubtitlePath are optional auxiliary inputs merged into
	// the push: an external audio track replacing the source's audio and a
	// subtitle file burned into the picture (RTMP has no subtitle track).
	AudioPath    string `json:"audioPath,omitempty"`
	SubtitlePath string `json:"subtitlePath,omitempty"`

	// Optional metadata filled in by the ffprobe probe; Probed reports
	// whether the values come from a successful probe.
	Duration float64 `json:"duration,omitempty"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Bitrate  int64   `json:"bitrate,omitempty"`
	Probed   bool    `json:"probed,omitempty"`

	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// MediaService provides CRUD over the media collection of a Store plus
// directory scanning.
type MediaService struct {
	store *Store
}

// NewMediaService returns a MediaService backed by store.
func NewMediaService(store *Store) *MediaService {
	return &MediaService{store: store}
}

// List returns all media in insertion order.
func (ms *MediaService) List() []*Media {
	out := make([]*Media, 0)
	ms.store.View(func(d *Data) {
		out = append(out, d.Media...)
	})
	return out
}

// ListSorted returns all media sorted by name.
func (ms *MediaService) ListSorted() []*Media {
	out := ms.List()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the media with the given id.
func (ms *MediaService) Get(id string) (*Media, error) {
	var found *Media
	ms.store.View(func(d *Data) {
		for _, m := range d.Media {
			if m.ID == id {
				found = m
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("media %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Add registers a new media entry. If ID is empty one is generated; the path
// must be non-empty and unique. When the file exists on disk and Size/ModTime
// are zero, basic file metadata is derived from os.Stat.
func (ms *MediaService) Add(m *Media) error {
	return ms.addMany([]*Media{m})
}

// AddBatch registers several media entries in one atomic update.
func (ms *MediaService) AddBatch(ms_ []*Media) error {
	return ms.addMany(ms_)
}

func (ms *MediaService) addMany(items []*Media) error {
	now := time.Now()

	// pending pairs each prepared copy back to the caller's input object so
	// generated fields (id, timestamps, ext, name) can be surfaced to the
	// caller only when the whole update succeeds.
	type pending struct {
		in   *Media
		done *Media
	}
	var prepped []pending

	err := ms.store.Update(func(d *Data) error {
		for _, m := range items {
			if m == nil {
				return fmt.Errorf("media: %w: nil media", ErrInvalid)
			}
			// Prepare a copy instead of mutating the caller's object: if the
			// batch fails part-way the rolled-back store leaves the caller's
			// Media untouched (no assigned ids, no timestamps, no path
			// rewriting).
			mm := *m
			if err := validateMedia(&mm); err != nil {
				return err
			}
			mm.Path = filepath.Clean(mm.Path)
			for _, exist := range d.Media {
				if normPath(exist.Path) == normPath(mm.Path) {
					return fmt.Errorf("media path %q: %w", m.Path, ErrExists)
				}
			}
			if mm.ID == "" {
				mm.ID = newID()
			}
			if mm.Ext == "" {
				mm.Ext = strings.ToLower(filepath.Ext(mm.Path))
			}
			fillMediaStat(&mm, now)
			mm.CreatedAt = now
			mm.UpdatedAt = now
			d.Media = append(d.Media, &mm)
			prepped = append(prepped, pending{in: m, done: &mm})
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Success: surface the generated fields to the caller's inputs so they
	// can be referenced by id afterwards.
	for _, p := range prepped {
		*p.in = *p.done
	}
	return nil
}

// Update applies fn to the media with the given id under the store write
// lock; fn may mutate the media in place. Returning an error rolls back.
func (ms *MediaService) Update(id string, fn func(m *Media) error) error {
	return ms.store.Update(func(d *Data) error {
		for _, m := range d.Media {
			if m.ID != id {
				continue
			}
			if err := fn(m); err != nil {
				return err
			}
			m.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("media %q: %w", id, ErrNotFound)
	})
}

// Delete removes a media entry. Media still referenced by any playlist item
// cannot be deleted (ErrInUse).
func (ms *MediaService) Delete(id string) error {
	return ms.store.Update(func(d *Data) error {
		idx := -1
		for i, m := range d.Media {
			if m.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("media %q: %w", id, ErrNotFound)
		}
		for _, p := range d.Playlists {
			for _, item := range p.Items {
				if item.MediaID == id {
					return fmt.Errorf("media %q is referenced by playlist %q: %w", id, p.Name, ErrInUse)
				}
			}
		}
		for _, t := range d.Tasks {
			if t.MediaID == id {
				return fmt.Errorf("media %q is referenced by task %q: %w", id, t.Name, ErrInUse)
			}
		}
		d.Media = append(d.Media[:idx], d.Media[idx+1:]...)
		return nil
	})
}

// Scan walks root and merges every matching file into the library: existing
// entries (same absolute path) are updated in place keeping their id, new
// files are added. Files are matched against opts.Extensions (defaults to
// DefaultVideoExts) and, when opts.Probe is set, enriched through ffprobe
// when the binary is available.
func (ms *MediaService) Scan(ctx context.Context, root string, opts ScanOptions) (*ScanResult, error) {
	found, err := ScanDirectory(ctx, root, opts)
	if err != nil {
		return nil, err
	}

	result := &ScanResult{Added: []*Media{}, Updated: []*Media{}}
	err = ms.store.Update(func(d *Data) error {
		byPath := make(map[string]*Media, len(d.Media))
		for _, m := range d.Media {
			byPath[normPath(m.Path)] = m
		}

		for _, m := range found {
			path := normPath(m.Path)
			if exist, ok := byPath[path]; ok {
				exist.Name = m.Name
				exist.Ext = m.Ext
				exist.Size = m.Size
				exist.ModTime = m.ModTime
				if m.Probed {
					exist.Duration = m.Duration
					exist.Width = m.Width
					exist.Height = m.Height
					exist.Bitrate = m.Bitrate
					exist.Probed = true
				}
				exist.UpdatedAt = time.Now()
				result.Updated = append(result.Updated, exist)
				continue
			}

			m.ID = newID()
			m.CreatedAt = time.Now()
			m.UpdatedAt = time.Now()
			d.Media = append(d.Media, m)
			byPath[path] = m
			result.Added = append(result.Added, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result.Skipped = len(found) - len(result.Added) - len(result.Updated)
	return result, nil
}

// normPath returns a canonical form of a path used for deduplication. On
// Windows the filesystem is case-insensitive, so paths are compared
// case-insensitively (lower-cased); elsewhere the cleaned path is compared
// as-is.
func normPath(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

func validateMedia(m *Media) error {
	if m == nil {
		return fmt.Errorf("media: %w: nil media", ErrInvalid)
	}
	if strings.TrimSpace(m.Path) == "" {
		return fmt.Errorf("media: %w: empty path", ErrInvalid)
	}
	if m.Name == "" {
		m.Name = filepath.Base(m.Path)
	}
	// A path that exists as a directory becomes a directory entry; its
	// playback order defaults to file-name order.
	if st, err := os.Stat(m.Path); err == nil && st.IsDir() {
		m.IsDir = true
		if m.SortBy == "" {
			m.SortBy = SortByName
		}
	}
	switch m.SortBy {
	case "", SortByName, SortByTime, SortByRandom:
	default:
		return fmt.Errorf("media: %w: invalid sortBy %q", ErrInvalid, m.SortBy)
	}
	return nil
}

// Expand turns a media entry into the concrete playback queue: a single
// file yields itself; a directory yields its video files (filtered by
// DefaultVideoExts) ordered by the entry's SortBy (random order is
// reshuffled on every call). Files are returned sorted with the entry's
// auxiliary inputs attached.
func (ms *MediaService) Expand(m *Media) ([]*Media, error) {
	if m == nil {
		return nil, fmt.Errorf("media: %w: nil media", ErrInvalid)
	}
	if !m.IsDir {
		return []*Media{m}, nil
	}
	entries, err := os.ReadDir(m.Path)
	if err != nil {
		return nil, fmt.Errorf("media: read directory %q: %w", m.Path, err)
	}
	exts := make(map[string]bool, len(DefaultVideoExts))
	for _, e := range DefaultVideoExts {
		exts[strings.ToLower(e)] = true
	}
	out := make([]*Media, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if !exts[strings.ToLower(filepath.Ext(ent.Name()))] {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		item := *m
		item.ID = ""
		item.IsDir = false
		item.Name = ent.Name()
		item.Path = filepath.Join(m.Path, ent.Name())
		item.Ext = filepath.Ext(ent.Name())
		item.Size = info.Size()
		item.ModTime = info.ModTime()
		out = append(out, &item)
	}
	// 目录内的文件固定按文件名排序播放（运营约定）。
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// fillMediaStat derives Size/ModTime/Name/Ext from the file system when the
// entry does not carry them yet and the file exists.
func fillMediaStat(m *Media, now time.Time) {
	if m.ModTime.IsZero() || m.Size == 0 {
		if fi, err := os.Stat(m.Path); err == nil {
			m.Size = fi.Size()
			m.ModTime = fi.ModTime()
		}
	}
	if m.ModTime.IsZero() {
		m.ModTime = now
	}
	if m.Name == "" {
		m.Name = filepath.Base(m.Path)
	}
}

// ResolveMedia returns the media for id, or an error wrapping ErrNotFound.
func ResolveMedia(d *Data, id string) (*Media, error) {
	for _, m := range d.Media {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, fmt.Errorf("media %q: %w", id, ErrNotFound)
}