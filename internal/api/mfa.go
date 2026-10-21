package api

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/CoOre/duskrun/internal/core"
)

// MFA policy values (setting mfa_required).
const (
	settingMFARequired = "mfa_required"
	mfaPolicyOff       = "off"
	mfaPolicyAdmins    = "admins"
	mfaPolicyAll       = "all"
)

// Lifetimes of the in-memory MFA state. A login challenge outlives the code on
// screen by a comfortable margin; enrolment allows for fumbling with a phone.
const (
	mfaChallengeTTL  = 5 * time.Minute
	mfaEnrollTTL     = 10 * time.Minute
	mfaChallengeFail = 5 // wrong codes per challenge before it is discarded
)

// policyCovers reports whether policy demands a second factor from role.
func policyCovers(policy string, role core.Role) bool {
	switch policy {
	case mfaPolicyAll:
		return true
	case mfaPolicyAdmins:
		return role == core.RoleAdmin
	default:
		return false
	}
}

// mfaPolicy reads the instance policy. An unknown stored value reads as off:
// the only writer validates, so that is a hand-edited database, and failing
// closed there would lock out every user without a factor at once.
func (s *server) mfaPolicy(ctx context.Context) (string, error) {
	if s.d.Store == nil {
		return mfaPolicyOff, nil
	}
	v, err := s.d.Store.GetSetting(ctx, settingMFARequired, mfaPolicyOff)
	if err != nil {
		return "", err
	}
	if v != mfaPolicyAdmins && v != mfaPolicyAll {
		return mfaPolicyOff, nil
	}
	return v, nil
}

// newWebAuthn builds the relying party from DUSKRUN_PUBLIC_URL. An empty URL
// returns (nil, nil): passkeys are simply unavailable.
func newWebAuthn(publicURL, displayName string) (*webauthn.WebAuthn, error) {
	if publicURL == "" {
		return nil, nil
	}
	u, err := url.Parse(publicURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return nil, fmt.Errorf("DUSKRUN_PUBLIC_URL %q must be an absolute http(s) URL", publicURL)
	}
	return webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: displayName,
		RPOrigins:     []string{u.Scheme + "://" + u.Host},
		// Attestation would let the server pin authenticator models; for a
		// self-hosted tool that is policy nobody asked for, and requesting it
		// makes some platforms show an extra privacy prompt.
		AttestationPreference: protocol.PreferNoAttestation,
		// A passkey here is the second factor after a password, so user
		// verification is welcome but not required — a plain security key
		// without a PIN is still a possession factor.
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		},
	})
}

// waUser adapts an account and its stored credentials to webauthn.User.
type waUser struct {
	u     *core.User
	creds []webauthn.Credential
}

func (w waUser) WebAuthnID() []byte                         { return w.u.WebAuthnID }
func (w waUser) WebAuthnName() string                       { return w.u.Email }
func (w waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }
func (w waUser) WebAuthnDisplayName() string {
	if w.u.Name != "" {
		return w.u.Name
	}
	return w.u.Email
}

// loadWAUser decodes the user's stored credentials for the library. rows is
// returned too, so a caller that just verified an assertion can find the row
// to update without a second query.
func (s *server) loadWAUser(ctx context.Context, u *core.User) (waUser, []core.WebAuthnCredential, error) {
	rows, err := s.d.Store.ListWebAuthnCredentials(ctx, u.ID)
	if err != nil {
		return waUser{}, nil, err
	}
	creds := make([]webauthn.Credential, 0, len(rows))
	for _, r := range rows {
		var c webauthn.Credential
		if _, err := c.UnmarshalMsg(r.Data); err != nil {
			return waUser{}, nil, fmt.Errorf("decode passkey %d: %w", r.ID, err)
		}
		creds = append(creds, c)
	}
	return waUser{u: u, creds: creds}, rows, nil
}

// --- in-memory state ----------------------------------------------------

// pending is a small TTL map for state that lives between two requests of one
// ceremony: a login waiting for its second factor, a TOTP secret waiting to be
// confirmed, a WebAuthn challenge waiting for the authenticator's answer.
//
// It is deliberately not persisted. Every entry is minutes long and cheap to
// restart, the daemon is a single process, and keeping unconfirmed secrets out
// of the database means a crash mid-enrolment leaves nothing behind.
type pending[V any] struct {
	mu  sync.Mutex
	m   map[string]*pendingEntry[V]
	now func() time.Time
}

type pendingEntry[V any] struct {
	v       V
	expires time.Time
}

func newPending[V any](now func() time.Time) *pending[V] {
	return &pending[V]{m: map[string]*pendingEntry[V]{}, now: now}
}

// put stores v under key, replacing any earlier entry, and drops expired ones
// so the map cannot grow past what the TTL allows.
func (p *pending[V]) put(key string, v V, ttl time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for k, e := range p.m {
		if !now.Before(e.expires) {
			delete(p.m, k)
		}
	}
	p.m[key] = &pendingEntry[V]{v: v, expires: now.Add(ttl)}
}

// update runs fn on the live entry under the lock. fn returns whether the
// entry should be kept. ok is false when there is no live entry.
func (p *pending[V]) update(key string, fn func(v *V) (keep bool)) (ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.m[key]
	if e == nil {
		return false
	}
	if !p.now().Before(e.expires) {
		delete(p.m, key)
		return false
	}
	if !fn(&e.v) {
		delete(p.m, key)
	}
	return true
}

// get returns a copy of the live entry.
func (p *pending[V]) get(key string) (V, bool) {
	var out V
	ok := p.update(key, func(v *V) bool { out = *v; return true })
	return out, ok
}

// take returns the live entry and removes it: the single-use read.
func (p *pending[V]) take(key string) (V, bool) {
	var out V
	ok := p.update(key, func(v *V) bool { out = *v; return false })
	return out, ok
}

func (p *pending[V]) del(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, key)
}

// mfaChallenge is a login that passed the password and awaits a second factor.
type mfaChallenge struct {
	userID int64
	fails  int
	// webauthn is the assertion challenge issued by /login/mfa/webauthn/options,
	// nil until the client asks for one.
	webauthn *webauthn.SessionData
}

// passkeyEnrollment is a registration ceremony in flight.
type passkeyEnrollment struct {
	session webauthn.SessionData
	name    string
}

// mfaState groups the pending maps. Enrolment entries are keyed by user id: a
// user has at most one of each in flight, and starting again replaces it.
type mfaState struct {
	challenges *pending[mfaChallenge]      // key: TokenHash(mfa_token)
	totp       *pending[string]            // key: user id; value: base32 secret
	passkeys   *pending[passkeyEnrollment] // key: user id
}

func newMFAState(now func() time.Time) *mfaState {
	return &mfaState{
		challenges: newPending[mfaChallenge](now),
		totp:       newPending[string](now),
		passkeys:   newPending[passkeyEnrollment](now),
	}
}

func userKey(id int64) string { return strconv.FormatInt(id, 10) }
