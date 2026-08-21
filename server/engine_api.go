package server

import (
	"errors"
	"net/http"

	"github.com/bytelang/kplayer/engine"
)

// handleEngine serves the /engine REST endpoints: the ffmpeg engine
// configuration (GET/POST /engine/ffmpeg), applying it to a running stream
// (POST /engine/ffmpeg/apply) and its live status (GET /engine/status).
// Without an engine injected the routes report 404.
//
// POST /engine/ffmpeg saves the configuration through
// engine.Engine.UpdateConfig (400 on ErrInvalid) WITHOUT touching a running
// stream; POST /engine/ffmpeg/apply then makes it take effect through
// engine.Engine.Apply (a running stream is restarted with the new
// configuration). This two-step flow lets an operator fix a mistake before
// it disrupts an ongoing stream.
func (h *managementHandler) handleEngine(w http.ResponseWriter, r *http.Request) {
	if h.engine == nil {
		h.writeError(w, http.StatusNotFound, "ffmpeg engine not enabled")
		return
	}
	switch {
	case r.URL.Path == "/engine/ffmpeg" && r.Method == http.MethodGet:
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"config": h.engine.Config()})
	case r.URL.Path == "/engine/ffmpeg" && r.Method == http.MethodPost:
		var cfg engine.Config
		if !h.decode(w, r, &cfg) {
			return
		}
		if err := h.engine.UpdateConfig(cfg); err != nil {
			if errors.Is(err, engine.ErrInvalid) {
				h.writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			h.writeManagementError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "config": h.engine.Config()})
	case r.URL.Path == "/engine/ffmpeg/apply" && r.Method == http.MethodPost:
		if err := h.engine.Apply(r.Context()); err != nil {
			h.writeManagementError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "status": h.engine.Status()})
	case r.URL.Path == "/engine/status" && r.Method == http.MethodGet:
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"status": h.engine.Status()})
	default:
		http.NotFound(w, r)
	}
}
