package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/core"
)

// TestEveryRouteHasARole walks what chi actually registered and demands a
// recorded minimum role for each entry.
//
// This is the guard the TZ asks for: roles are attached per route rather than
// per middleware group, so a new endpoint added to any register* function is
// one forgotten argument away from being reachable by every role. The walk
// fails that case instead of leaving it to be discovered in production.
func TestEveryRouteHasARole(t *testing.T) {
	h, s := newRouter(Deps{Token: testToken})
	mux, ok := h.(*chi.Mux)
	if !ok {
		t.Fatalf("router is %T, want *chi.Mux", h)
	}

	// Public by design; everything else under /api must carry a role.
	exempt := map[string]bool{"POST /api/login": true}

	var missing []string
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/") {
			return nil
		}
		key := method + " " + route
		if exempt[key] {
			return nil
		}
		// chi reports the mounted path; s.roles is keyed by the sub-path used
		// at registration. Trailing slashes come from chi's own normalisation.
		sub := strings.TrimSuffix(strings.TrimPrefix(route, "/api"), "/")
		if _, ok := s.roles[method+" "+sub]; !ok {
			missing = append(missing, key)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(missing) > 0 {
		t.Fatalf("routes registered without a role (use s.get/post/patch/del):\n  %s",
			strings.Join(missing, "\n  "))
	}
	if len(s.roles) < 30 {
		t.Fatalf("only %d routes recorded — the walk is probably not seeing the real router", len(s.roles))
	}
}

// TestRoleMatrix checks one request per significant cell of TZ §6, negatives
// included: a hidden button is not a permission check, so each denial is
// asserted at the API.
func TestRoleMatrix(t *testing.T) {
	cases := []struct {
		method, path string
		viewer       int // expected status for each role
		operator     int
		admin        int
		// body overrides the default empty object for routes that enforce a
		// second, field-level check behind the role. Those answer 403 on a
		// meaningless body for a reason unrelated to the route's own role, and
		// this matrix is only about the route.
		body any
	}{
		// Reads are open to every role.
		{http.MethodGet, "/api/tasks", 200, 200, 200, nil},
		{http.MethodGet, "/api/runs", 200, 200, 200, nil},
		{http.MethodGet, "/api/connections", 200, 200, 200, nil},
		{http.MethodGet, "/api/storages", 200, 200, 200, nil},
		{http.MethodGet, "/api/retention", 200, 200, 200, nil},
		{http.MethodGet, "/api/notifiers", 200, 200, 200, nil},
		{http.MethodGet, "/api/notifications", 200, 200, 200, nil},
		{http.MethodGet, "/api/settings", 200, 200, 200, nil},
		// GET /secrets returns metadata only (no ciphertext), so it is a read.
		{http.MethodGet, "/api/secrets", 200, 200, 200, nil},
		{http.MethodGet, "/api/sessions", 200, 200, 200, nil},
		{http.MethodGet, "/api/me", 200, 200, 200, nil},

		// Artifact download is operator+: "read-only" means seeing that a
		// backup happened, not pulling down a copy of the production database.
		{http.MethodGet, "/api/artifacts/1/download", 403, 404, 404, nil},

		// Managing backups is operator+.
		{http.MethodPost, "/api/tasks/1/run", 403, 503, 503, nil},
		{http.MethodPost, "/api/retention/sweep", 403, 503, 503, nil},
		{http.MethodPost, "/api/tasks", 403, 400, 400, nil},
		{http.MethodPatch, "/api/tasks/1", 403, 400, 400, nil},
		{http.MethodDelete, "/api/tasks/1", 403, 404, 404, nil},
		{http.MethodPost, "/api/connections", 403, 400, 400, nil},
		{http.MethodDelete, "/api/connections/1", 403, 404, 404, nil},
		{http.MethodPost, "/api/storages", 403, 400, 400, nil},
		{http.MethodPost, "/api/connections/test", 403, 400, 400, nil},
		{http.MethodGet, "/api/connections/1/databases", 403, 404, 404, nil},

		// Channels: toggling and testing an existing one is operator, creating
		// or removing one is admin — a channel points at secrets.
		//
		// The PATCH body resubscribes the seeded "log" channel without touching
		// its identity. An empty body would read as a rename, which an operator
		// is separately forbidden to do; see
		// TestOperatorMayToggleChannelButNotRewriteIt for that boundary.
		{http.MethodPatch, "/api/notifiers/1", 403, 404, 404,
			map[string]any{"name": "log", "type": "log", "events": []string{"failure"}}},
		{http.MethodPost, "/api/notifiers/1/test", 403, 503, 503, nil},
		{http.MethodPost, "/api/notifiers", 403, 403, 400, nil},
		{http.MethodDelete, "/api/notifiers/1", 403, 403, 404, nil},

		// Secrets, users and settings writes are admin-only.
		{http.MethodPost, "/api/secrets", 403, 403, 503, nil},
		{http.MethodDelete, "/api/secrets/1", 403, 403, 404, nil},
		{http.MethodGet, "/api/users", 403, 403, 200, nil},
		{http.MethodPost, "/api/users", 403, 403, 400, nil},
		{http.MethodPatch, "/api/users/1", 403, 403, 400, nil},
		{http.MethodDelete, "/api/users/1", 403, 403, 409, nil},
		{http.MethodPatch, "/api/settings", 403, 403, 200, nil},
	}

	srv, _ := authServer(t)
	tokens := map[core.Role]string{
		core.RoleViewer:   loginAs(t, srv, core.RoleViewer),
		core.RoleOperator: loginAs(t, srv, core.RoleOperator),
		core.RoleAdmin:    loginAs(t, srv, core.RoleAdmin),
	}

	for _, c := range cases {
		want := map[core.Role]int{
			core.RoleViewer: c.viewer, core.RoleOperator: c.operator, core.RoleAdmin: c.admin,
		}
		for _, role := range []core.Role{core.RoleViewer, core.RoleOperator, core.RoleAdmin} {
			name := string(role) + " " + c.method + " " + c.path
			t.Run(name, func(t *testing.T) {
				// An empty JSON object keeps write handlers past their decode
				// step, so a 403 can only come from the role check.
				body := c.body
				if body == nil {
					body = map[string]string{}
				}
				resp := doJSON(t, srv, c.method, c.path, tokens[role], body)
				defer resp.Body.Close()
				got, expect := resp.StatusCode, want[role]
				// Only the allow/deny decision is under test here. Anything
				// other than 403 means the role got through to the handler,
				// which is what the matrix is about; the handler's own status
				// depends on seed data and is asserted elsewhere.
				if expect == http.StatusForbidden && got != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 (role must not reach this route)", got)
				}
				if expect != http.StatusForbidden && got == http.StatusForbidden {
					t.Fatalf("status = 403, want the role to be allowed through")
				}
			})
		}
	}
}

// TestUnauthenticatedIsRejectedNotForbidden: no credential must read as 401 so
// the SPA logs out, rather than 403 which it treats as "wrong role".
func TestUnauthenticatedIsRejectedNotForbidden(t *testing.T) {
	srv, _ := authServer(t)
	for _, path := range []string{"/api/tasks", "/api/users", "/api/me"} {
		resp := doJSON(t, srv, http.MethodGet, path, "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestRoleAtLeast(t *testing.T) {
	cases := []struct {
		have, want core.Role
		ok         bool
	}{
		{core.RoleAdmin, core.RoleViewer, true},
		{core.RoleAdmin, core.RoleAdmin, true},
		{core.RoleOperator, core.RoleViewer, true},
		{core.RoleOperator, core.RoleAdmin, false},
		{core.RoleViewer, core.RoleOperator, false},
		{core.RoleViewer, core.RoleViewer, true},
		// An unrecognised role is allowed nothing, so a typo or a row from a
		// future version fails closed rather than open.
		{core.Role("root"), core.RoleViewer, false},
		{core.Role(""), core.RoleViewer, false},
	}
	for _, c := range cases {
		if got := c.have.AtLeast(c.want); got != c.ok {
			t.Fatalf("Role(%q).AtLeast(%q) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}
