package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// registerDeleteRoutes wires the DELETE endpoints for the four managed entities.
// Deletes are guarded: a connection/storage referenced by a task, a task with an
// active run, or a secret still referenced anywhere returns 409 Conflict with a
// message naming what blocks the removal, rather than orphaning dependents.
func (s *server) registerDeleteRoutes(r chi.Router) {
	s.del(r, "/connections/{id}", core.RoleOperator, s.deleteConnection)
	s.del(r, "/storages/{id}", core.RoleOperator, s.deleteStorage)
	s.del(r, "/tasks/{id}", core.RoleOperator, s.deleteTask)
	s.del(r, "/secrets/{id}", core.RoleAdmin, s.deleteSecret)
}

func (s *server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.d.Store.DeleteConnection(r.Context(), id)
	s.writeDeleteResult(w, err, "connection", "соединение используется в задаче — сначала удалите или перенастройте задачу")
}

func (s *server) deleteStorage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.d.Store.DeleteStorage(r.Context(), id)
	s.writeDeleteResult(w, err, "storage", "хранилище используется в задаче или связано с артефактами — сначала удалите их")
}

func (s *server) deleteTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.d.Store.DeleteTask(r.Context(), id)
	s.writeDeleteResult(w, err, "task", "у задачи есть активный запуск — дождитесь его завершения")
}

// deleteSecret refuses to remove a secret that any connection or storage still
// references, listing the referrers in the 409 message. The reference scan and
// the delete are separate calls; the tiny race between them is acceptable for a
// single-operator tool, and the referrers list makes the block actionable.
func (s *server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sec, err := s.d.Store.GetSecretByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "secret not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	refs, err := s.d.Store.SecretRefs(r.Context(), sec.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(refs) > 0 {
		writeError(w, http.StatusConflict, "секрет используется: "+strings.Join(refs, ", "))
		return
	}
	s.writeDeleteResult(w, s.d.Store.DeleteSecret(r.Context(), id), "secret", "секрет используется другой сущностью")
}

// writeDeleteResult maps a store Delete* error onto an HTTP status: nil → 204,
// ErrNotFound → 404, ErrInUse → 409 (with a human-facing reason), else 500.
func (s *server) writeDeleteResult(w http.ResponseWriter, err error, entity, inUseMsg string) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, sqlite.ErrNotFound):
		writeError(w, http.StatusNotFound, entity+" not found")
	case errors.Is(err, sqlite.ErrInUse):
		writeError(w, http.StatusConflict, inUseMsg)
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
