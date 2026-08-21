// Package management implements the phase-1 backend for the standalone web
// console of KPlayer: a single-machine control plane with local JSON
// persistence.
//
// Scope
//
//   - Store: a thread-safe, single-document JSON store persisted with atomic
//     file writes (temp file + rename), so a crash never leaves a truncated
//     data file behind.
//   - Media library: media model, CRUD service and a directory scanner that
//     collects name/path/extension/size/modification-time from the file
//     system. Optional metadata enrichment via ffprobe is attempted only when
//     the binary is available; it is never a hard dependency.
//   - Playlist (program schedule): ordered list of media references with full
//     CRUD and item manipulation.
//   - Alarm: basic alarm model and service (active/resolved lifecycle,
//     deduplication of identical active alarms, pruning of resolved ones).
//   - Schedule: scheduled-task model (interval or 5-field cron) persisted by a
//     task service, plus a stoppable Scheduler runtime. The scheduler triggers
//     playback exclusively through the abstract Player interface and never
//     imports core or provider packages, so it can be driven by any playback
//     backend (including a future console adapter for libkplayer).
//
// Design rules
//
//   - Standard library only; no third-party imports (Go 1.17 compatible).
//   - All services are safe for concurrent use; mutations go through
//     Store.Update, which serializes writers with copy-on-write semantics and
//     persists atomically.
//   - No dependency on cgo / libkplayer / core; package compiles with pure Go.
package management
