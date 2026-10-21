package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/plugin"
)

// securityRecorder collects the security events delivered in the background.
type securityRecorder struct {
	mu     sync.Mutex
	events []plugin.Event
}

func (r *securityRecorder) Name() string { return "rec" }
func (r *securityRecorder) Notify(_ context.Context, ev plugin.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

// settle waits until n events arrived, then a moment longer so an extra one
// would be caught, and returns them all.
func (r *securityRecorder) settle(t *testing.T, n int) []plugin.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		got := len(r.events)
		r.mu.Unlock()
		if got >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]plugin.Event(nil), r.events...)
}

func securityServer(t *testing.T) (*httptest.Server, *securityRecorder) {
	t.Helper()
	rec := &securityRecorder{}
	notes := core.NewNotifications(nil, nil, core.NotificationsConfig{
		ResolveSystem: func(_ context.Context, kind plugin.EventKind) []plugin.Notifier {
			if kind != plugin.EventSecurity {
				return nil
			}
			return []plugin.Notifier{rec}
		},
	}, nil)
	srv, _ := authServerWith(t, Deps{Notify: notes})
	return srv, rec
}

const (
	chromeMac  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	chromeMac2 = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	firefoxLin = "Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0"
)

func loginWithUA(t *testing.T, srv *httptest.Server, ua string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": "admin@corp.io", "password": testPassword})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/login", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: status %d", resp.StatusCode)
	}
}

// TestNewDeviceLoginReported: the first login of an account is not news, a
// browser update is not a new device, another browser is.
func TestNewDeviceLoginReported(t *testing.T) {
	srv, rec := securityServer(t)
	loginWithUA(t, srv, chromeMac)
	loginWithUA(t, srv, chromeMac2)
	if evs := rec.settle(t, 0); len(evs) != 0 {
		t.Fatalf("known device reported: %+v", evs)
	}
	loginWithUA(t, srv, firefoxLin)
	evs := rec.settle(t, 1)
	if len(evs) != 1 || evs[0].Subject != "admin@corp.io" || !strings.Contains(evs[0].Message, "Firefox · Linux") {
		t.Fatalf("events = %+v, want one new-device alert naming Firefox · Linux", evs)
	}
}

// TestLockoutReportedOnce: the attempt that locks the account and the address
// reports it; the attempts refused during the lockout do not.
func TestLockoutReportedOnce(t *testing.T) {
	srv, rec := securityServer(t)
	body := map[string]string{"email": "admin@corp.io", "password": "wrong"}
	for i := 0; i < 14; i++ {
		resp := doJSON(t, srv, http.MethodPost, "/api/login", "", body)
		resp.Body.Close()
	}
	evs := rec.settle(t, 2)
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want two: the account and the address", evs)
	}
	subjects := evs[0].Subject + " " + evs[1].Subject
	if !strings.Contains(subjects, "admin@corp.io") || !strings.Contains(subjects, "127.0.0.1") {
		t.Fatalf("subjects = %q, want the email and the IP", subjects)
	}
}

// TestUserAdministrationReported: creating, re-roling and deleting accounts
// all reach the security channels, naming who did it.
func TestUserAdministrationReported(t *testing.T) {
	srv, rec := securityServer(t)
	tok := loginAs(t, srv, core.RoleAdmin)

	resp := doJSON(t, srv, http.MethodPost, "/api/users", tok, map[string]string{
		"email": "new@corp.io", "role": "viewer", "password": "long-enough-password",
	})
	var u userDTO
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp = doJSON(t, srv, http.MethodPatch, "/api/users/"+itoa(u.ID), tok, map[string]string{"role": "admin"})
	resp.Body.Close()
	resp = doJSON(t, srv, http.MethodPatch, "/api/users/"+itoa(u.ID), tok, map[string]string{"name": "Renamed"})
	resp.Body.Close()
	resp = doJSON(t, srv, http.MethodDelete, "/api/users/"+itoa(u.ID), tok, nil)
	resp.Body.Close()

	evs := rec.settle(t, 3)
	if len(evs) != 3 {
		t.Fatalf("events = %+v, want create, role change, delete (a rename is not a security event)", evs)
	}
	for _, want := range []string{"создан", "viewer → admin", "удалён"} {
		found := false
		for _, ev := range evs {
			if ev.Subject == "new@corp.io" && strings.Contains(ev.Message, want) && strings.Contains(ev.Message, "admin@corp.io") {
				found = true
			}
		}
		if !found {
			t.Errorf("no event about new@corp.io containing %q: %+v", want, evs)
		}
	}
}

func TestDeviceFingerprint(t *testing.T) {
	a, label := deviceOf(chromeMac, "203.0.113.7")
	if label != "Chrome · macOS, 203.0.113.7" {
		t.Errorf("label = %q", label)
	}
	if b, _ := deviceOf(chromeMac2, "203.0.113.200"); b != a {
		t.Error("a browser update in the same /24 counted as a new device")
	}
	if c, _ := deviceOf(chromeMac, "198.51.100.7"); c == a {
		t.Error("another network counted as the same device")
	}
	if got := ipPrefix("2001:db8:1234:5678::1"); got != "2001:db8:1234::/48" {
		t.Errorf("ipPrefix v6 = %q", got)
	}
}
