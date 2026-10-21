package auth

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTOTPRoundTrip(t *testing.T) {
	enr, err := NewTOTP("Duskrun", "admin@corp.io")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enr.QRPNG, "data:image/png;base64,") {
		t.Fatalf("QR is not a PNG data URL: %.40q", enr.QRPNG)
	}
	u, err := url.Parse(enr.URL)
	if err != nil || u.Scheme != "otpauth" || u.Query().Get("secret") != enr.Secret {
		t.Fatalf("otpauth URL %q does not carry the secret", enr.URL)
	}

	now := time.Unix(1_800_000_000, 0)
	code, err := TOTPCode(enr.Secret, now)
	if err != nil {
		t.Fatal(err)
	}
	step, ok := VerifyTOTP(enr.Secret, code, now, 0)
	if !ok || step != now.Unix()/30 {
		t.Fatalf("VerifyTOTP = (%d, %v), want (%d, true)", step, ok, now.Unix()/30)
	}

	// One step of clock drift either way is tolerated; two is not.
	if _, ok := VerifyTOTP(enr.Secret, code, now.Add(30*time.Second), 0); !ok {
		t.Fatal("a code from the previous step must still verify")
	}
	if _, ok := VerifyTOTP(enr.Secret, code, now.Add(90*time.Second), 0); ok {
		t.Fatal("a code two steps old must not verify")
	}
}

// TestTOTPRefusesUsedStep: a step already recorded must be refused even with
// the right code, or an observed code stays usable for its whole window.
func TestTOTPRefusesUsedStep(t *testing.T) {
	enr, err := NewTOTP("Duskrun", "a@b.c")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	code, _ := TOTPCode(enr.Secret, now)
	step, ok := VerifyTOTP(enr.Secret, code, now, 0)
	if !ok {
		t.Fatal("first use must verify")
	}
	if _, ok := VerifyTOTP(enr.Secret, code, now, step); ok {
		t.Fatal("replay of an accepted step must be refused")
	}
}

func TestTOTPRejectsMalformed(t *testing.T) {
	enr, _ := NewTOTP("Duskrun", "a@b.c")
	for _, c := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := VerifyTOTP(enr.Secret, c, time.Now(), 0); ok {
			t.Fatalf("code %q verified", c)
		}
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashes, err := NewRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != RecoveryCodeCount || len(hashes) != RecoveryCodeCount {
		t.Fatalf("got %d codes / %d hashes, want %d", len(codes), len(hashes), RecoveryCodeCount)
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if len(c) != 14 || c[4] != '-' || c[9] != '-' {
			t.Fatalf("code %q is not xxxx-xxxx-xxxx", c)
		}
		if seen[c] {
			t.Fatalf("duplicate code %q", c)
		}
		seen[c] = true
		if RecoveryCodeHash(c) != hashes[i] {
			t.Fatalf("hash of %q does not match the stored one", c)
		}
		// Typed without dashes, in capitals, with spaces: same code.
		typed := " " + strings.ToUpper(strings.ReplaceAll(c, "-", " ")) + " "
		if RecoveryCodeHash(typed) != hashes[i] {
			t.Fatalf("normalised %q does not match %q", typed, c)
		}
	}
	// Crockford folding: O reads as 0, I and L as 1.
	if RecoveryCodeHash("o1l1-i000-0000") != RecoveryCodeHash("0111-1000-0000") {
		t.Fatal("ambiguous letters are not folded into digits")
	}
}
