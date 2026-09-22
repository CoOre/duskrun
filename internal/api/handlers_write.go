package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/plugin"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// registerWriteRoutes wires the create endpoints.
func (s *server) registerWriteRoutes(r chi.Router) {
	s.post(r, "/connections/test", core.RoleOperator, s.testConnection)
	s.post(r, "/connections", core.RoleOperator, s.createConnection)
	s.patch(r, "/connections/{id}", core.RoleOperator, s.updateConnection)
	s.post(r, "/storages", core.RoleOperator, s.createStorage)
	s.patch(r, "/storages/{id}", core.RoleOperator, s.updateStorage)
	s.post(r, "/tasks", core.RoleOperator, s.createTask)
	s.patch(r, "/tasks/{id}", core.RoleOperator, s.updateTask)
	s.post(r, "/secrets", core.RoleAdmin, s.createSecret)
}

type createSecretReq struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// createSecret seals a plaintext value under the master key and upserts it by
// name. The value is write-only: it is never returned by any endpoint.
func (s *server) createSecret(w http.ResponseWriter, r *http.Request) {
	if s.d.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "secret store unavailable (no master key configured)")
		return
	}
	var req createSecretReq
	if !decode(w, r, &req) {
		return
	}
	if req.Name == "" || req.Type == "" || req.Value == "" {
		writeError(w, http.StatusBadRequest, "name, type and value are required")
		return
	}
	sealed, err := s.d.Secrets.Seal(req.Name, req.Type, []byte(req.Value))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := s.d.Store.PutSecret(r.Context(), sealed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

type createConnectionReq struct {
	Name            string          `json:"name"`
	Engine          string          `json:"engine"`
	ConnectorType   string          `json:"connector_type"`
	ConnectorConfig json.RawMessage `json:"connector_config"`
	Username        string          `json:"username"`
	SecretRef       string          `json:"secret_ref"`
}

func (s *server) createConnection(w http.ResponseWriter, r *http.Request) {
	var req createConnectionReq
	if !decode(w, r, &req) {
		return
	}
	c, err := buildConnection(req, 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Only new connections pick up the configured default; an existing one
	// keeps whatever mode it was created with, so tightening or loosening the
	// default never silently changes a tunnel that already verifies a host.
	if c.ConnectorConfig, err = applyHostKeyDefault(c.ConnectorType, c.ConnectorConfig, s.hostKeyModeDefault(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.d.Store.CreateConnection(r.Context(), c)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *server) updateConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req createConnectionReq
	if !decode(w, r, &req) {
		return
	}
	// The listing masks inline credentials, so a client doing read-modify-write
	// sends "••••" back for each one. Restore them from the stored row before
	// building, or editing the port would overwrite the private key with four
	// bullets — a connection that saves cleanly and fails at the next backup.
	if current, err := s.d.Store.GetConnection(r.Context(), id); err == nil {
		req.ConnectorConfig = unmaskSecrets(req.ConnectorConfig, current.ConnectorConfig)
	}
	c, err := buildConnection(req, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.d.Store.UpdateConnection(r.Context(), c); err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "connection not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func buildConnection(req createConnectionReq, id int64) (core.Connection, error) {
	if req.Name == "" || req.Engine == "" || req.ConnectorType == "" {
		return core.Connection{}, errors.New("name, engine and connector_type are required")
	}
	if !plugin.Connectors.Has(req.ConnectorType) {
		return core.Connection{}, errors.New("unknown connector type: " + req.ConnectorType)
	}
	if !plugin.Dumpers.Has(req.Engine) {
		return core.Connection{}, errors.New("unknown engine (no dumper): " + req.Engine)
	}
	return core.Connection{
		ID: id, Name: req.Name, Engine: req.Engine, ConnectorType: req.ConnectorType,
		ConnectorConfig: req.ConnectorConfig, Username: req.Username, SecretRef: req.SecretRef,
	}, nil
}

type createStorageReq struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config"`
	SecretRef string          `json:"secret_ref"`
}

func (s *server) createStorage(w http.ResponseWriter, r *http.Request) {
	var req createStorageReq
	if !decode(w, r, &req) {
		return
	}
	storage, ok := s.storageFromReq(w, req, 0)
	if !ok {
		return
	}
	id, err := s.d.Store.CreateStorage(r.Context(), storage)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *server) updateStorage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req createStorageReq
	if !decode(w, r, &req) {
		return
	}
	// Same round trip as updateConnection: the listing masked whatever inline
	// access_key/secret_key the row carries, and a client echoing that back
	// must not persist the mask over them.
	if current, err := s.d.Store.GetStorage(r.Context(), id); err == nil {
		req.Config = unmaskSecrets(req.Config, current.Config)
	}
	storage, ok := s.storageFromReq(w, req, id)
	if !ok {
		return
	}
	if err := s.d.Store.UpdateStorage(r.Context(), storage); err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "storage not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func (s *server) storageFromReq(w http.ResponseWriter, req createStorageReq, id int64) (core.Storage, bool) {
	if req.Name == "" || req.Type == "" {
		writeError(w, http.StatusBadRequest, "name and type are required")
		return core.Storage{}, false
	}
	if !plugin.Storages.Has(req.Type) {
		writeError(w, http.StatusBadRequest, "unknown storage type: "+req.Type)
		return core.Storage{}, false
	}
	return core.Storage{ID: id, Name: req.Name, Type: req.Type, Config: req.Config, SecretRef: req.SecretRef}, true
}

type createTaskReq struct {
	Name         string          `json:"name"`
	ConnectionID int64           `json:"connection_id"`
	StorageID    int64           `json:"storage_id"`
	DumperOpts   json.RawMessage `json:"dumper_opts"`
	CodecChain   []string        `json:"codec_chain"`
	Cron         string          `json:"cron"`
	Retention    core.Retention  `json:"retention"`
	Notifiers    []string        `json:"notifiers"`
	Enabled      *bool           `json:"enabled"`
	Retries      int             `json:"retries"`
	TimeoutSec   int             `json:"timeout_sec"`
	// WatchdogSec: 0 derives the threshold from cron, -1 disables it. A POINTER
	// so an omitted field in a PATCH keeps the task's current threshold instead
	// of silently resetting an explicit one back to auto.
	WatchdogSec *int `json:"watchdog_sec"`
}

func (s *server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if !decode(w, r, &req) {
		return
	}
	task, ok := s.taskFromReq(w, r, req, 0, true, core.WatchdogAuto)
	if !ok {
		return
	}
	id, err := s.d.Store.CreateTask(r.Context(), task)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *server) updateTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.d.Store.GetTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req createTaskReq
	if !decode(w, r, &req) {
		return
	}
	task, ok := s.taskFromReq(w, r, req, id, current.Enabled, current.Watchdog)
	if !ok {
		return
	}
	if err := s.d.Store.UpdateTask(r.Context(), task); err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func (s *server) taskFromReq(w http.ResponseWriter, r *http.Request, req createTaskReq, id int64, defaultEnabled bool, defaultWatchdog time.Duration) (core.Task, bool) {
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return core.Task{}, false
	}
	if _, err := core.ParseCron(req.Cron); err != nil {
		writeError(w, http.StatusBadRequest, "invalid cron: "+err.Error())
		return core.Task{}, false
	}
	// Referenced connection/storage must exist.
	conn, err := s.d.Store.GetConnection(r.Context(), req.ConnectionID)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "connection does not exist")
			return core.Task{}, false
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return core.Task{}, false
	}
	// The external dump tool for the connection's engine must be installed.
	if err := s.d.ToolCheck(conn.Engine); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return core.Task{}, false
	}
	if _, err := s.d.Store.GetStorage(r.Context(), req.StorageID); err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "storage does not exist")
			return core.Task{}, false
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return core.Task{}, false
	}
	// Referenced plugins must be registered.
	for _, c := range req.CodecChain {
		if !plugin.Codecs.Has(c) {
			writeError(w, http.StatusBadRequest, "unknown codec: "+c)
			return core.Task{}, false
		}
	}
	// Notifiers now name CONFIGURED channels, not plugin types: a task that
	// references a channel nobody created would deliver nothing.
	channels, err := s.d.Store.ListNotifiers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return core.Task{}, false
	}
	known := make(map[string]bool, len(channels))
	for _, c := range channels {
		known[c.Name] = true
	}
	for _, n := range req.Notifiers {
		if !known[n] {
			writeError(w, http.StatusBadRequest, "unknown notifier channel: "+n)
			return core.Task{}, false
		}
	}

	if err := req.Retention.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid retention: "+err.Error())
		return core.Task{}, false
	}

	enabled := defaultEnabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	watchdog := defaultWatchdog
	if req.WatchdogSec != nil {
		// Only 0 (auto) and -1 (off) are meaningful sentinels; any other negative
		// would silently read as "off" and hide a typo.
		if *req.WatchdogSec < -1 {
			writeError(w, http.StatusBadRequest, "watchdog_sec must be >= 0 (0 = derive from cron) or -1 (off)")
			return core.Task{}, false
		}
		watchdog = time.Duration(*req.WatchdogSec) * time.Second
	}
	return core.Task{
		ID: id, Name: req.Name, ConnectionID: req.ConnectionID, StorageID: req.StorageID,
		DumperOpts: req.DumperOpts, CodecChain: req.CodecChain, Cron: req.Cron,
		Retention: req.Retention.Normalized(), Notifiers: req.Notifiers, Enabled: enabled,
		Retries: req.Retries, Timeout: timeoutFromSec(req.TimeoutSec),
		Watchdog: watchdog,
	}, true
}

// timeoutFromSec converts a seconds value to a Duration (0 → store default).
func timeoutFromSec(sec int) time.Duration {
	return time.Duration(sec) * time.Second
}

// maxBodyBytes bounds a JSON request body.
//
// Every body this API accepts is a small object: a task, a connection, a plugin
// config. 1 MiB is far above the largest of them (an inline PEM private key is
// a few kilobytes) and far below what it costs to care about. Without a bound,
// json.Decoder reads until the client stops sending, so one request can grow
// the process by as much memory as the sender is willing to spend.
const maxBodyBytes = 1 << 20

// decode reads a JSON request body into v, writing a 400 on failure. Returns
// false when the caller should stop.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		// A body over the limit is not a malformed one, and 413 tells a client
		// that trimming its request is the fix — 400 would send it hunting for
		// a syntax error that is not there.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds "+strconv.Itoa(maxBodyBytes)+" bytes")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
