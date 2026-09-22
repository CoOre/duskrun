package core

import (
	"context"
	"encoding/json"
	"testing"
)

func TestResolveConnectorConfigResolvesPrivateKeyRef(t *testing.T) {
	exec := NewExecutor(secretStore{
		secrets: map[string]*Secret{
			"secret://ssh/host": {Name: "secret://ssh/host"},
		},
	}, staticSecretOpener{value: []byte("PRIVATE KEY")}, nil)

	raw := json.RawMessage(`{"ssh_host":"h","private_key_ref":"secret://ssh/host","remote_host":"db","remote_port":5432}`)
	got, err := exec.res.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(got, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["private_key"] != "PRIVATE KEY" {
		t.Fatalf("private_key = %q, want resolved key", cfg["private_key"])
	}
	if cfg["private_key_ref"] != "secret://ssh/host" {
		t.Fatalf("private_key_ref = %q, want original ref retained", cfg["private_key_ref"])
	}
}

func TestResolveConnectorConfigKeepsInlinePrivateKey(t *testing.T) {
	exec := NewExecutor(secretStore{}, staticSecretOpener{value: []byte("SECRET KEY")}, nil)

	raw := json.RawMessage(`{"private_key":"INLINE KEY","private_key_ref":"secret://ssh/host"}`)
	got, err := exec.res.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("config = %s, want original inline config", got)
	}
}

type secretStore struct {
	secrets map[string]*Secret
}

func (s secretStore) GetConnection(context.Context, int64) (*Connection, error) { return nil, nil }
func (s secretStore) GetStorage(context.Context, int64) (*Storage, error)       { return nil, nil }
func (s secretStore) InsertArtifact(context.Context, Artifact) (int64, error)   { return 0, nil }
func (s secretStore) Finish(context.Context, int64, RunStatus, string, string) error {
	return nil
}
func (s secretStore) GetSecretByRef(_ context.Context, ref string) (*Secret, error) {
	return s.secrets[ref], nil
}

type staticSecretOpener struct {
	value []byte
}

func (o staticSecretOpener) Open(Secret) ([]byte, error) {
	return o.value, nil
}
