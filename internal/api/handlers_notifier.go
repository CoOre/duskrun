package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/plugin"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// eventKinds is the closed set a channel may subscribe to.
var eventKinds = []string{
	string(plugin.EventSuccess),
	string(plugin.EventFailure),
	string(plugin.EventRetentionError),
	string(plugin.EventWatchdog),
}

// maxNotificationLimit bounds the delivery-log listing.
const maxNotificationLimit = 500

// notifierDTO is a configured channel. Config is returned with credential
// values masked; secret REFERENCES stay visible, which is the point of them.
type notifierDTO struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config,omitempty"`
	Events    []string        `json:"events"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
	// UsedBy names the tasks referencing this channel, so the UI can explain a
	// refused delete before the operator attempts it.
	UsedBy []string `json:"used_by"`
}

type notificationDTO struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Task      string    `json:"task,omitempty"`
	RunID     *int64    `json:"run_id,omitempty"`
	Channel   string    `json:"channel"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type createNotifierReq struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Config  json.RawMessage `json:"config"`
	Events  []string        `json:"events"`
	Enabled *bool           `json:"enabled"`
}

// registerNotifierRoutes wires the channel and delivery-log endpoints.
//
// PATCH is operator because subscribing a channel to events is the operator's
// job (the events × channels matrix), but the route's role is only half the
// check: which fields an operator may touch is enforced in updateNotifier. See
// notifierPatchAllowed for why.
func (s *server) registerNotifierRoutes(r chi.Router) {
	s.get(r, "/notifiers", core.RoleViewer, s.listNotifiers)
	s.post(r, "/notifiers", core.RoleAdmin, s.createNotifier)
	s.patch(r, "/notifiers/{id}", core.RoleOperator, s.updateNotifier)
	s.del(r, "/notifiers/{id}", core.RoleAdmin, s.deleteNotifier)
	s.post(r, "/notifiers/{id}/test", core.RoleOperator, s.testNotifier)
	s.get(r, "/notifications", core.RoleViewer, s.listNotifications)
}

func (s *server) listNotifiers(w http.ResponseWriter, r *http.Request) {
	channels, err := s.d.Store.ListNotifiers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]notifierDTO, 0, len(channels))
	for _, c := range channels {
		users, err := s.d.Store.NotifierUsers(r.Context(), c.Name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if users == nil {
			users = []string{}
		}
		out = append(out, notifierDTO{
			ID: c.ID, Name: c.Name, Type: c.Type, Config: maskSecrets(c.Config),
			Events: c.Events, Enabled: c.Enabled, CreatedAt: c.CreatedAt, UsedBy: users,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) createNotifier(w http.ResponseWriter, r *http.Request) {
	var req createNotifierReq
	if !decode(w, r, &req) {
		return
	}
	ch, ok := s.notifierFromReq(w, req, 0, nil)
	if !ok {
		return
	}
	if !s.notifierBuilds(w, r, ch) {
		return
	}
	id, err := s.d.Store.CreateNotifier(r.Context(), ch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *server) updateNotifier(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.d.Store.GetNotifier(r.Context(), id)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "notifier not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req createNotifierReq
	if !decode(w, r, &req) {
		return
	}
	// A PATCH that omits events, enabled or config must not silently re-subscribe
	// a deliberately narrowed channel, switch a muted one back on, or blank its
	// configuration. Config matters doubly: the listing masks credentials, so a
	// client echoing back what it read would otherwise persist "••••" over a
	// real token.
	if !notifierPatchAllowed(w, r, req, current) {
		return
	}
	ch, ok := s.notifierFromReq(w, req, id, current)
	if !ok {
		return
	}
	if !s.notifierBuilds(w, r, ch) {
		return
	}
	if err := s.d.Store.UpdateNotifier(r.Context(), ch); err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "notifier not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func (s *server) deleteNotifier(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.d.Store.DeleteNotifier(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, sqlite.ErrNotFound):
			writeError(w, http.StatusNotFound, "notifier not found")
		case errors.Is(err, sqlite.ErrInUse):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testNotifier delivers a synthetic event through the channel exactly as a real
// event would travel — same construction, same secret resolution — so a green
// result means the channel actually works, not that its config parses.
func (s *server) testNotifier(w http.ResponseWriter, r *http.Request) {
	if s.d.Notify == nil {
		writeError(w, http.StatusServiceUnavailable, "notifications not configured")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ch, err := s.d.Store.GetNotifier(r.Context(), id)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "notifier not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.d.Notify.Test(r.Context(), *ch); err != nil {
		// The request itself succeeded; the delivery is what failed, and the
		// caller wants to read the reason rather than handle an HTTP error.
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *server) listNotifications(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > maxNotificationLimit {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be at most %d", maxNotificationLimit))
			return
		}
		limit = n
	}
	entries, err := s.d.Store.ListNotifications(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]notificationDTO, 0, len(entries))
	for _, n := range entries {
		out = append(out, notificationDTO{
			ID: n.ID, Kind: n.Kind, Task: n.Task, RunID: n.RunID, Channel: n.Channel,
			Status: string(n.Status), Error: n.Error, CreatedAt: n.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// notifierFromReq builds a channel from the request. current is the stored row
// on update (nil on create) and supplies the defaults for omitted fields.
func (s *server) notifierFromReq(w http.ResponseWriter, req createNotifierReq, id int64, current *core.NotifierChannel) (core.NotifierChannel, bool) {
	if req.Name == "" || req.Type == "" {
		writeError(w, http.StatusBadRequest, "name and type are required")
		return core.NotifierChannel{}, false
	}
	if !plugin.Notifiers.Has(req.Type) {
		writeError(w, http.StatusBadRequest, "unknown notifier type: "+req.Type)
		return core.NotifierChannel{}, false
	}
	for _, e := range req.Events {
		if !validEventKind(e) {
			writeError(w, http.StatusBadRequest, "unknown event kind: "+e)
			return core.NotifierChannel{}, false
		}
	}
	events := req.Events
	if events == nil {
		if current != nil {
			events = current.Events // keep the existing subscription
		} else {
			// A channel created without an explicit subscription would be
			// configured but deliver nothing, so on create "omitted" means all.
			events = append([]string(nil), eventKinds...)
		}
	}
	enabled := true
	if current != nil {
		enabled = current.Enabled
	}
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	config := req.Config
	if current != nil {
		if len(config) == 0 {
			config = current.Config
		} else {
			config = unmaskSecrets(config, current.Config)
		}
	}
	return core.NotifierChannel{
		ID: id, Name: req.Name, Type: req.Type, Config: config,
		Events: events, Enabled: enabled,
	}, true
}

// notifierBuilds runs the plugin's own constructor over the channel's resolved
// config. The type check above only says the plugin exists; without this a
// channel missing a required field (smtp without recipients) is stored looking
// healthy and fails for the first time in the delivery log — after a backup has
// already failed with nobody notified.
func (s *server) notifierBuilds(w http.ResponseWriter, r *http.Request, ch core.NotifierChannel) bool {
	if s.d.Notify == nil {
		return true
	}
	if err := s.d.Notify.Validate(r.Context(), ch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// notifierPatchAllowed enforces the field-level half of the channel permission
// model: an operator may change what a channel is *subscribed to*, only an
// admin may change what a channel *is*.
//
// The split is a privilege boundary, not tidiness. A channel config names both
// the secret it uses (`password_ref`, `token_ref`) and the host that secret is
// presented to, and core.SecretResolver hands the plugin the decrypted value
// without asking who last edited the row. An operator free to rewrite type and
// config could repoint a channel at an SMTP server it controls, aim
// `password_ref` at any secret in the store — including ones it may never read
// through /secrets — and recover the plaintext from its own AUTH PLAIN. The
// only trace left behind is a successful line in the delivery log. Creating and
// deleting channels are already admin for this reason; editing was the way
// around it.
//
// events and enabled carry no such power: they decide when an
// already-configured channel fires, which is exactly the operator's job.
func notifierPatchAllowed(w http.ResponseWriter, r *http.Request, req createNotifierReq, current *core.NotifierChannel) bool {
	if principalFrom(r.Context()).IsAdmin() {
		return true
	}
	deny := func(field string) bool {
		writeError(w, http.StatusForbidden, "forbidden: changing "+field+" requires role admin")
		return false
	}
	if req.Name != current.Name {
		return deny("name")
	}
	if req.Type != current.Type {
		return deny("type")
	}
	// An omitted config keeps the stored one, so there is nothing to compare.
	// A present one is compared *after* unmasking: a client that read the
	// listing echoes back "••••" for every credential, and that round trip is
	// not an edit.
	if len(req.Config) > 0 && !sameJSON(unmaskSecrets(req.Config, current.Config), current.Config) {
		return deny("config")
	}
	return true
}

// sameJSON reports whether two configs carry the same value. It compares parsed
// documents rather than bytes so that re-serialisation — different key order,
// different spacing — does not read as an edit. Absent, null and empty-object
// configs are all "no configuration" and compare equal; unparseable input is
// never equal, which denies rather than admits.
func sameJSON(a, b json.RawMessage) bool {
	norm := func(raw json.RawMessage) (any, bool) {
		if len(raw) == 0 {
			return nil, true
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, false
		}
		if m, isMap := v.(map[string]any); isMap && len(m) == 0 {
			return nil, true
		}
		return v, true
	}
	av, aok := norm(a)
	bv, bok := norm(b)
	return aok && bok && reflect.DeepEqual(av, bv)
}

func validEventKind(kind string) bool {
	for _, k := range eventKinds {
		if k == kind {
			return true
		}
	}
	return false
}
