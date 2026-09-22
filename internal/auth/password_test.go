package auth

import (
	"strings"
	"testing"
)

// cheap keeps the table-driven tests off the 64 MiB default: the encoding and
// verification logic is what is under test, not the cost parameters.
var cheap = Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := HashWith("correct horse battery staple", cheap)
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}
	ok, err := Verify(h, "correct horse battery staple")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("correct password did not verify")
	}
	if ok, _ := Verify(h, "correct horse battery stapl"); ok {
		t.Fatal("wrong password verified")
	}
}

// TestHashIsSalted: the same password hashed twice must not produce the same
// string, or the table leaks which accounts share a password.
func TestHashIsSalted(t *testing.T) {
	a, _ := HashWith("same-password", cheap)
	b, _ := HashWith("same-password", cheap)
	if a == b {
		t.Fatal("two hashes of the same password are identical — salt is not applied")
	}
}

// TestVerifyUsesEncodedParams: a hash written with parameters other than the
// current defaults still verifies, which is what lets DefaultParams be raised
// without a data migration.
func TestVerifyUsesEncodedParams(t *testing.T) {
	other := Params{Memory: 16 * 1024, Time: 2, Threads: 1, SaltLen: 8, KeyLen: 16}
	h, err := HashWith("passphrase-here", other)
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}
	if !strings.Contains(h, "m=16384,t=2,p=1") {
		t.Fatalf("encoded params missing from %q", h)
	}
	ok, err := Verify(h, "passphrase-here")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("hash with non-default params failed to verify")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	good, _ := HashWith("passphrase-here", cheap)
	cases := map[string]string{
		"empty":         "",
		"not argon2":    "$2b$12$abcdefghijklmnopqrstuv",
		"missing field": "$argon2id$v=19$m=8192,t=1,p=1$c2FsdA",
		"bad version":   strings.Replace(good, "v=19", "v=18", 1),
		"bad hash b64":  good[:strings.LastIndex(good, "$")] + "$!!!!",
	}
	for name, enc := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := Verify(enc, "passphrase-here")
			if ok {
				t.Fatal("malformed hash reported a match")
			}
			if err == nil {
				t.Fatal("malformed hash returned no error")
			}
		})
	}
}

// TestVerifyDummyAlwaysFalse guards the no-such-user branch of login: it must
// do the work and still say no.
func TestVerifyDummyAlwaysFalse(t *testing.T) {
	if VerifyDummy("anything at all") {
		t.Fatal("VerifyDummy matched")
	}
}

func TestSessionTokenAndHash(t *testing.T) {
	a, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	b, _ := NewSessionToken()
	if a == b {
		t.Fatal("two session tokens are identical")
	}
	if len(a) < 40 {
		t.Fatalf("token %q is shorter than 32 bytes base64url", a)
	}
	if TokenHash(a) == a {
		t.Fatal("TokenHash returned the token itself")
	}
	if h1, h2 := TokenHash(a), TokenHash(a); h1 != h2 {
		t.Fatal("TokenHash is not deterministic")
	}
	if TokenHash(a) == TokenHash(b) {
		t.Fatal("distinct tokens hash the same")
	}
}
