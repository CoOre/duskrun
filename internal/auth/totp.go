package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TOTP parameters. These are what every authenticator app assumes when the
// otpauth URI leaves them out, and several apps silently ignore anything else,
// so they are fixed rather than configurable.
const (
	totpPeriod = 30
	totpDigits = otp.DigitsSix
	// totpSkew is how many steps either side of now are accepted: a phone
	// clock a little off, or a code typed just as it rolled over, still works.
	totpSkew = 1
)

// TOTPEnrollment is a freshly generated, not yet confirmed TOTP secret.
type TOTPEnrollment struct {
	Secret string // base32, what the user types when they cannot scan
	URL    string // otpauth://totp/... — what the QR code encodes
	QRPNG  string // data: URL of the QR code, ready for an <img src>
}

// NewTOTP generates a 160-bit secret (RFC 4226's recommended length) labelled
// "issuer: account" in the authenticator app.
func NewTOTP(issuer, account string) (*TOTPEnrollment, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: account,
		Period:      totpPeriod,
		SecretSize:  20,
		Digits:      totpDigits,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: generate totp: %w", err)
	}
	img, err := key.Image(240, 240)
	if err != nil {
		return nil, fmt.Errorf("auth: render totp qr: %w", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("auth: encode totp qr: %w", err)
	}
	return &TOTPEnrollment{
		Secret: key.Secret(),
		URL:    key.URL(),
		QRPNG:  "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

// VerifyTOTP checks code against secret at now and returns the time step it
// matched. A step at or below afterStep is refused even when the code is right:
// the caller records the returned step, and that is what makes a code good for
// one login rather than for its whole 90-second window.
func VerifyTOTP(secret, code string, now time.Time, afterStep int64) (int64, bool) {
	code = strings.TrimSpace(strings.ReplaceAll(code, " ", ""))
	if len(code) != totpDigits.Length() {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		step := cur + d
		if step <= afterStep {
			continue
		}
		want, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriod, 0), totp.ValidateOpts{
			Period: totpPeriod, Digits: totpDigits, Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// TOTPCode returns the code for secret at t. Tests use it to play the
// authenticator app.
func TOTPCode(secret string, t time.Time) (string, error) {
	return totp.GenerateCodeCustom(secret, t, totp.ValidateOpts{
		Period: totpPeriod, Digits: totpDigits, Algorithm: otp.AlgorithmSHA1,
	})
}

// --- recovery codes ------------------------------------------------------

// RecoveryCodeCount is how many codes a set holds.
const RecoveryCodeCount = 10

// recoveryAlphabet is Crockford's base32: no I, L, O or U, so a code copied
// from paper cannot be misread as another valid code. 32 symbols keep the
// mapping from random bytes unbiased.
const recoveryAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// recoveryLen is 12 symbols, 60 bits: far beyond online guessing under the
// login throttle, and costly enough offline that a database copy alone does
// not yield usable codes in practice.
const recoveryLen = 12

// NewRecoveryCodes returns a fresh set of codes formatted for display
// ("xxxx-xxxx-xxxx") and their hashes for storage, index-aligned.
func NewRecoveryCodes() (codes, hashes []string, err error) {
	raw := make([]byte, recoveryLen)
	for range RecoveryCodeCount {
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, fmt.Errorf("auth: read recovery code: %w", err)
		}
		var b strings.Builder
		for i, c := range raw {
			if i > 0 && i%4 == 0 {
				b.WriteByte('-')
			}
			b.WriteByte(recoveryAlphabet[c&31])
		}
		code := b.String()
		codes = append(codes, code)
		hashes = append(hashes, RecoveryCodeHash(code))
	}
	return codes, hashes, nil
}

// RecoveryCodeHash normalises a code as typed — case, dashes and spaces, and
// the letters Crockford folds into digits — and hashes it. SHA-256 rather than
// argon2 for the same reason as TokenHash: the input is random, not chosen.
func RecoveryCodeHash(code string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(code) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'o':
			r = '0'
		case 'i', 'l':
			r = '1'
		}
		b.WriteRune(r)
	}
	return TokenHash(b.String())
}
