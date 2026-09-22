package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/plugin"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// registerActionRoutes wires run-now, artifact download, and restore-hint.
func (s *server) registerActionRoutes(r chi.Router) {
	s.post(r, "/tasks/{id}/run", core.RoleOperator, s.runNow)
	s.get(r, "/tasks/{id}/restore-hint", core.RoleViewer, s.restoreHint)
	s.get(r, "/runs/{id}/events", core.RoleViewer, s.streamRunEvents)
	s.get(r, "/runs/{id}/artifact/download", core.RoleOperator, s.downloadRunArtifact)
	s.get(r, "/artifacts/{id}/download", core.RoleOperator, s.downloadArtifact)
}

// runNow enqueues an immediate run for a task (202 + run id). A task that already
// has an active run yields 409.
func (s *server) runNow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.d.Store.GetTask(r.Context(), id); err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	runID, err := s.d.Store.Enqueue(r.Context(), id)
	if err != nil {
		if errors.Is(err, sqlite.ErrActiveRun) {
			writeError(w, http.StatusConflict, "task already has an active run")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int64{"run_id": runID})
}

// restoreHint returns the human restore command for a task's dumper.
func (s *server) restoreHint(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	task, err := s.d.Store.GetTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	conn, err := s.d.Store.GetConnection(r.Context(), task.ConnectionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Deliberately unresolved: the hint is text shown in the UI, and building it
	// from a config with decrypted secrets in it would be one careless
	// fmt.Sprintf away from printing a private key on the page.
	dumper, err := plugin.Dumpers.Create(conn.Engine, task.DumperOpts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var opts plugin.DumpOptions
	if len(task.DumperOpts) > 0 {
		_ = json.Unmarshal(task.DumperOpts, &opts)
	}
	writeJSON(w, http.StatusOK, map[string]string{"restore_hint": dumper.RestoreHint(opts)})
}

// downloadArtifact streams an artifact's bytes from its storage.
func (s *server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	art, err := s.d.Store.GetArtifact(r.Context(), id)
	if err != nil {
		s.downloadArtifactObject(w, r, art, err)
		return
	}
	s.downloadArtifactObject(w, r, art, nil)
}

func (s *server) downloadRunArtifact(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathID(w, r)
	if !ok {
		return
	}
	art, err := s.d.Store.GetArtifactByRun(r.Context(), runID)
	if err != nil {
		s.downloadArtifactObject(w, r, art, err)
		return
	}
	s.downloadArtifactObject(w, r, art, nil)
}

func (s *server) downloadArtifactObject(w http.ResponseWriter, r *http.Request, art *core.Artifact, err error) {
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "artifact not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stor, err := s.d.Store.GetStorage(r.Context(), art.StorageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Same resolution the executor does when it writes the object: a storage
	// keeping its credentials in the secret store must be readable here too, or
	// the artifact would be downloadable only from destinations with inline keys.
	config, err := s.res.Resolve(r.Context(), stor.Config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	storage, err := plugin.Storages.Create(stor.Type, config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Deferred before the read: the object's reader is closed first (LIFO), then
	// the session behind it. Skipping this would leave one ssh connection per
	// download for the life of the daemon.
	defer core.CloseStorage(storage)
	rc, err := storage.Read(r.Context(), art.Key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(art.Key)+`"`)
	if art.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(art.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// pathID parses the {id} URL param, writing a 400 on failure.
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}
