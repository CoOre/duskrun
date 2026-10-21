package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/CoOre/duskrun/internal/auth"
	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/store/sqlite"
)

// registerMFARoutes wires second-factor self-service and the admin reset.
//
// Every route that adds, removes or replaces a factor asks for the account
// password again, for the reason changeOwnPassword does: a stolen session
// token must not be enough to bind the attacker's authenticator to the account
// (which would lock the owner out at their next login) or to strip the owner's.
// Confirming a TOTP secret or a new passkey needs no second prompt — the
// password was checked when that enrolment was started.
func (s *server) registerMFARoutes(r chi.Router) {
	s.enrollRoute(r, http.MethodGet, "/me/mfa", core.RoleViewer, s.mfaStatus)
	s.enrollRoute(r, http.MethodPost, "/me/mfa/totp/setup", core.RoleViewer, s.totpSetup)
	s.enrollRoute(r, http.MethodPost, "/me/mfa/totp/confirm", core.RoleViewer, s.totpConfirm)
	s.post(r, "/me/mfa/totp/disable", core.RoleViewer, s.totpDisable)
	s.enrollRoute(r, http.MethodPost, "/me/mfa/passkeys/options", core.RoleViewer, s.passkeyOptions)
	s.enrollRoute(r, http.MethodPost, "/me/mfa/passkeys", core.RoleViewer, s.passkeyRegister)
	s.del(r, "/me/mfa/passkeys/{id}", core.RoleViewer, s.passkeyDelete)
	s.post(r, "/me/mfa/recovery-codes", core.RoleViewer, s.regenerateRecoveryCodes)
	s.post(r, "/users/{id}/mfa/reset", core.RoleAdmin, s.resetUserMFA)
}

// --- login, second step -------------------------------------------------

// startMFAChallenge answers a correct password on an account with 2FA. The
// mfa_token is as random as a session token and kept only as its hash, but it
// grants nothing except an attempt at the second factor for mfaChallengeTTL.
func (s *server) startMFAChallenge(w http.ResponseWriter, user *core.User) {
	token, err := auth.NewSessionToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mfa.challenges.put(auth.TokenHash(token), mfaChallenge{userID: user.ID}, mfaChallengeTTL)

	methods := make([]string, 0, 3)
	if user.TOTPEnabled {
		methods = append(methods, "totp")
	}
	if user.Passkeys > 0 && s.webauthn != nil {
		methods = append(methods, "webauthn")
	}
	methods = append(methods, "recovery")
	writeJSON(w, http.StatusOK, mfaChallengeResp{MFARequired: true, MFAToken: token, MFAMethods: methods})
}

// errChallengeGone is the 410 answer: the challenge expired, was used up by
// wrong codes, or never existed. The client goes back to the password step.
const errChallengeGone = "sign-in attempt expired, sign in again"

type mfaTokenReq struct {
	MFAToken string `json:"mfa_token"`
}

// loginMFAWebAuthnOptions issues an assertion challenge for the pending login.
// It is a separate round trip because the challenge must be fresh and bound to
// this attempt; it is stored on the login challenge and consumed by /login/mfa.
func (s *server) loginMFAWebAuthnOptions(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}
	var req mfaTokenReq
	if !decode(w, r, &req) {
		return
	}
	if s.webauthn == nil {
		writeError(w, http.StatusConflict, "passkeys are not configured on this instance (DUSKRUN_PUBLIC_URL)")
		return
	}
	key := auth.TokenHash(req.MFAToken)
	ch, ok := s.mfa.challenges.get(key)
	if !ok {
		writeError(w, http.StatusGone, errChallengeGone)
		return
	}
	ctx := r.Context()
	user, err := s.d.Store.GetUser(ctx, ch.userID)
	if err != nil {
		writeError(w, http.StatusGone, errChallengeGone)
		return
	}
	wu, _, err := s.loadWAUser(ctx, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(wu.creds) == 0 {
		writeError(w, http.StatusConflict, "this account has no passkeys")
		return
	}
	assertion, session, err := s.webauthn.BeginLogin(wu)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !s.mfa.challenges.update(key, func(c *mfaChallenge) bool { c.webauthn = session; return true }) {
		writeError(w, http.StatusGone, errChallengeGone)
		return
	}
	writeJSON(w, http.StatusOK, assertion)
}

type loginMFAReq struct {
	MFAToken   string          `json:"mfa_token"`
	Method     string          `json:"method"` // totp | recovery | webauthn
	Code       string          `json:"code"`
	Credential json.RawMessage `json:"credential"`
}

// loginMFA completes a login with the second factor.
//
// Guessing is bounded twice: each challenge allows mfaChallengeFail wrong
// answers before it is thrown away (and the password must be entered again),
// and the shared login limiter counts failures per account and per address, so
// re-entering the password does not reset the budget.
func (s *server) loginMFA(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}
	var req loginMFAReq
	if !decode(w, r, &req) {
		return
	}
	key := auth.TokenHash(req.MFAToken)
	ch, ok := s.mfa.challenges.get(key)
	if !ok {
		writeError(w, http.StatusGone, errChallengeGone)
		return
	}

	keys := []string{"mfa:" + strconv.FormatInt(ch.userID, 10)}
	if ip := clientIP(r, s.d.TrustProxy); ip != "" {
		keys = append(keys, "mfa-ip:"+ip)
	}
	for _, k := range keys {
		if blocked, retry := s.limiter.blocked(k); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
			return
		}
	}

	ctx := r.Context()
	user, err := s.d.Store.GetUser(ctx, ch.userID)
	if err != nil || user.Disabled {
		// Deleted or disabled between the two steps: the attempt is over.
		s.mfa.challenges.del(key)
		writeError(w, http.StatusGone, errChallengeGone)
		return
	}

	var (
		verified  bool
		codesLeft *int
	)
	switch req.Method {
	case "totp":
		verified, err = s.verifyLoginTOTP(r, user, req.Code)
	case "recovery":
		verified, err = s.d.Store.UseRecoveryCode(ctx, user.ID, auth.RecoveryCodeHash(req.Code), s.now())
		if verified {
			n, cerr := s.d.Store.CountRecoveryCodes(ctx, user.ID)
			if cerr == nil {
				codesLeft = &n
			}
		}
	case "webauthn":
		verified, err = s.verifyLoginPasskey(r, key, user, req.Credential)
	default:
		writeError(w, http.StatusBadRequest, "method must be one of totp, recovery, webauthn")
		return
	}
	if err != nil {
		s.d.Log.Error("login: verify second factor", "user", user.ID, "method", req.Method, "err", err)
		writeError(w, http.StatusServiceUnavailable, "could not verify the second factor, retry shortly")
		return
	}

	if !verified {
		s.mfa.challenges.update(key, func(c *mfaChallenge) bool {
			c.fails++
			return c.fails < mfaChallengeFail
		})
		delay := s.limiter.fail(keys...)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
		writeError(w, http.StatusUnauthorized, "invalid code")
		return
	}

	// take, not del: of two requests that both verified (two tabs, a double
	// click), only the one that removes the challenge gets a session.
	if _, ok := s.mfa.challenges.take(key); !ok {
		writeError(w, http.StatusGone, errChallengeGone)
		return
	}
	s.limiter.succeed(keys...)
	s.issueSession(w, r, user, codesLeft)
}

// verifyLoginTOTP checks a code against the user's secret and records its time
// step, so the same code cannot complete a second login.
func (s *server) verifyLoginTOTP(r *http.Request, user *core.User, code string) (bool, error) {
	if !user.TOTPEnabled {
		return false, nil
	}
	ctx := r.Context()
	t, err := s.d.Store.GetTOTP(ctx, user.ID)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	secret, err := s.openTOTP(t)
	if err != nil {
		return false, err
	}
	step, ok := auth.VerifyTOTP(secret, code, s.now(), t.LastStep)
	if !ok {
		return false, nil
	}
	return s.d.Store.AdvanceTOTPStep(ctx, user.ID, step)
}

// verifyLoginPasskey validates an assertion against the challenge issued for
// this login. The challenge is consumed by the attempt whether it succeeds or
// not: a WebAuthn challenge is single-use by design.
func (s *server) verifyLoginPasskey(r *http.Request, key string, user *core.User, raw json.RawMessage) (bool, error) {
	if s.webauthn == nil || len(raw) == 0 {
		return false, nil
	}
	var session *webauthn.SessionData
	s.mfa.challenges.update(key, func(c *mfaChallenge) bool {
		session, c.webauthn = c.webauthn, nil
		return true
	})
	if session == nil {
		return false, nil
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return false, nil
	}
	ctx := r.Context()
	wu, rows, err := s.loadWAUser(ctx, user)
	if err != nil {
		return false, err
	}
	cred, err := s.webauthn.ValidateLogin(wu, *session, parsed)
	if err != nil {
		return false, nil
	}
	for _, row := range rows {
		if bytes.Equal(row.CredentialID, cred.ID) {
			data, err := cred.MarshalMsg(nil)
			if err != nil {
				return false, err
			}
			// The updated sign counter is what lets the next assertion detect
			// a cloned authenticator; losing it is not worth failing a login.
			if err := s.d.Store.TouchWebAuthnCredential(ctx, row.ID, data, s.now()); err != nil {
				s.d.Log.Warn("store passkey counter", "credential", row.ID, "err", err)
			}
			return true, nil
		}
	}
	return false, nil
}

// openTOTP decrypts a stored TOTP secret.
func (s *server) openTOTP(t *core.TOTP) (string, error) {
	if s.opener == nil {
		return "", errors.New("no master key: TOTP secrets cannot be opened")
	}
	b, err := s.opener.Open(core.Secret{Ciphertext: t.Secret, KeyID: t.KeyID})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// --- self-service -------------------------------------------------------

type passkeyDTO struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func toPasskeyDTO(c core.WebAuthnCredential) passkeyDTO {
	return passkeyDTO{ID: c.ID, Name: c.Name, CreatedAt: c.CreatedAt, LastUsedAt: c.LastUsedAt}
}

type mfaStatusDTO struct {
	TOTP              bool         `json:"totp"`
	Passkeys          []passkeyDTO `json:"passkeys"`
	RecoveryCodesLeft int          `json:"recovery_codes_left"`
	WebAuthnAvailable bool         `json:"webauthn_available"`
	Policy            string       `json:"policy"`
	// Required means the policy covers this user, so removing the last factor
	// will be refused.
	Required bool `json:"required"`
}

// mfaUser returns the calling account, or writes the reason there is none.
func (s *server) mfaUser(w http.ResponseWriter, r *http.Request) (*principal, bool) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return nil, false
	}
	p := principalFrom(r.Context())
	if p.User == nil {
		writeError(w, http.StatusBadRequest, "the static API token has no second factor")
		return nil, false
	}
	return p, true
}

func (s *server) mfaStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	rows, err := s.d.Store.ListWebAuthnCredentials(ctx, p.User.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	left, err := s.d.Store.CountRecoveryCodes(ctx, p.User.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	policy, err := s.mfaPolicy(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := mfaStatusDTO{
		TOTP:              p.User.TOTPEnabled,
		Passkeys:          make([]passkeyDTO, 0, len(rows)),
		RecoveryCodesLeft: left,
		WebAuthnAvailable: s.webauthn != nil,
		Policy:            policy,
		Required:          policyCovers(policy, p.User.Role),
	}
	for _, c := range rows {
		out.Passkeys = append(out.Passkeys, toPasskeyDTO(c))
	}
	writeJSON(w, http.StatusOK, out)
}

type passwordReq struct {
	Password string `json:"password"`
}

// confirmPassword re-checks the caller's password, writing the failure itself.
// Wrong is 403, never 401: the session is fine, and a 401 would sign the SPA
// out over a typo.
func (s *server) confirmPassword(w http.ResponseWriter, r *http.Request, u *core.User, password string) bool {
	if password == "" {
		writeError(w, http.StatusBadRequest, "password is required")
		return false
	}
	if !s.acquirePW(r.Context()) {
		writeError(w, http.StatusServiceUnavailable, "server busy, retry shortly")
		return false
	}
	ok, err := auth.Verify(u.PasswordHash, password)
	s.releasePW()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	if !ok {
		writeError(w, http.StatusForbidden, "current password is incorrect")
		return false
	}
	return true
}

// firstFactorCodes returns a fresh recovery set when u has no factor yet — the
// moment 2FA turns on is the moment a fallback becomes necessary. For a user
// who already has one, it returns nils and the existing codes stay.
func firstFactorCodes(u *core.User) (codes, hashes []string, err error) {
	if u.MFAEnabled() {
		return nil, nil, nil
	}
	return auth.NewRecoveryCodes()
}

type totpSetupResp struct {
	Secret string `json:"secret"`
	URL    string `json:"otpauth_url"`
	QR     string `json:"qr"`
}

// totpSetup generates a secret and holds it, unconfirmed, until the user
// proves their app produces matching codes. Nothing is stored before that: an
// abandoned setup must not leave a half-enrolled factor that blocks login.
func (s *server) totpSetup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	if s.d.Secrets == nil || s.opener == nil {
		writeError(w, http.StatusServiceUnavailable, "TOTP needs the master key to store its secret")
		return
	}
	var req passwordReq
	if !decode(w, r, &req) {
		return
	}
	if !s.confirmPassword(w, r, p.User, req.Password) {
		return
	}
	issuer, err := s.d.Store.GetSetting(r.Context(), settingInstanceName, defaultInstanceName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	enr, err := auth.NewTOTP(issuer, p.User.Email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mfa.totp.put(userKey(p.User.ID), enr.Secret, mfaEnrollTTL)
	writeJSON(w, http.StatusOK, totpSetupResp{Secret: enr.Secret, URL: enr.URL, QR: enr.QRPNG})
}

type codeReq struct {
	Code string `json:"code"`
}

type recoveryCodesResp struct {
	RecoveryCodes []string `json:"recovery_codes,omitempty"`
}

// totpConfirm stores the pending secret once a code from it verifies. On an
// account that already has TOTP this replaces the old secret — the "new phone"
// path, which never leaves the account without a working factor.
func (s *server) totpConfirm(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	var req codeReq
	if !decode(w, r, &req) {
		return
	}
	k := userKey(p.User.ID)
	secret, ok := s.mfa.totp.get(k)
	if !ok {
		writeError(w, http.StatusConflict, "no TOTP setup in progress, start again")
		return
	}
	step, ok := auth.VerifyTOTP(secret, req.Code, s.now(), 0)
	if !ok {
		// 400, not 401: see confirmPassword.
		writeError(w, http.StatusBadRequest, "invalid code")
		return
	}
	sealed, err := s.d.Secrets.Seal("totp", "totp", []byte(secret))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	codes, hashes, err := firstFactorCodes(p.User)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.d.Store.PutTOTP(r.Context(), core.TOTP{
		UserID: p.User.ID, Secret: sealed.Ciphertext, KeyID: sealed.KeyID, LastStep: step,
	}, hashes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mfa.totp.del(k)
	writeJSON(w, http.StatusOK, recoveryCodesResp{RecoveryCodes: codes})
}

// lastFactorGuard refuses removing the account's only factor while the policy
// requires one. Without it a user could switch 2FA off and keep working until
// their next request, which the enrolment gate would then block anyway — the
// refusal says why up front.
func (s *server) lastFactorGuard(w http.ResponseWriter, r *http.Request, u *core.User, remaining int) bool {
	if remaining > 0 {
		return true
	}
	policy, err := s.mfaPolicy(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	if policyCovers(policy, u.Role) {
		writeError(w, http.StatusConflict,
			"two-factor authentication is required on this instance; add another factor before removing this one")
		return false
	}
	return true
}

func (s *server) totpDisable(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	var req passwordReq
	if !decode(w, r, &req) {
		return
	}
	if !p.User.TOTPEnabled {
		writeError(w, http.StatusConflict, "TOTP is not enabled")
		return
	}
	if !s.lastFactorGuard(w, r, p.User, p.User.Passkeys) {
		return
	}
	if !s.confirmPassword(w, r, p.User, req.Password) {
		return
	}
	if err := s.d.Store.DeleteTOTP(r.Context(), p.User.ID); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type passkeyOptionsReq struct {
	Password string `json:"password"`
	Name     string `json:"name"`
}

func (s *server) passkeyOptions(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	if s.webauthn == nil {
		writeError(w, http.StatusConflict, "passkeys are not configured on this instance (DUSKRUN_PUBLIC_URL)")
		return
	}
	var req passkeyOptionsReq
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Passkey"
	}
	if utf8.RuneCountInString(name) > 64 {
		writeError(w, http.StatusBadRequest, "name must be at most 64 characters")
		return
	}
	if !s.confirmPassword(w, r, p.User, req.Password) {
		return
	}
	wu, _, err := s.loadWAUser(r.Context(), p.User)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Excluding the user's existing credentials makes the browser refuse to
	// register the same authenticator twice, instead of silently adding a
	// duplicate row that looks like a second, independent key.
	creation, session, err := s.webauthn.BeginRegistration(wu,
		webauthn.WithExclusions(webauthn.Credentials(wu.creds).CredentialDescriptors()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mfa.passkeys.put(userKey(p.User.ID), passkeyEnrollment{session: *session, name: name}, mfaEnrollTTL)
	writeJSON(w, http.StatusOK, creation)
}

type passkeyRegisterReq struct {
	Credential json.RawMessage `json:"credential"`
}

type passkeyRegisterResp struct {
	Passkey       passkeyDTO `json:"passkey"`
	RecoveryCodes []string   `json:"recovery_codes,omitempty"`
}

func (s *server) passkeyRegister(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	if s.webauthn == nil {
		writeError(w, http.StatusConflict, "passkeys are not configured on this instance (DUSKRUN_PUBLIC_URL)")
		return
	}
	var req passkeyRegisterReq
	if !decode(w, r, &req) {
		return
	}
	// Single use: a failed attempt needs a fresh challenge from /options.
	enr, ok := s.mfa.passkeys.take(userKey(p.User.ID))
	if !ok {
		writeError(w, http.StatusConflict, "no passkey registration in progress, start again")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		writeError(w, http.StatusBadRequest, "malformed credential: "+err.Error())
		return
	}
	ctx := r.Context()
	wu, _, err := s.loadWAUser(ctx, p.User)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cred, err := s.webauthn.CreateCredential(wu, enr.session, parsed)
	if err != nil {
		writeError(w, http.StatusBadRequest, "passkey verification failed: "+err.Error())
		return
	}
	data, err := cred.MarshalMsg(nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	codes, hashes, err := firstFactorCodes(p.User)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	row := core.WebAuthnCredential{UserID: p.User.ID, CredentialID: cred.ID, Name: enr.name, Data: data}
	id, err := s.d.Store.AddWebAuthnCredential(ctx, row, hashes)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "this passkey is already registered")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	row.ID, row.CreatedAt = id, s.now()
	writeJSON(w, http.StatusCreated, passkeyRegisterResp{Passkey: toPasskeyDTO(row), RecoveryCodes: codes})
}

func (s *server) passkeyDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req passwordReq
	if !decode(w, r, &req) {
		return
	}
	remaining := p.User.Passkeys - 1
	if p.User.TOTPEnabled {
		remaining++
	}
	if !s.lastFactorGuard(w, r, p.User, remaining) {
		return
	}
	if !s.confirmPassword(w, r, p.User, req.Password) {
		return
	}
	if err := s.d.Store.DeleteWebAuthnCredential(r.Context(), p.User.ID, id); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// regenerateRecoveryCodes replaces the whole set: the old codes stop working,
// which is also the answer to "I think someone saw my codes".
func (s *server) regenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mfaUser(w, r)
	if !ok {
		return
	}
	var req passwordReq
	if !decode(w, r, &req) {
		return
	}
	if !p.User.MFAEnabled() {
		writeError(w, http.StatusConflict, "enable a second factor first")
		return
	}
	if !s.confirmPassword(w, r, p.User, req.Password) {
		return
	}
	codes, hashes, err := auth.NewRecoveryCodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.d.Store.ReplaceRecoveryCodes(r.Context(), p.User.ID, hashes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recoveryCodesResp{RecoveryCodes: codes})
}

// --- admin --------------------------------------------------------------

// resetUserMFA is the admin's answer to "I lost my phone and my codes": every
// factor and code of the account goes, and so do its sessions. The user then
// signs in with the password alone (or is sent to enrolment, if the policy
// requires 2FA). An admin's own factors are managed from their own settings,
// where the password is asked for; this route would skip that check.
func (s *server) resetUserMFA(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p := principalFrom(r.Context())
	if p.UserID() == id {
		writeError(w, http.StatusConflict, "manage your own second factors from your security settings")
		return
	}
	ctx := r.Context()
	if err := s.d.Store.ResetUserMFA(ctx, id); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	s.mfa.totp.del(userKey(id))
	s.mfa.passkeys.del(userKey(id))
	s.d.Log.Info("mfa reset by admin", "user", id, "by", p.Label())
	u, err := s.d.Store.GetUser(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toUserDTO(*u))
}
