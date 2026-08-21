package management

import (
	"fmt"
	"regexp"
)

// DeployResult carries the entities created by a Deploy call: the playlist,
// one scene template per SceneKinds entry and, when the template carries a
// task, the scheduled task bound to the new playlist. TaskID and Task are
// empty/nil when the template has no task. On failure Deploy returns the
// partially populated result alongside the error, so callers can see which
// resources were already created.
type DeployResult struct {
	PlaylistID       string           `json:"playlistId"`
	SceneTemplateIDs []string         `json:"sceneTemplateIds,omitempty"`
	TaskID           string           `json:"taskId,omitempty"`
	Playlist         *Playlist        `json:"playlist,omitempty"`
	Scenes           []*SceneTemplate `json:"scenes,omitempty"`
	Task             *ScheduleTask    `json:"task,omitempty"`
}

// Deploy performs a one-click deployment of an industry template onto the
// store. It runs the following steps in order:
//
//  1. Media placeholders are expanded: every MediaPlaceholders entry
//     containing a "${key}" placeholder is substituted with the matching
//     parameter (a missing parameter fails with ErrInvalid), while entries
//     without placeholders are used as literal media ids.
//  2. Every expanded id must reference an existing media (ErrNotFound). The
//     check runs before the playlist is created, so a missing media fails
//     without creating anything.
//  3. The playlist is created via PlaylistService.Create with the template's
//     playlist name, an empty description, the expanded media ids and
//     loop=false.
//  4. One scene template per SceneKinds entry is created via
//     SceneTemplateService.Create with the name "<template.Name> - <kind>",
//     no params and Enabled=true.
//  5. When template.Task is set, the task is created via TaskService.Create
//     from the template's task spec, targeting the playlist created in
//     step 3.
//
// Deployment is deliberately not transactional: the steps run through the
// regular services, each of which is validated and persisted independently,
// and a failing step does not roll back the resources already created
// ("partial deployment" semantics). The error is returned together with the
// DeployResult holding those resources, so a caller that needs cleanup can
// delete them explicitly. This keeps Deploy simple and mirrors the other
// services, whose Create methods are themselves all-or-nothing. Note that
// scene template names must be unique: deploying a template whose scenes
// already exist fails at the scene step with ErrExists, leaving the newly
// created playlist in place. A nil template is rejected with ErrInvalid.
func Deploy(template *IndustryTemplate, params map[string]string, store *Store) (*DeployResult, error) {
	if template == nil {
		return nil, fmt.Errorf("deploy: %w: nil template", ErrInvalid)
	}
	result := &DeployResult{}

	// Step 1: expand the media placeholders.
	expanded := make([]string, 0, len(template.MediaPlaceholders))
	for _, entry := range template.MediaPlaceholders {
		id, err := expandPlaceholders(entry, params)
		if err != nil {
			return result, fmt.Errorf("deploy %q: %w", template.Name, err)
		}
		expanded = append(expanded, id)
	}

	// Step 2: every expanded id must reference an existing media. The check
	// runs up front, so a missing media fails before anything is created.
	var mediaErr error
	store.View(func(d *Data) {
		for _, id := range expanded {
			if _, err := ResolveMedia(d, id); err != nil {
				mediaErr = fmt.Errorf("deploy %q: %w", template.Name, err)
				return
			}
		}
	})
	if mediaErr != nil {
		return result, mediaErr
	}

	// Step 3: create the playlist.
	playlist, err := NewPlaylistService(store).Create(template.PlaylistName, "", expanded, false)
	if err != nil {
		return result, fmt.Errorf("deploy %q: create playlist: %w", template.Name, err)
	}
	result.PlaylistID = playlist.ID
	result.Playlist = playlist

	// Step 4: one scene template per kind, enabled, without params.
	scenes := NewSceneTemplateService(store)
	for _, kind := range template.SceneKinds {
		scene, err := scenes.Create(SceneTemplateSpec{
			Name:    template.Name + " - " + string(kind),
			Kind:    kind,
			Params:  nil,
			Enabled: true,
		})
		if err != nil {
			return result, fmt.Errorf("deploy %q: create scene template: %w", template.Name, err)
		}
		result.SceneTemplateIDs = append(result.SceneTemplateIDs, scene.ID)
		result.Scenes = append(result.Scenes, scene)
	}

	// Step 5: optionally create the scheduled task bound to the new
	// playlist.
	if template.Task != nil {
		spec := TaskSpec{
			Name:       template.Task.Name,
			Type:       template.Task.Type,
			Interval:   template.Task.Interval,
			Cron:       template.Task.Cron,
			PlaylistID: playlist.ID,
			Enabled:    template.Task.Enabled,
		}
		task, err := NewTaskService(store).Create(spec)
		if err != nil {
			return result, fmt.Errorf("deploy %q: create task: %w", template.Name, err)
		}
		result.TaskID = task.ID
		result.Task = task
	}

	return result, nil
}

// deployPlaceholderRe matches a "${key}" placeholder whose key consists of
// letters, digits and underscores, mirroring the placeholder syntax of the
// config template expansion.
var deployPlaceholderRe = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// expandPlaceholders substitutes every "${key}" placeholder of s with the
// corresponding value from params and returns the expanded string. It is a
// lightweight, media-id-specific variant of the config template expansion,
// implemented independently: keys consist of letters, digits and underscores,
// values are injected literally in a single pass with no recursive expansion,
// and a placeholder without a matching parameter returns an error wrapping
// ErrInvalid naming the key. An entry without a matching placeholder
// (including text that merely looks like one, such as "${a-b}", whose key
// does not fit the syntax) is returned unchanged.
func expandPlaceholders(s string, params map[string]string) (string, error) {
	if !deployPlaceholderRe.MatchString(s) {
		return s, nil
	}
	var missing string
	out := deployPlaceholderRe.ReplaceAllStringFunc(s, func(m string) string {
		key := m[2 : len(m)-1]
		v, ok := params[key]
		if !ok {
			if missing == "" {
				missing = key
			}
			return m
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("placeholder %q: missing parameter: %w", missing, ErrInvalid)
	}
	return out, nil
}
