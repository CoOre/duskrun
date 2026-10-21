package sqlite

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CoOre/duskrun/internal/core"
)

// TestUserCarriesFactorState: the derived columns must reflect the factor
// tables on every read path, including the session join the auth middleware
// uses — that is where the enrolment gate reads them.
func TestUserCarriesFactorState(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := mkUser(t, st, "a@corp.io", core.RoleAdmin)

	u, err := st.GetUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if u.MFAEnabled() || len(u.WebAuthnID) != 32 {
		t.Fatalf("fresh user: mfa=%v handle=%d bytes, want false/32", u.MFAEnabled(), len(u.WebAuthnID))
	}
	other := mkUser(t, st, "b@corp.io", core.RoleViewer)
	if o, _ := st.GetUser(ctx, other); bytes.Equal(o.WebAuthnID, u.WebAuthnID) {
		t.Fatal("two users share a WebAuthn handle")
	}

	if err := st.PutTOTP(ctx, core.TOTP{UserID: id, Secret: []byte("sealed"), KeyID: "k"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddWebAuthnCredential(ctx, core.WebAuthnCredential{UserID: id, CredentialID: []byte{1}, Data: []byte{2}}, nil); err != nil {
		t.Fatal(err)
	}
	mkSession(t, st, id, "h1", time.Now().Add(time.Hour))
	_, su, err := st.SessionByToken(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if !su.TOTPEnabled || su.Passkeys != 1 || !bytes.Equal(su.WebAuthnID, u.WebAuthnID) {
		t.Fatalf("session user = totp %v, passkeys %d; want true, 1 and the same handle", su.TOTPEnabled, su.Passkeys)
	}
}

func TestTOTPStepIsSingleUse(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := mkUser(t, st, "a@corp.io", core.RoleAdmin)
	if err := st.PutTOTP(ctx, core.TOTP{UserID: id, Secret: []byte("s"), KeyID: "k", LastStep: 100}, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		step int64
		want bool
	}{{100, false}, {99, false}, {101, true}, {101, false}, {103, true}} {
		ok, err := st.AdvanceTOTPStep(ctx, id, tc.step)
		if err != nil {
			t.Fatal(err)
		}
		if ok != tc.want {
			t.Fatalf("AdvanceTOTPStep(%d) = %v, want %v", tc.step, ok, tc.want)
		}
	}
}

func TestRecoveryCodesLifecycle(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := mkUser(t, st, "a@corp.io", core.RoleAdmin)

	if err := st.PutTOTP(ctx, core.TOTP{UserID: id, Secret: []byte("s"), KeyID: "k"}, []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if ok, _ := st.UseRecoveryCode(ctx, id, "h1", now); !ok {
		t.Fatal("first use of h1 refused")
	}
	if ok, _ := st.UseRecoveryCode(ctx, id, "h1", now); ok {
		t.Fatal("second use of h1 accepted")
	}
	other := mkUser(t, st, "b@corp.io", core.RoleViewer)
	if ok, _ := st.UseRecoveryCode(ctx, other, "h2", now); ok {
		t.Fatal("another user's code accepted")
	}
	if n, _ := st.CountRecoveryCodes(ctx, id); n != 1 {
		t.Fatalf("codes left = %d, want 1", n)
	}

	// Removing the only factor drops the codes with it.
	if err := st.DeleteTOTP(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountRecoveryCodes(ctx, id); n != 0 {
		t.Fatalf("codes left after last factor removed = %d, want 0", n)
	}
}

// TestDeletePasskeyKeepsCodesWhileTOTPRemains: codes go with the last factor,
// not with any factor.
func TestDeletePasskeyKeepsCodesWhileTOTPRemains(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := mkUser(t, st, "a@corp.io", core.RoleAdmin)
	if err := st.PutTOTP(ctx, core.TOTP{UserID: id, Secret: []byte("s"), KeyID: "k"}, []string{"h1"}); err != nil {
		t.Fatal(err)
	}
	pk, err := st.AddWebAuthnCredential(ctx, core.WebAuthnCredential{UserID: id, CredentialID: []byte{1}, Data: []byte{2}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	other := mkUser(t, st, "b@corp.io", core.RoleViewer)
	if err := st.DeleteWebAuthnCredential(ctx, other, pk); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting another user's passkey: err = %v, want ErrNotFound", err)
	}
	if err := st.DeleteWebAuthnCredential(ctx, id, pk); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountRecoveryCodes(ctx, id); n != 1 {
		t.Fatalf("codes left = %d, want 1 while TOTP remains", n)
	}
}

func TestResetUserMFA(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := mkUser(t, st, "a@corp.io", core.RoleAdmin)
	if err := st.PutTOTP(ctx, core.TOTP{UserID: id, Secret: []byte("s"), KeyID: "k"}, []string{"h1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddWebAuthnCredential(ctx, core.WebAuthnCredential{UserID: id, CredentialID: []byte{1}, Data: []byte{2}}, nil); err != nil {
		t.Fatal(err)
	}
	mkSession(t, st, id, "h", time.Now().Add(time.Hour))

	if err := st.ResetUserMFA(ctx, id); err != nil {
		t.Fatal(err)
	}
	u, _ := st.GetUser(ctx, id)
	if u.MFAEnabled() {
		t.Fatal("factors survived the reset")
	}
	if n, _ := st.CountRecoveryCodes(ctx, id); n != 0 {
		t.Fatalf("codes left = %d, want 0", n)
	}
	if _, _, err := st.SessionByToken(ctx, "h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session survived the reset: err = %v", err)
	}
	if err := st.ResetUserMFA(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset of a missing user: err = %v, want ErrNotFound", err)
	}
}
