package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// refStore is the minimal SecretRefStore: a ref map plus an optional lookup
// failure, which is how a store outage reaches the resolver.
type refStore struct {
	secrets map[string]*Secret
	err     error
}

func (s refStore) GetSecretByRef(_ context.Context, ref string) (*Secret, error) {
	if s.err != nil {
		return nil, s.err
	}
	sec, ok := s.secrets[ref]
	if !ok {
		return nil, errors.New("not found")
	}
	return sec, nil
}

func newTestResolver(refs ...string) *SecretResolver {
	m := map[string]*Secret{}
	for _, r := range refs {
		m[r] = &Secret{Name: r}
	}
	return NewSecretResolver(refStore{secrets: m}, staticSecretOpener{value: []byte("PLAIN")})
}

func TestResolveNestedRef(t *testing.T) {
	// mssql's fetch block groups the SSH credentials one level down; a resolver
	// that only walked the top level would hand the dumper an empty key.
	res := newTestResolver("secret://ssh/db")
	raw := json.RawMessage(`{"database":"app","fetch":{"host":"db","private_key_ref":"secret://ssh/db"}}`)

	got, err := res.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var cfg struct {
		Fetch struct {
			PrivateKey    string `json:"private_key"`
			PrivateKeyRef string `json:"private_key_ref"`
		} `json:"fetch"`
	}
	if err := json.Unmarshal(got, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Fetch.PrivateKey != "PLAIN" {
		t.Fatalf("fetch.private_key = %q, want resolved value", cfg.Fetch.PrivateKey)
	}
	if cfg.Fetch.PrivateKeyRef != "secret://ssh/db" {
		t.Fatalf("fetch.private_key_ref = %q, want the ref retained", cfg.Fetch.PrivateKeyRef)
	}
}

func TestResolveInsideArray(t *testing.T) {
	res := newTestResolver("secret://a")
	raw := json.RawMessage(`{"targets":[{"password_ref":"secret://a"}]}`)

	got, err := res.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(string(got), `"password":"PLAIN"`) {
		t.Fatalf("config = %s, want the array element resolved", got)
	}
}

func TestResolveStorageKeys(t *testing.T) {
	// The s3 config has always declared access_key_ref/secret_key_ref, but
	// nothing resolved them: the plugin silently fell back to the AWS default
	// chain. This is the regression guard for that.
	res := newTestResolver("secret://s3/access", "secret://s3/secret")
	raw := json.RawMessage(`{"bucket":"b","access_key_ref":"secret://s3/access","secret_key_ref":"secret://s3/secret"}`)

	got, err := res.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(got, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["access_key"] != "PLAIN" || cfg["secret_key"] != "PLAIN" {
		t.Fatalf("config = %s, want both keys resolved", got)
	}
}

func TestResolveKeepsBytesWhenNothingToDo(t *testing.T) {
	res := newTestResolver()
	raw := json.RawMessage(`{"bucket":"b","region":"eu"}`)

	got, err := res.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("config = %s, want the original bytes untouched", got)
	}
}

func TestResolveEmptyAndBadConfig(t *testing.T) {
	res := newTestResolver()
	got, err := res.Resolve(context.Background(), nil)
	if err != nil || got != nil {
		t.Fatalf("Resolve(nil) = %s, %v; want nil, nil", got, err)
	}
	if _, err := res.Resolve(context.Background(), json.RawMessage(`{`)); err == nil {
		t.Fatal("Resolve(broken json) = nil error, want a decode failure")
	}
}

func TestResolveMissingSecretFails(t *testing.T) {
	// The dangerous outcome is not an error but a substitution of "": an sftp
	// storage would then try an anonymous login and the operator would chase a
	// permission error instead of a missing secret.
	res := newTestResolver()
	raw := json.RawMessage(`{"fetch":{"private_key_ref":"secret://gone"}}`)

	got, err := res.Resolve(context.Background(), raw)
	if err == nil {
		t.Fatalf("Resolve = %s, want an error for the missing secret", got)
	}
	if !strings.Contains(err.Error(), "fetch.private_key_ref") {
		t.Fatalf("err = %v, want the failing key named", err)
	}
	if !strings.Contains(err.Error(), "secret://gone") {
		t.Fatalf("err = %v, want the ref named", err)
	}
}

func TestResolveWithoutOpenerFails(t *testing.T) {
	res := NewSecretResolver(refStore{}, nil)
	if _, err := res.Resolve(context.Background(), json.RawMessage(`{"password_ref":"secret://a"}`)); err == nil {
		t.Fatal("Resolve without an opener = nil error, want a failure")
	}
	// A config that needs nothing still works without an opener — that is what
	// keeps `duskrun` usable with no master key for storages with inline config.
	raw := json.RawMessage(`{"path":"/backups"}`)
	got, err := res.Resolve(context.Background(), raw)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("Resolve = %s, %v; want the config through untouched", got, err)
	}
}

func TestNilResolverIsUsable(t *testing.T) {
	var res *SecretResolver
	raw := json.RawMessage(`{"path":"/backups"}`)
	got, err := res.Resolve(context.Background(), raw)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("Resolve = %s, %v; want the config through untouched", got, err)
	}
	if _, err := res.Resolve(context.Background(), json.RawMessage(`{"password_ref":"secret://a"}`)); err == nil {
		t.Fatal("nil resolver resolved a ref, want a failure")
	}
}

func TestResolveIgnoresBareRefSuffix(t *testing.T) {
	// A key that is exactly "_ref" has no base to write to, and a ref with an
	// inline value already set must not cost a store lookup.
	res := NewSecretResolver(refStore{err: errors.New("store down")}, staticSecretOpener{})
	raw := json.RawMessage(`{"_ref":"secret://a","password":"inline","password_ref":"secret://b"}`)

	got, err := res.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("config = %s, want the original bytes untouched", got)
	}
}
