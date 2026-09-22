package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// taskDTO is the JSON shape for a task in list responses.
type taskDTO struct {
	ID           int64           `json:"id"`
	Name         string          `json:"name"`
	ConnectionID int64           `json:"connection_id"`
	StorageID    int64           `json:"storage_id"`
	Cron         string          `json:"cron"`
	DumperOpts   json.RawMessage `json:"dumper_opts,omitempty"`
	CodecChain   []string        `json:"codec_chain"`
	Retention    core.Retention  `json:"retention"`
	Notifiers    []string        `json:"notifiers"`
	Enabled      bool            `json:"enabled"`
	Retries      int             `json:"retries"`
	TimeoutSec   int64           `json:"timeout_sec"`
	// WatchdogSec is the configured staleness threshold: 0 = derived from cron,
	// -1 = off. The resolved value for a task on auto comes from GET /watchdog.
	WatchdogSec int64 `json:"watchdog_sec"`
	// NextRun is the next activation of Cron, in UTC. Derived on every read from
	// the same core.NextRun the dispatcher uses, so the UI never has to parse
	// cron (or guess the server's timezone). Omitted for a paused task and for a
	// cron the parser rejects.
	NextRun *time.Time `json:"next_run,omitempty"`
}

func toTaskDTO(t core.Task) taskDTO {
	return toTaskDTOAt(t, time.Now())
}

// toTaskDTOAt is toTaskDTO with an injectable clock, so next_run is testable.
func toTaskDTOAt(t core.Task, now time.Time) taskDTO {
	dto := taskDTO{
		ID: t.ID, Name: t.Name, ConnectionID: t.ConnectionID, StorageID: t.StorageID,
		Cron: t.Cron, DumperOpts: t.DumperOpts, CodecChain: t.CodecChain,
		Retention: t.Retention, Notifiers: t.Notifiers, Enabled: t.Enabled,
		Retries: t.Retries, TimeoutSec: int64(t.Timeout / time.Second),
		WatchdogSec: int64(t.Watchdog / time.Second),
	}
	if t.Enabled {
		if next, err := core.NextRun(t.Cron, now); err == nil {
			utc := next.UTC()
			dto.NextRun = &utc
		}
	}
	return dto
}

// runDTO is the JSON shape for a run.
type runDTO struct {
	ID         int64        `json:"id"`
	TaskID     int64        `json:"task_id"`
	Status     string       `json:"status"`
	Attempt    int          `json:"attempt"`
	Worker     string       `json:"worker,omitempty"`
	Error      string       `json:"error,omitempty"`
	Log        string       `json:"log,omitempty"`
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	CreatedAt  time.Time    `json:"created_at"`
	Artifact   *artifactDTO `json:"artifact,omitempty"`
}

type artifactDTO struct {
	ID        int64     `json:"id"`
	RunID     int64     `json:"run_id"`
	StorageID int64     `json:"storage_id"`
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	Checksum  string    `json:"checksum"`
	CreatedAt time.Time `json:"created_at"`
}

func toArtifactDTO(a core.Artifact) artifactDTO {
	return artifactDTO{
		ID: a.ID, RunID: a.RunID, StorageID: a.StorageID, Key: a.Key,
		Size: a.Size, Checksum: a.Checksum, CreatedAt: a.CreatedAt,
	}
}

func toRunDTO(r core.Run, artifact *core.Artifact) runDTO {
	var art *artifactDTO
	if artifact != nil {
		dto := toArtifactDTO(*artifact)
		art = &dto
	}
	return runDTO{
		ID: r.ID, TaskID: r.TaskID, Status: string(r.Status), Attempt: r.Attempt,
		Worker: r.Worker, Error: r.Error, Log: r.Log,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, CreatedAt: r.CreatedAt,
		Artifact: art,
	}
}

// connDTO is the safe JSON shape for a connection in list responses.
//
// connector_config is returned because the editing forms need it — host, port,
// ssh_user and the *_ref pointers are what the form is made of — but with
// inline credentials masked. The listing is readable by `viewer`, and the API
// accepts an inline private_key just as readily as a private_key_ref; without
// masking, "read-only" would include reading the SSH key to the database host.
type connDTO struct {
	ID              int64           `json:"id"`
	Name            string          `json:"name"`
	Engine          string          `json:"engine"`
	ConnectorType   string          `json:"connector_type"`
	ConnectorConfig json.RawMessage `json:"connector_config,omitempty"`
	Username        string          `json:"username"`
	SecretRef       string          `json:"secret_ref"`
}

func toConnDTO(c core.Connection) connDTO {
	return connDTO{
		ID: c.ID, Name: c.Name, Engine: c.Engine,
		ConnectorType: c.ConnectorType, ConnectorConfig: maskSecrets(c.ConnectorConfig),
		Username: c.Username, SecretRef: c.SecretRef,
	}
}

// storageDTO is the safe JSON shape for a storage in list responses. Config is
// returned for the same reason connDTO returns connector_config, and masked for
// the same reason: an inline access_key/secret_key must not be readable by a
// role whose permission is to see that a backup happened.
type storageDTO struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config,omitempty"`
	SecretRef string          `json:"secret_ref"`
}

func toStorageDTO(st core.Storage) storageDTO {
	return storageDTO{
		ID: st.ID, Name: st.Name, Type: st.Type,
		Config: maskSecrets(st.Config), SecretRef: st.SecretRef,
	}
}

// secretDTO is the safe JSON shape for a secret. It carries only metadata —
// never Ciphertext or the plaintext value, which stay server-side.
type secretDTO struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	KeyID      string     `json:"key_id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func toSecretDTO(sec core.Secret) secretDTO {
	return secretDTO{
		ID: sec.ID, Name: sec.Name, Type: sec.Type, KeyID: sec.KeyID,
		CreatedAt: sec.CreatedAt, LastUsedAt: sec.LastUsedAt,
	}
}

// registerReadRoutes wires the read endpoints.
func (s *server) registerReadRoutes(r chi.Router) {
	s.get(r, "/tasks", core.RoleViewer, s.listTasks)
	s.get(r, "/runs", core.RoleViewer, s.listRuns)
	s.get(r, "/runs/{id}", core.RoleViewer, s.getRun)
	s.get(r, "/connections", core.RoleViewer, s.listConnections)
	s.get(r, "/connections/{id}/databases", core.RoleOperator, s.listConnectionDatabases)
	s.get(r, "/storages", core.RoleViewer, s.listStorages)
	s.get(r, "/secrets", core.RoleViewer, s.listSecrets)
}

func (s *server) listConnectionDatabases(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	conn, err := s.d.Store.GetConnection(r.Context(), id)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "connection not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	databases, err := s.d.DatabaseList(ctx, *conn)
	if err != nil {
		if errors.Is(err, errDatabaseListUnsupported) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"databases": databases})
}

func (s *server) listSecrets(w http.ResponseWriter, r *http.Request) {
	secrets, err := s.d.Store.ListSecrets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]secretDTO, 0, len(secrets))
	for _, sec := range secrets {
		out = append(out, toSecretDTO(sec))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) listConnections(w http.ResponseWriter, r *http.Request) {
	conns, err := s.d.Store.ListConnections(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]connDTO, 0, len(conns))
	for _, c := range conns {
		out = append(out, toConnDTO(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) listStorages(w http.ResponseWriter, r *http.Request) {
	storages, err := s.d.Store.ListStorages(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]storageDTO, 0, len(storages))
	for _, st := range storages {
		out = append(out, toStorageDTO(st))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.d.Store.ListTasks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]taskDTO, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toTaskDTO(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var taskID int64
	if v := q.Get("task"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid task id")
			return
		}
		taskID = id
	}
	status := q.Get("status")

	var runs []core.Run
	var err error
	if q.Get("limit") != "" || q.Get("before") != "" || q.Get("after") != "" || q.Get("anchor") != "" {
		limit, ok := parseRunPageLimit(q.Get("limit"))
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		beforeID, ok := parseRunCursor(q.Get("before"))
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid before cursor")
			return
		}
		afterID, ok := parseRunCursor(q.Get("after"))
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid after cursor")
			return
		}
		anchorID, ok := parseRunCursor(q.Get("anchor"))
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid anchor cursor")
			return
		}
		if cursorCount(beforeID, afterID, anchorID) > 1 {
			writeError(w, http.StatusBadRequest, "use only one of before, after, anchor")
			return
		}
		runs, err = s.d.Store.ListRunsPage(r.Context(), taskID, status, limit, beforeID, afterID, anchorID)
	} else {
		runs, err = s.d.Store.ListRuns(r.Context(), taskID, status)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]runDTO, 0, len(runs))
	for _, run := range runs {
		out = append(out, s.artifactForRun(r.Context(), run))
	}
	writeJSON(w, http.StatusOK, out)
}

func parseRunPageLimit(v string) (int, bool) {
	if v == "" {
		return 50, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	if n > 100 {
		n = 100
	}
	return n, true
}

func parseRunCursor(v string) (int64, bool) {
	if v == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func cursorCount(ids ...int64) int {
	n := 0
	for _, id := range ids {
		if id != 0 {
			n++
		}
	}
	return n
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	run, err := s.d.Store.GetRun(r.Context(), id)
	if errors.Is(err, sqlite.ErrNotFound) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.artifactForRun(r.Context(), *run))
}

func (s *server) artifactForRun(ctx context.Context, run core.Run) runDTO {
	art, err := s.d.Store.GetArtifactByRun(ctx, run.ID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return toRunDTO(run, nil)
	}
	if err != nil {
		s.d.Log.Warn("api: load artifact for run failed", "run", run.ID, "err", err)
		return toRunDTO(run, nil)
	}
	return toRunDTO(run, art)
}
