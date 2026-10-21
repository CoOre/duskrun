package webpush

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/plugin"
)

// ErrGone is returned when the push service reports a subscription that no
// longer exists — the user revoked the permission, or the browser profile is
// gone. The subscription has been deleted by then.
var ErrGone = errors.New("push: the browser is no longer subscribed")

// ErrNoSubscribers is a channel delivery that had nowhere to go. It is an
// error so the delivery log does not record "sent" for an alert nobody got.
var ErrNoSubscribers = errors.New("push: no subscribed browsers")

// KindTest marks a notification sent to check delivery rather than to report
// an event.
const KindTest = "test"

// Payload is what the service worker receives. It renders the title from Kind
// and Task itself, so the wording lives in one place with the rest of the UI.
type Payload struct {
	Kind    string    `json:"kind"`
	Task    string    `json:"task"`
	RunID   int64     `json:"run_id,omitempty"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
	// URL is the page a click on the notification opens.
	URL string `json:"url"`
}

// Payload size caps. A push message is limited to 4 KB, and the library pads
// every record to exactly that, so the plaintext must stay under about 3.9 KB.
// These leave room for the JSON around them even if every byte were escaped.
const (
	maxMessageBytes = 1800
	maxTaskBytes    = 200
)

// concurrency bounds simultaneous requests during one broadcast.
const concurrency = 8

// payloadFor turns an event into the message a browser receives.
func payloadFor(ev plugin.Event) Payload {
	p := Payload{
		Kind:    string(ev.Kind),
		Task:    truncate(ev.Task, maxTaskBytes),
		RunID:   ev.RunID,
		Message: truncate(ev.Message, maxMessageBytes),
		At:      ev.At,
		URL:     "/",
	}
	switch {
	case ev.RunID != 0:
		p.URL = fmt.Sprintf("/history?at=%d", ev.RunID)
	case ev.Kind == plugin.EventRetentionError:
		p.URL = "/retention"
	case ev.Kind == plugin.EventSuccess:
		// A real success always names its run; one without is the channel
		// test, and must not show up on a phone as a completed backup.
		p.Kind, p.Task, p.URL = KindTest, "", "/notifications"
	}
	return p
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ellipsis = "…"
	cut := n - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// options are the delivery headers for an event kind. A failure is worth
// waking the phone for and still worth showing a day later; a success is
// neither.
func (s *Service) options(p Payload) *webpush.Options {
	o := &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.subject,
		VAPIDPublicKey:  s.publicKey,
		VAPIDPrivateKey: s.privateKey,
		TTL:             int((24 * time.Hour).Seconds()),
		Urgency:         webpush.UrgencyHigh,
		Topic:           topic(p.Kind, p.Task),
	}
	if p.Kind == string(plugin.EventSuccess) {
		o.TTL = int(time.Hour.Seconds())
		o.Urgency = webpush.UrgencyNormal
	}
	return o
}

// topic collapses undelivered messages: a phone back online after a night
// offline shows the latest failure of a task, not one per retry. It is keyed
// by kind as well, so a later success never hides an earlier failure. RFC 8030
// limits it to 32 URL-safe base64 characters.
func topic(kind, task string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + task))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:32]
}

// SendTo delivers ev to one subscription and records the outcome on it.
func (s *Service) SendTo(ctx context.Context, sub core.PushSubscription, ev plugin.Event) error {
	if err := s.unavailableError(); err != nil {
		return err
	}
	err := s.send(ctx, sub, payloadFor(ev))
	switch {
	case errors.Is(err, ErrGone):
		if derr := s.store.DeletePushSubscriptionByEndpoint(context.WithoutCancel(ctx), 0, sub.Endpoint); derr != nil {
			s.log.Error("push: drop expired subscription failed", "subscription", sub.ID, "err", derr)
		}
	case err != nil:
		s.mark(ctx, sub.ID, err.Error())
	default:
		s.mark(ctx, sub.ID, "")
	}
	return err
}

func (s *Service) mark(ctx context.Context, id int64, cause string) {
	if err := s.store.MarkPushResult(context.WithoutCancel(ctx), id, cause); err != nil {
		s.log.Error("push: record delivery result failed", "subscription", id, "err", err)
	}
}

// send makes the request. The endpoint is checked here as well as when it was
// saved, so narrowing DUSKRUN_PUSH_ENDPOINTS also stops subscriptions stored
// before the change.
func (s *Service) send(ctx context.Context, sub core.PushSubscription, p Payload) error {
	if err := s.CheckEndpoint(sub.Endpoint); err != nil {
		return err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("push: marshal payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	resp, err := webpush.SendNotificationWithContext(ctx, body, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, s.options(p))
	if err != nil {
		// net/http errors quote the URL, and an endpoint is a bearer
		// capability for that browser: keep it out of the log.
		return fmt.Errorf("push: send: %s", strings.ReplaceAll(err.Error(), sub.Endpoint, "<endpoint>"))
	}
	defer resp.Body.Close()
	// A short excerpt names the cause where the status alone does not (Apple
	// answers 403 with {"reason":"BadJwtToken"}, FCM with a sentence).
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	}
	msg := fmt.Sprintf("push: service answered %d", resp.StatusCode)
	if text := strings.Join(strings.Fields(string(snippet)), " "); text != "" && utf8.ValidString(text) {
		msg += ": " + text
	}
	return errors.New(msg)
}

// Broadcast delivers ev to every subscription whose owner holds at least
// minRole. One browser failing does not fail the delivery: the error reports
// only a delivery that reached nobody, and per-browser failures are recorded
// on the subscriptions, where the owner sees them.
func (s *Service) Broadcast(ctx context.Context, minRole core.Role, ev plugin.Event) error {
	if err := s.unavailableError(); err != nil {
		return err
	}
	all, err := s.store.PushTargets(ctx)
	if err != nil {
		return fmt.Errorf("push: list subscriptions: %w", err)
	}
	targets := all[:0]
	for _, sub := range all {
		if sub.Role.AtLeast(minRole) {
			targets = append(targets, sub)
		}
	}
	if len(targets) == 0 {
		return ErrNoSubscribers
	}

	var (
		mu        sync.Mutex
		delivered int
		gone      int
		failures  []error
		wg        sync.WaitGroup
		sem       = make(chan struct{}, concurrency)
	)
	for _, sub := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			err := s.SendTo(ctx, sub, ev)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				delivered++
			case errors.Is(err, ErrGone):
				gone++
			default:
				failures = append(failures, err)
			}
		}()
	}
	wg.Wait()

	switch {
	case delivered > 0:
		return nil
	case len(failures) == 0:
		// Every subscription turned out to be expired and was dropped.
		return ErrNoSubscribers
	}
	return fmt.Errorf("0/%d delivered: %w", len(targets)-gone, failures[0])
}
