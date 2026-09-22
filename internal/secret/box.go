// Package secret implements envelope encryption for stored secrets (roadmap Q1).
//
// Each secret gets a fresh random 256-bit DEK. The value is sealed with the DEK
// (AES-256-GCM); the DEK itself is sealed with the master key (AES-256-GCM) and
// stored alongside. Master-key rotation therefore only needs to re-seal DEKs,
// not re-encrypt every value — the basis for rotation without downtime.
//
// Blob layout: [1 byte version=1][2 bytes big-endian len(wrappedDEK)][wrappedDEK][sealedValue]
// Each AES-GCM segment is [12-byte nonce][ciphertext||tag].
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/duskrun/duskrun/internal/core"
)

const blobVersion = 1

// Box seals and opens secrets under a master key. The master key material is
// normalised to 32 bytes via SHA-256, so any sufficiently-random env/file value
// works. KeyID identifies the master-key version that wrapped a secret's DEK.
type Box struct {
	master cipher.AEAD
	keyID  string
}

// NewBox derives the master AEAD from raw key material. keyID labels this
// master-key version (recorded on every secret for rotation bookkeeping).
func NewBox(masterKey []byte, keyID string) (*Box, error) {
	if len(masterKey) == 0 {
		return nil, fmt.Errorf("secret: empty master key")
	}
	if keyID == "" {
		keyID = "env:v1"
	}
	aead, err := newAEAD(deriveKey(masterKey))
	if err != nil {
		return nil, err
	}
	return &Box{master: aead, keyID: keyID}, nil
}

// KeyID returns the master-key version label.
func (b *Box) KeyID() string { return b.keyID }

// Seal encrypts plaintext and returns a Secret ready to persist.
func (b *Box) Seal(name, typ string, plaintext []byte) (core.Secret, error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return core.Secret{}, fmt.Errorf("secret: gen dek: %w", err)
	}
	dekAEAD, err := newAEAD(dek)
	if err != nil {
		return core.Secret{}, err
	}
	sealedValue, err := sealAEAD(dekAEAD, plaintext)
	if err != nil {
		return core.Secret{}, err
	}
	wrappedDEK, err := sealAEAD(b.master, dek)
	if err != nil {
		return core.Secret{}, err
	}
	if len(wrappedDEK) > 0xffff {
		return core.Secret{}, fmt.Errorf("secret: wrapped dek too large")
	}

	blob := make([]byte, 0, 3+len(wrappedDEK)+len(sealedValue))
	blob = append(blob, blobVersion)
	blob = binary.BigEndian.AppendUint16(blob, uint16(len(wrappedDEK)))
	blob = append(blob, wrappedDEK...)
	blob = append(blob, sealedValue...)

	return core.Secret{Name: name, Type: typ, Ciphertext: blob, KeyID: b.keyID}, nil
}

// Open decrypts a Secret's value.
func (b *Box) Open(s core.Secret) ([]byte, error) {
	blob := s.Ciphertext
	if len(blob) < 3 || blob[0] != blobVersion {
		return nil, fmt.Errorf("secret: bad blob header")
	}
	dl := int(binary.BigEndian.Uint16(blob[1:3]))
	if 3+dl > len(blob) {
		return nil, fmt.Errorf("secret: truncated blob")
	}
	wrappedDEK := blob[3 : 3+dl]
	sealedValue := blob[3+dl:]

	dek, err := openAEAD(b.master, wrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("secret: unwrap dek: %w", err)
	}
	dekAEAD, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	pt, err := openAEAD(dekAEAD, sealedValue)
	if err != nil {
		return nil, fmt.Errorf("secret: open value: %w", err)
	}
	return pt, nil
}

// --- primitives ---

func deriveKey(master []byte) []byte {
	h := sha256.Sum256(master)
	return h[:]
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: aes: %w", err)
	}
	return cipher.NewGCM(block)
}

func sealAEAD(a cipher.AEAD, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, a.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secret: nonce: %w", err)
	}
	return a.Seal(nonce, nonce, plaintext, nil), nil
}

func openAEAD(a cipher.AEAD, ct []byte) ([]byte, error) {
	ns := a.NonceSize()
	if len(ct) < ns {
		return nil, fmt.Errorf("secret: ciphertext too short")
	}
	return a.Open(nil, ct[:ns], ct[ns:], nil)
}

// Fingerprint is a short, non-secret label for a master key (for UI/logs).
func Fingerprint(masterKey []byte) string {
	h := sha256.Sum256(masterKey)
	return hex.EncodeToString(h[:4])
}
