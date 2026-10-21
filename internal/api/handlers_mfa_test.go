package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"

	"github.com/CoOre/duskrun/internal/auth"
	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/secret"
	"github.com/CoOre/duskrun/internal/store/sqlite"
)

// testClock is a settable clock. TOTP codes are single-use per 30-second step,
// so the tests move time on explicitly instead of sleeping.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const testPublicURL = "http://localhost"

func mfaServer(t *testing.T) (*httptest.Server, *sqlite.Store, *testClock) {
	t.Helper()
	box, err := secret.NewBox([]byte("test-master-key"), "env:v1")
	if err != nil {
		t.Fatal(err)
	}
	clk := &testClock{t: time.Unix(1_800_000_000, 0)}
	srv, st := authServerWith(t, Deps{Secrets: box, Now: clk.Now, PublicURL: testPublicURL})
	return srv, st, clk
}

func decodeBody[T any](t *testing.T, resp *http.Response, want int) T {
	t.Helper()
	defer resp.Body.Close()
	var out T
	if resp.StatusCode != want {
		var e map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&e)
		t.Fatalf("%s %s: status %d (%s), want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, e["error"], want)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func status(t *testing.T, resp *http.Response) int {
	t.Helper()
	resp.Body.Close()
	return resp.StatusCode
}

// enrollTOTP turns TOTP on for the session's user and returns the secret and
// the recovery codes handed out with it.
func enrollTOTP(t *testing.T, srv *httptest.Server, token string, clk *testClock) (string, []string) {
	t.Helper()
	setup := decodeBody[totpSetupResp](t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/totp/setup", token,
		map[string]string{"password": testPassword}), http.StatusOK)
	code, _ := auth.TOTPCode(setup.Secret, clk.Now())
	out := decodeBody[recoveryCodesResp](t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/totp/confirm", token,
		map[string]string{"code": code}), http.StatusOK)
	// The confirming code's step is now spent; the next login needs the next one.
	clk.Add(30 * time.Second)
	return setup.Secret, out.RecoveryCodes
}

func startLogin(t *testing.T, srv *httptest.Server, email string) mfaChallengeResp {
	t.Helper()
	return decodeBody[mfaChallengeResp](t, doJSON(t, srv, http.MethodPost, "/api/login", "",
		map[string]string{"email": email, "password": testPassword}), http.StatusOK)
}

func TestTOTPEnrolAndLogin(t *testing.T) {
	srv, _, clk := mfaServer(t)
	tok := loginAs(t, srv, core.RoleAdmin)

	secret, codes := enrollTOTP(t, srv, tok, clk)
	if len(codes) != auth.RecoveryCodeCount {
		t.Fatalf("got %d recovery codes with the first factor, want %d", len(codes), auth.RecoveryCodeCount)
	}
	me := decodeBody[meDTO](t, doJSON(t, srv, http.MethodGet, "/api/me", tok, nil), http.StatusOK)
	if !me.MFAEnabled {
		t.Fatal("/me does not report mfa_enabled after enrolment")
	}

	// The password alone no longer yields a session.
	ch := startLogin(t, srv, "admin@corp.io")
	if !ch.MFARequired || ch.MFAToken == "" || !strings.Contains(strings.Join(ch.MFAMethods, ","), "totp") {
		t.Fatalf("login answer = %+v, want an MFA challenge offering totp", ch)
	}

	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]string{"mfa_token": ch.MFAToken, "method": "totp", "code": "000000"})); got != http.StatusUnauthorized {
		t.Fatalf("wrong code: status %d, want 401", got)
	}

	code, _ := auth.TOTPCode(secret, clk.Now())
	resp := decodeBody[loginResp](t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]string{"mfa_token": ch.MFAToken, "method": "totp", "code": code}), http.StatusOK)
	if resp.Token == "" {
		t.Fatal("completed login returned no token")
	}

	// The same code cannot complete a second login, and the used challenge is gone.
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]string{"mfa_token": ch.MFAToken, "method": "totp", "code": code})); got != http.StatusGone {
		t.Fatalf("reused challenge: status %d, want 410", got)
	}
	ch2 := startLogin(t, srv, "admin@corp.io")
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]string{"mfa_token": ch2.MFAToken, "method": "totp", "code": code})); got != http.StatusUnauthorized {
		t.Fatalf("replayed code: status %d, want 401", got)
	}
}

func TestRecoveryCodeLogin(t *testing.T) {
	srv, _, clk := mfaServer(t)
	_, codes := enrollTOTP(t, srv, loginAs(t, srv, core.RoleAdmin), clk)

	ch := startLogin(t, srv, "admin@corp.io")
	// Typed sloppily: capitals, no dashes.
	typed := strings.ToUpper(strings.ReplaceAll(codes[0], "-", ""))
	resp := decodeBody[loginResp](t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]string{"mfa_token": ch.MFAToken, "method": "recovery", "code": typed}), http.StatusOK)
	if resp.RecoveryCodesLeft == nil || *resp.RecoveryCodesLeft != auth.RecoveryCodeCount-1 {
		t.Fatalf("recovery_codes_left = %v, want %d", resp.RecoveryCodesLeft, auth.RecoveryCodeCount-1)
	}

	ch = startLogin(t, srv, "admin@corp.io")
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]string{"mfa_token": ch.MFAToken, "method": "recovery", "code": codes[0]})); got != http.StatusUnauthorized {
		t.Fatalf("spent recovery code: status %d, want 401", got)
	}
}

// TestChallengeExhausts: a challenge allows a handful of wrong answers, then
// the password has to be entered again.
func TestChallengeExhausts(t *testing.T) {
	srv, _, clk := mfaServer(t)
	enrollTOTP(t, srv, loginAs(t, srv, core.RoleAdmin), clk)
	ch := startLogin(t, srv, "admin@corp.io")
	for i := range mfaChallengeFail {
		if got := status(t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
			map[string]string{"mfa_token": ch.MFAToken, "method": "totp", "code": "000000"})); got != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, got)
		}
	}
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]string{"mfa_token": ch.MFAToken, "method": "totp", "code": "000000"})); got != http.StatusGone {
		t.Fatalf("after %d failures: status %d, want 410", mfaChallengeFail, got)
	}
}

func TestChallengeExpires(t *testing.T) {
	srv, _, clk := mfaServer(t)
	secret, _ := enrollTOTP(t, srv, loginAs(t, srv, core.RoleAdmin), clk)
	ch := startLogin(t, srv, "admin@corp.io")
	clk.Add(mfaChallengeTTL + time.Second)
	code, _ := auth.TOTPCode(secret, clk.Now())
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]string{"mfa_token": ch.MFAToken, "method": "totp", "code": code})); got != http.StatusGone {
		t.Fatalf("expired challenge: status %d, want 410", got)
	}
}

// TestFactorChangesNeedPassword: a stolen session token alone must not add or
// remove factors. A wrong password is 403 — a 401 would sign the SPA out.
func TestFactorChangesNeedPassword(t *testing.T) {
	srv, _, clk := mfaServer(t)
	tok := loginAs(t, srv, core.RoleAdmin)
	wrong := map[string]string{"password": "not the password"}

	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/totp/setup", tok, wrong)); got != http.StatusForbidden {
		t.Fatalf("setup with wrong password: %d, want 403", got)
	}
	enrollTOTP(t, srv, tok, clk)
	for _, path := range []string{"/api/me/mfa/totp/disable", "/api/me/mfa/recovery-codes"} {
		if got := status(t, doJSON(t, srv, http.MethodPost, path, tok, wrong)); got != http.StatusForbidden {
			t.Fatalf("%s with wrong password: %d, want 403", path, got)
		}
	}
	// The session survived all of that.
	if got := status(t, doJSON(t, srv, http.MethodGet, "/api/me", tok, nil)); got != http.StatusOK {
		t.Fatalf("session after wrong passwords: %d, want 200", got)
	}

	codes := decodeBody[recoveryCodesResp](t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/recovery-codes", tok,
		map[string]string{"password": testPassword}), http.StatusOK)
	if len(codes.RecoveryCodes) != auth.RecoveryCodeCount {
		t.Fatalf("regenerated %d codes, want %d", len(codes.RecoveryCodes), auth.RecoveryCodeCount)
	}
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/totp/disable", tok,
		map[string]string{"password": testPassword})); got != http.StatusOK {
		t.Fatalf("disable: %d, want 200", got)
	}
	st := decodeBody[mfaStatusDTO](t, doJSON(t, srv, http.MethodGet, "/api/me/mfa", tok, nil), http.StatusOK)
	if st.TOTP || st.RecoveryCodesLeft != 0 {
		t.Fatalf("after disabling the only factor: %+v, want no TOTP and no codes", st)
	}
}

func TestTOTPConfirmWrongCodeIs400(t *testing.T) {
	srv, _, _ := mfaServer(t)
	tok := loginAs(t, srv, core.RoleAdmin)
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/totp/confirm", tok,
		map[string]string{"code": "123456"})); got != http.StatusConflict {
		t.Fatalf("confirm without setup: %d, want 409", got)
	}
	decodeBody[totpSetupResp](t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/totp/setup", tok,
		map[string]string{"password": testPassword}), http.StatusOK)
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/totp/confirm", tok,
		map[string]string{"code": "000000"})); got != http.StatusBadRequest {
		t.Fatalf("confirm with wrong code: %d, want 400", got)
	}
}

func TestMFAPolicyEnforcement(t *testing.T) {
	srv, _, clk := mfaServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)
	op := loginAs(t, srv, core.RoleOperator)

	// An admin without a factor cannot require one.
	if got := status(t, doJSON(t, srv, http.MethodPatch, "/api/settings", admin,
		map[string]string{"mfa_required": "admins"})); got != http.StatusConflict {
		t.Fatalf("requiring 2FA without own factor: %d, want 409", got)
	}
	enrollTOTP(t, srv, admin, clk)
	if got := status(t, doJSON(t, srv, http.MethodPatch, "/api/settings", admin,
		map[string]string{"mfa_required": "bogus"})); got != http.StatusBadRequest {
		t.Fatalf("unknown policy: %d, want 400", got)
	}
	set := decodeBody[settingsDTO](t, doJSON(t, srv, http.MethodPatch, "/api/settings", admin,
		map[string]string{"mfa_required": "all"}), http.StatusOK)
	if set.MFARequired != "all" {
		t.Fatalf("mfa_required = %q, want all", set.MFARequired)
	}

	// The operator's existing session is held at enrolment immediately.
	if got := status(t, doJSON(t, srv, http.MethodGet, "/api/tasks", op, nil)); got != http.StatusForbidden {
		t.Fatalf("operator without 2FA under policy: %d, want 403", got)
	}
	me := decodeBody[meDTO](t, doJSON(t, srv, http.MethodGet, "/api/me", op, nil), http.StatusOK)
	if !me.MFAEnrollRequired {
		t.Fatal("/me does not report mfa_enroll_required")
	}
	// The static token is a machine account and is not subject to the policy.
	if got := status(t, doJSON(t, srv, http.MethodGet, "/api/tasks", testToken, nil)); got != http.StatusOK {
		t.Fatalf("static token under policy: %d, want 200", got)
	}

	enrollTOTP(t, srv, op, clk)
	if got := status(t, doJSON(t, srv, http.MethodGet, "/api/tasks", op, nil)); got != http.StatusOK {
		t.Fatalf("operator after enrolling: %d, want 200", got)
	}
	// And the only factor cannot be removed while the policy stands.
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/totp/disable", op,
		map[string]string{"password": testPassword})); got != http.StatusConflict {
		t.Fatalf("removing the last factor under policy: %d, want 409", got)
	}
}

func TestAdminResetMFA(t *testing.T) {
	srv, _, clk := mfaServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)
	op := loginAs(t, srv, core.RoleOperator)
	enrollTOTP(t, srv, op, clk)
	enrollTOTP(t, srv, admin, clk)

	users := decodeBody[[]userDTO](t, doJSON(t, srv, http.MethodGet, "/api/users", admin, nil), http.StatusOK)
	var opID, adminID int64
	for _, u := range users {
		switch u.Email {
		case "operator@corp.io":
			opID = u.ID
			if !u.MFATOTP {
				t.Fatal("user list does not show the operator's TOTP")
			}
		case "admin@corp.io":
			adminID = u.ID
		}
	}

	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/users/"+itoa(adminID)+"/mfa/reset", admin, nil)); got != http.StatusConflict {
		t.Fatalf("self reset: %d, want 409", got)
	}
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/users/"+itoa(opID)+"/mfa/reset", op, nil)); got != http.StatusForbidden {
		t.Fatalf("reset by a non-admin: %d, want 403", got)
	}
	u := decodeBody[userDTO](t, doJSON(t, srv, http.MethodPost, "/api/users/"+itoa(opID)+"/mfa/reset", admin, nil), http.StatusOK)
	if u.MFATOTP || u.MFAPasskeys != 0 {
		t.Fatalf("after reset: %+v, want no factors", u)
	}
	// Every session of the reset account ended.
	if got := status(t, doJSON(t, srv, http.MethodGet, "/api/me", op, nil)); got != http.StatusUnauthorized {
		t.Fatalf("operator session after reset: %d, want 401", got)
	}
	// And the password alone signs in again.
	loginAs(t, srv, core.RoleOperator)
}

func TestPasskeysUnavailableWithoutPublicURL(t *testing.T) {
	srv, _ := authServer(t)
	tok := loginAs(t, srv, core.RoleAdmin)
	st := decodeBody[mfaStatusDTO](t, doJSON(t, srv, http.MethodGet, "/api/me/mfa", tok, nil), http.StatusOK)
	if st.WebAuthnAvailable {
		t.Fatal("webauthn_available without DUSKRUN_PUBLIC_URL")
	}
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/passkeys/options", tok,
		map[string]string{"password": testPassword})); got != http.StatusConflict {
		t.Fatalf("passkey options without public URL: %d, want 409", got)
	}
}

// --- passkeys, end to end with a software authenticator -----------------

// softKey plays a FIDO2 authenticator: an ECDSA P-256 credential producing
// "none" attestation, which is what real platform passkeys send by default.
type softKey struct {
	t       *testing.T
	priv    *ecdsa.PrivateKey
	id      []byte
	counter uint32
}

func newSoftKey(t *testing.T) *softKey {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &softKey{t: t, priv: priv, id: id}
}

var b64u = base64.RawURLEncoding

func (k *softKey) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]string{"type": typ, "challenge": challenge, "origin": testPublicURL})
	return b
}

func (k *softKey) authData(flags byte, attested []byte) []byte {
	rp := sha256.Sum256([]byte("localhost"))
	k.counter++
	out := append(rp[:], flags)
	out = binary.BigEndian.AppendUint32(out, k.counter)
	return append(out, attested...)
}

// create answers navigator.credentials.create() for the given options.
func (k *softKey) create(opts map[string]any) map[string]any {
	pk := opts["publicKey"].(map[string]any)
	cose, err := webauthncbor.Marshal(map[int]any{
		1: 2, 3: -7, -1: 1,
		-2: k.priv.X.FillBytes(make([]byte, 32)),
		-3: k.priv.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		k.t.Fatal(err)
	}
	attested := make([]byte, 16) // AAGUID: zero, as with "none" attestation
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(k.id)))
	attested = append(attested, k.id...)
	attested = append(attested, cose...)
	att, err := webauthncbor.Marshal(map[string]any{
		"fmt": "none", "attStmt": map[string]any{}, "authData": k.authData(0x45, attested), // UP|UV|AT
	})
	if err != nil {
		k.t.Fatal(err)
	}
	return map[string]any{
		"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    b64u.EncodeToString(k.clientData("webauthn.create", pk["challenge"].(string))),
			"attestationObject": b64u.EncodeToString(att),
		},
	}
}

// get answers navigator.credentials.get() for the given options.
func (k *softKey) get(opts map[string]any) map[string]any {
	pk := opts["publicKey"].(map[string]any)
	cd := k.clientData("webauthn.get", pk["challenge"].(string))
	ad := k.authData(0x05, nil) // UP|UV
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	if err != nil {
		k.t.Fatal(err)
	}
	return map[string]any{
		"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    b64u.EncodeToString(cd),
			"authenticatorData": b64u.EncodeToString(ad),
			"signature":         b64u.EncodeToString(sig),
		},
	}
}

func registerPasskey(t *testing.T, srv *httptest.Server, token string, key *softKey) passkeyRegisterResp {
	t.Helper()
	opts := decodeBody[map[string]any](t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/passkeys/options", token,
		map[string]string{"password": testPassword, "name": "YubiKey"}), http.StatusOK)
	return decodeBody[passkeyRegisterResp](t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/passkeys", token,
		map[string]any{"credential": key.create(opts)}), http.StatusCreated)
}

func TestPasskeyEnrolAndLogin(t *testing.T) {
	srv, st, _ := mfaServer(t)
	tok := loginAs(t, srv, core.RoleAdmin)
	key := newSoftKey(t)

	reg := registerPasskey(t, srv, tok, key)
	if reg.Passkey.Name != "YubiKey" || len(reg.RecoveryCodes) != auth.RecoveryCodeCount {
		t.Fatalf("register = %+v, want the name back and a recovery set", reg)
	}
	// The same authenticator twice is refused by the exclusion list in a real
	// browser; the server must refuse it too.
	opts := decodeBody[map[string]any](t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/passkeys/options", tok,
		map[string]string{"password": testPassword}), http.StatusOK)
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/me/mfa/passkeys", tok,
		map[string]any{"credential": key.create(opts)})); got != http.StatusConflict {
		t.Fatalf("duplicate passkey: %d, want 409", got)
	}

	ch := startLogin(t, srv, "admin@corp.io")
	if strings.Join(ch.MFAMethods, ",") != "webauthn,recovery" {
		t.Fatalf("methods = %v, want webauthn and recovery", ch.MFAMethods)
	}
	// Without first fetching options there is no challenge to answer.
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]any{"mfa_token": ch.MFAToken, "method": "webauthn", "credential": map[string]any{}})); got != http.StatusUnauthorized {
		t.Fatalf("assertion without options: %d, want 401", got)
	}
	assertOpts := decodeBody[map[string]any](t, doJSON(t, srv, http.MethodPost, "/api/login/mfa/webauthn/options", "",
		map[string]string{"mfa_token": ch.MFAToken}), http.StatusOK)
	resp := decodeBody[loginResp](t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]any{"mfa_token": ch.MFAToken, "method": "webauthn", "credential": key.get(assertOpts)}), http.StatusOK)
	if resp.Token == "" {
		t.Fatal("passkey login returned no token")
	}

	rows, err := st.ListWebAuthnCredentials(context.Background(), resp.User.ID)
	if err != nil || len(rows) != 1 || rows[0].LastUsedAt == nil {
		t.Fatalf("credential after login = %+v (err %v), want last_used_at stamped", rows, err)
	}

	// A different key cannot answer for this account.
	ch = startLogin(t, srv, "admin@corp.io")
	assertOpts = decodeBody[map[string]any](t, doJSON(t, srv, http.MethodPost, "/api/login/mfa/webauthn/options", "",
		map[string]string{"mfa_token": ch.MFAToken}), http.StatusOK)
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/login/mfa", "",
		map[string]any{"mfa_token": ch.MFAToken, "method": "webauthn", "credential": newSoftKey(t).get(assertOpts)})); got != http.StatusUnauthorized {
		t.Fatalf("foreign passkey: %d, want 401", got)
	}

	// Deleting needs the password and removes the factor.
	path := "/api/me/mfa/passkeys/" + itoa(reg.Passkey.ID)
	if got := status(t, doJSON(t, srv, http.MethodDelete, path, tok, map[string]string{"password": "nope"})); got != http.StatusForbidden {
		t.Fatalf("delete with wrong password: %d, want 403", got)
	}
	if got := status(t, doJSON(t, srv, http.MethodDelete, path, tok, map[string]string{"password": testPassword})); got != http.StatusNoContent {
		t.Fatalf("delete: %d, want 204", got)
	}
	if got := status(t, doJSON(t, srv, http.MethodPost, "/api/login", "",
		map[string]string{"email": "admin@corp.io", "password": testPassword})); got != http.StatusOK {
		t.Fatalf("login after removing the passkey: %d", got)
	}
}
