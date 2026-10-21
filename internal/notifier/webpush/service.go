// Package webpush delivers notifications to browsers through the Web Push
// protocol (RFC 8030), with payload encryption (RFC 8291) and VAPID server
// identification (RFC 8292).
//
// Unlike the other notifiers it needs the database — subscriptions are rows,
// and so is the signing key — so it is not registered from init(): serve builds
// a Service and calls Register with it.
package webpush

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/CoOre/duskrun/internal/core"
)

// Store is the persistence Service needs.
type Store interface {
	InitVAPIDKeys(ctx context.Context, k core.VAPIDKeys) (*core.VAPIDKeys, error)
	PushTargets(ctx context.Context) ([]core.PushSubscription, error)
	DeletePushSubscriptionByEndpoint(ctx context.Context, userID int64, endpoint string) error
	MarkPushResult(ctx context.Context, id int64, cause string) error
}

// Box seals the VAPID private key at rest and opens it at startup.
// *secret.Box implements it.
type Box interface {
	Seal(name, typ string, plaintext []byte) (core.Secret, error)
	Open(s core.Secret) ([]byte, error)
}

// Reasons push can be unavailable on an instance, reported to the UI as-is.
const (
	ReasonNoPublicURL    = "no_public_url"
	ReasonInsecureOrigin = "insecure_origin"
	ReasonNoMasterKey    = "no_master_key"
	ReasonKeyUnreadable  = "key_unreadable"
)

// DefaultHosts are the push services of the browsers in use: Chrome, Edge and
// other Chromium builds (FCM), Firefox (autopush), Safari (Apple) and legacy
// Edge (WNS). A leading dot matches any subdomain.
var DefaultHosts = []string{
	"fcm.googleapis.com",
	"updates.push.services.mozilla.com",
	".push.apple.com",
	".notify.windows.com",
}

// sendTimeout bounds one request to a push service.
const sendTimeout = 10 * time.Second

// Config configures a Service.
type Config struct {
	Store Store
	// Box may be nil, which leaves push unavailable: the signing key cannot be
	// stored without the master key.
	Box Box
	// PublicURL is DUSKRUN_PUBLIC_URL. Browsers only offer push to a secure
	// origin, and it doubles as the VAPID contact.
	PublicURL string
	// ExtraHosts extends DefaultHosts (DUSKRUN_PUSH_ENDPOINTS).
	ExtraHosts []string
	// Client sends the requests. Nil uses a client with sendTimeout. Redirects
	// are refused either way.
	Client *http.Client
	Log    *slog.Logger
}

// Service holds the instance's signing keys and sends pushes.
type Service struct {
	store   Store
	log     *slog.Logger
	client  *http.Client
	hosts   []string
	subject string

	// reason is non-empty when push is unavailable; the keys are then unset.
	reason     string
	publicKey  string
	privateKey string
}

// New builds the Service, generating the VAPID key pair on first use. An
// instance that cannot offer push still gets a Service — one that reports why —
// so the UI can explain instead of failing; only an unexpected storage error is
// returned.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	client := &http.Client{Timeout: sendTimeout}
	if cfg.Client != nil {
		c := *cfg.Client
		client = &c
	}
	// A push service has no reason to redirect, and following one would send
	// the signed request to a host the allowlist never approved.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	s := &Service{
		store:  cfg.Store,
		log:    cfg.Log,
		client: client,
		hosts:  append(append([]string(nil), DefaultHosts...), cfg.ExtraHosts...),
	}
	s.reason, s.subject = originCheck(cfg.PublicURL)
	if s.reason == "" && cfg.Box == nil {
		s.reason = ReasonNoMasterKey
	}
	if s.reason != "" {
		return s, nil
	}

	if err := s.loadKeys(ctx, cfg.Box); err != nil {
		return nil, err
	}
	return s, nil
}

// loadKeys reads the stored key pair, creating one when there is none.
func (s *Service) loadKeys(ctx context.Context, box Box) error {
	// A fresh pair is offered on every start and kept only when none is stored:
	// that is one cheap key generation, and it leaves the first-writer-wins
	// decision to a single statement instead of a read-then-write race.
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return fmt.Errorf("webpush: generate vapid keys: %w", err)
	}
	sealed, err := box.Seal("vapid", "vapid", []byte(priv))
	if err != nil {
		return fmt.Errorf("webpush: seal vapid key: %w", err)
	}
	stored, err := s.store.InitVAPIDKeys(ctx, core.VAPIDKeys{
		PublicKey: pub, PrivateKey: sealed.Ciphertext, KeyID: sealed.KeyID,
	})
	if err != nil {
		return fmt.Errorf("webpush: %w", err)
	}
	plain, err := box.Open(core.Secret{Ciphertext: stored.PrivateKey, KeyID: stored.KeyID})
	if err != nil {
		// The master key changed under a stored pair. Generating a new one would
		// silently orphan every subscription, so say so and leave push off.
		s.log.Error("push disabled: the stored VAPID key cannot be opened with the current master key", "err", err)
		s.reason = ReasonKeyUnreadable
		return nil
	}
	s.publicKey, s.privateKey = stored.PublicKey, string(plain)
	return nil
}

// originCheck reports why publicURL cannot host push ("" when it can) and the
// VAPID subject to sign with.
func originCheck(publicURL string) (reason, subject string) {
	if publicURL == "" {
		return ReasonNoPublicURL, ""
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		return ReasonNoPublicURL, ""
	}
	switch {
	case u.Scheme == "https":
		return "", u.Scheme + "://" + u.Host
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		// Browsers treat localhost as a secure context, so development works
		// over plain HTTP. Push services want an https: or mailto: contact; the
		// library turns a bare address into mailto:.
		return "", "duskrun@localhost"
	}
	return ReasonInsecureOrigin, ""
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Available reports the public key browsers subscribe with, or why push is
// off on this instance.
func (s *Service) Available() (publicKey, reason string) {
	if s == nil {
		return "", ReasonNoPublicURL
	}
	return s.publicKey, s.reason
}

// unavailableError explains a Service that cannot send, for the channel form
// and the delivery log.
func (s *Service) unavailableError() error {
	switch s.reason {
	case ReasonNoPublicURL:
		return fmt.Errorf("push: DUSKRUN_PUBLIC_URL is not set")
	case ReasonInsecureOrigin:
		return fmt.Errorf("push: DUSKRUN_PUBLIC_URL must be https:// (or http://localhost)")
	case ReasonNoMasterKey:
		return fmt.Errorf("push: no master key to store the signing key with")
	case ReasonKeyUnreadable:
		return fmt.Errorf("push: the stored signing key cannot be opened with the current master key")
	}
	return nil
}

// CheckEndpoint refuses a subscription endpoint the server must not POST to.
// Any signed-in user may store one, so without this the server would make
// requests to whatever address a viewer supplied — including internal ones.
func (s *Service) CheckEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("push: endpoint is not a URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("push: endpoint must be https")
	}
	if u.User != nil {
		return fmt.Errorf("push: endpoint must not carry credentials")
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range s.hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		switch {
		case h == "":
		case strings.HasPrefix(h, "."):
			if strings.HasSuffix(host, h) {
				return nil
			}
		case host == h:
			return nil
		}
	}
	return fmt.Errorf("push: %s is not a known push service (see DUSKRUN_PUSH_ENDPOINTS)", host)
}
