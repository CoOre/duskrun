package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/notifier/webpush"
	"github.com/CoOre/duskrun/internal/plugin"
)

// PushService is what the push endpoints need from webpush.Service.
type PushService interface {
	Available() (publicKey, reason string)
	CheckEndpoint(endpoint string) error
	SendTo(ctx context.Context, sub core.PushSubscription, ev plugin.Event) error
}

// maxUserAgent bounds the stored User-Agent: it only labels a device.
const maxUserAgent = 300

// eventTest is the kind of a notification sent to one device by hand. It is
// never a channel subscription, so it is not in eventKinds.
const eventTest = plugin.EventKind(webpush.KindTest)

type pushStatusDTO struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
}

type pushSubscriptionDTO struct {
	ID       int64  `json:"id"`
	Endpoint string `json:"endpoint"`
	// Service names the push service host (fcm.googleapis.com, …), which is
	// what tells a Chrome subscription from a Firefox one at a glance.
	Service       string     `json:"service"`
	UserAgent     string     `json:"user_agent"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	FailCount     int        `json:"fail_count"`
}

// subscribeReq is the browser's PushSubscription.toJSON() as sent by the SPA.
type subscribeReq struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

type unsubscribeReq struct {
	Endpoint string `json:"endpoint"`
}

// registerPushRoutes wires browser push subscriptions. Every route is viewer:
// a subscription only ever reaches its owner, and which events a user gets is
// decided by push channels, which only an admin creates.
//
// Unsubscribing goes by POST with the endpoint in the body, not DELETE with a
// query string: the endpoint is a capability to push to that browser, and URLs
// end up in proxy access logs. It stays reachable at the MFA enrolment screen
// because logging out from there unsubscribes too.
func (s *server) registerPushRoutes(r chi.Router) {
	s.get(r, "/push", core.RoleViewer, s.pushStatus)
	s.get(r, "/push/subscriptions", core.RoleViewer, s.listPushSubscriptions)
	s.post(r, "/push/subscriptions", core.RoleViewer, s.subscribePush)
	s.del(r, "/push/subscriptions/{id}", core.RoleViewer, s.deletePushSubscription)
	s.post(r, "/push/subscriptions/{id}/test", core.RoleViewer, s.testPushSubscription)
	s.enrollRoute(r, http.MethodPost, "/push/unsubscribe", core.RoleViewer, s.unsubscribePush)
}

func (s *server) pushAvailability() (publicKey, reason string) {
	if s.d.Push == nil {
		return "", webpush.ReasonNoPublicURL
	}
	return s.d.Push.Available()
}

func (s *server) pushStatus(w http.ResponseWriter, _ *http.Request) {
	key, reason := s.pushAvailability()
	writeJSON(w, http.StatusOK, pushStatusDTO{Available: reason == "", Reason: reason, PublicKey: key})
}

// pushUser returns the caller when it is a person. The static API token is
// not one: it has no browser and no user row to hang a subscription on.
func (s *server) pushUser(w http.ResponseWriter, r *http.Request) (*core.User, bool) {
	p := principalFrom(r.Context())
	if p.User == nil {
		writeError(w, http.StatusForbidden, "push subscriptions belong to a user, not the API token")
		return nil, false
	}
	return p.User, true
}

func (s *server) listPushSubscriptions(w http.ResponseWriter, r *http.Request) {
	u, ok := s.pushUser(w, r)
	if !ok {
		return
	}
	subs, err := s.d.Store.ListPushSubscriptions(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]pushSubscriptionDTO, 0, len(subs))
	for _, sub := range subs {
		out = append(out, toPushSubscriptionDTO(sub))
	}
	writeJSON(w, http.StatusOK, out)
}

func toPushSubscriptionDTO(sub core.PushSubscription) pushSubscriptionDTO {
	host := ""
	if u, err := url.Parse(sub.Endpoint); err == nil {
		host = u.Hostname()
	}
	return pushSubscriptionDTO{
		ID: sub.ID, Endpoint: sub.Endpoint, Service: host, UserAgent: sub.UserAgent,
		CreatedAt: sub.CreatedAt, LastSuccessAt: sub.LastSuccessAt,
		LastError: sub.LastError, FailCount: sub.FailCount,
	}
}

func (s *server) subscribePush(w http.ResponseWriter, r *http.Request) {
	u, ok := s.pushUser(w, r)
	if !ok {
		return
	}
	if _, reason := s.pushAvailability(); reason != "" {
		writeError(w, http.StatusConflict, "push is unavailable on this instance: "+reason)
		return
	}
	var req subscribeReq
	if !decode(w, r, &req) {
		return
	}
	if err := s.d.Push.CheckEndpoint(req.Endpoint); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Checked here so a malformed key is refused now, with the browser still
	// on the page, rather than failing every delivery later.
	if b, err := decodeB64URL(req.Keys.P256dh); err != nil || len(b) != 65 || b[0] != 0x04 {
		writeError(w, http.StatusBadRequest, "keys.p256dh must be an uncompressed P-256 point")
		return
	}
	if b, err := decodeB64URL(req.Keys.Auth); err != nil || len(b) != 16 {
		writeError(w, http.StatusBadRequest, "keys.auth must be 16 bytes")
		return
	}
	id, err := s.d.Store.UpsertPushSubscription(r.Context(), core.PushSubscription{
		UserID: u.ID, Endpoint: req.Endpoint, P256dh: req.Keys.P256dh, Auth: req.Keys.Auth,
		UserAgent: clip(r.UserAgent(), maxUserAgent),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

// decodeB64URL accepts the key with or without padding: browsers send it
// unpadded, but nothing in the spec forbids the padded form.
func decodeB64URL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// clip cuts s to at most n bytes on a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func (s *server) unsubscribePush(w http.ResponseWriter, r *http.Request) {
	u, ok := s.pushUser(w, r)
	if !ok {
		return
	}
	var req unsubscribeReq
	if !decode(w, r, &req) {
		return
	}
	if req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	if err := s.d.Store.DeletePushSubscriptionByEndpoint(r.Context(), u.ID, req.Endpoint); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) deletePushSubscription(w http.ResponseWriter, r *http.Request) {
	u, ok := s.pushUser(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.d.Store.DeletePushSubscription(r.Context(), u.ID, id); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testPushSubscription sends a notification to one of the caller's devices.
// It is not written to the delivery log: that log is about channels, and the
// channel test (POST /notifiers/{id}/test) already covers a whole channel.
func (s *server) testPushSubscription(w http.ResponseWriter, r *http.Request) {
	u, ok := s.pushUser(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, reason := s.pushAvailability(); reason != "" {
		writeError(w, http.StatusConflict, "push is unavailable on this instance: "+reason)
		return
	}
	sub, err := s.d.Store.GetPushSubscription(r.Context(), u.ID, id)
	if err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	// Detached for the same reason as the channel test: closing the tab must
	// not abort a request already on its way to the push service.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), core.TestTimeout)
	defer cancel()
	err = s.d.Push.SendTo(ctx, *sub, plugin.Event{
		Kind: eventTest, Message: "test notification from duskrun", At: s.now(),
	})
	switch {
	case errors.Is(err, webpush.ErrGone):
		writeError(w, http.StatusGone, err.Error())
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
