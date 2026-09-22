package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/auth"
	"github.com/duskrun/duskrun/internal/core"
)

// minPasswordLen is a floor, not a policy. Composition rules ("one digit, one
// symbol") push people towards shorter, more guessable passwords; length is the
// property argon2 cannot make up for.
const minPasswordLen = 10

// registerUserRoutes wires account management. Every route is admin-only —
// creating an account grants access to the instance, and changing a role grants
// more of it.
func (s *server) registerUserRoutes(r chi.Router) {
	s.get(r, "/users", core.RoleAdmin, s.listUsers)
	s.post(r, "/users", core.RoleAdmin, s.createUser)
	s.patch(r, "/users/{id}", core.RoleAdmin, s.updateUser)
	s.del(r, "/users/{id}", core.RoleAdmin, s.deleteUser)
}

func (s *server) listUsers(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}
	users, err := s.d.Store.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		out = append(out, toUserDTO(u))
	}
	writeJSON(w, http.StatusOK, out)
}

type createUserReq struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Password string `json:"password"`
}

func (s *server) createUser(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}
	var req createUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email, err := validateEmail(req.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	role := core.Role(strings.TrimSpace(req.Role))
	if !role.Valid() {
		writeError(w, http.StatusBadRequest, "role must be one of admin, operator, viewer")
		return
	}
	if err := validatePassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	if !s.acquirePW(ctx) {
		writeError(w, http.StatusServiceUnavailable, "server busy, retry shortly")
		return
	}
	hash, err := auth.Hash(req.Password)
	s.releasePW()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	id, err := s.d.Store.CreateUser(ctx, core.User{
		Email: email, Name: strings.TrimSpace(req.Name), Role: role, PasswordHash: hash,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a user with this email already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	u, err := s.d.Store.GetUser(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toUserDTO(*u))
}

// updateUserReq uses pointers so an omitted field keeps its stored value, the
// same shape PATCH /notifiers/{id} already uses.
type updateUserReq struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
	Password *string `json:"password"` // admin reset; ends every session of that user
}

func (s *server) updateUser(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req updateUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	ctx := r.Context()
	cur, err := s.d.Store.GetUser(ctx, id)
	if err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}

	next := *cur
	if req.Name != nil {
		next.Name = strings.TrimSpace(*req.Name)
	}
	if req.Role != nil {
		role := core.Role(strings.TrimSpace(*req.Role))
		if !role.Valid() {
			writeError(w, http.StatusBadRequest, "role must be one of admin, operator, viewer")
			return
		}
		next.Role = role
	}
	if req.Disabled != nil {
		next.Disabled = *req.Disabled
	}

	// Self-targeting is refused before the store's last-admin check, because
	// the two failures need different explanations: an admin locking themselves
	// out of their own session is a mistake even when other admins remain.
	p := principalFrom(ctx)
	if p.UserID() == id && (next.Disabled != cur.Disabled || next.Role != cur.Role) {
		writeError(w, http.StatusConflict, "cannot change your own role or disable yourself")
		return
	}

	// The password is validated and hashed before anything is written: the UI
	// sends name, role and password in one request, so a 400 here after the
	// role had already been committed would report a failure that half
	// happened.
	hash := ""
	if req.Password != nil {
		if err := validatePassword(*req.Password); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !s.acquirePW(ctx) {
			writeError(w, http.StatusServiceUnavailable, "server busy, retry shortly")
			return
		}
		hash, err = auth.Hash(*req.Password)
		s.releasePW()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if err := s.d.Store.UpdateUser(ctx, next); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}

	if hash != "" {
		// An admin resetting someone else's password wants every client of
		// that account kicked, including the one that may have prompted the
		// reset — but an admin who resets their own from this form should no
		// more be logged out than they are by POST /me/password.
		keep := int64(0)
		if p.UserID() == id && p.Session != nil {
			keep = p.Session.ID
		}
		if err := s.d.Store.SetUserPassword(ctx, id, hash, keep); err != nil {
			status, msg := storeErrStatus(err)
			writeError(w, status, msg)
			return
		}
	}

	out, err := s.d.Store.GetUser(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toUserDTO(*out))
}

func (s *server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if p := principalFrom(r.Context()); p.UserID() == id {
		writeError(w, http.StatusConflict, "cannot delete your own account")
		return
	}
	if err := s.d.Store.DeleteUser(r.Context(), id); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateEmail normalises and checks the login address.
func validateEmail(raw string) (string, error) {
	e := normalizeEmail(raw)
	if e == "" {
		return "", fmt.Errorf("email is required")
	}
	if _, err := mail.ParseAddress(e); err != nil {
		return "", fmt.Errorf("email is not a valid address")
	}
	return e, nil
}

// validatePassword enforces a length floor, counted in runes so a passphrase in
// Cyrillic is not judged by its byte count.
func validatePassword(p string) error {
	if utf8.RuneCountInString(p) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	return nil
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// isUniqueViolation spots the duplicate-email case so it becomes a 409 rather
// than a 500 with a driver message in it.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED")
}
