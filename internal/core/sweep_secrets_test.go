package core

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

// sweepCapture records the config the storage plugin was built with during a
// sweep. The sweep opens storages on its own path, so it needs its own guard.
var sweepCapture []byte

func init() {
	plugin.Storages.Register("sweepcapturefs", func(cfg []byte) (plugin.Storage, error) {
		sweepCapture = append([]byte(nil), cfg...)
		return nullStorage{}, nil
	})
}

type nullStorage struct{}

func (nullStorage) Write(context.Context, string, io.Reader) (plugin.ObjectMeta, error) {
	return plugin.ObjectMeta{}, nil
}
func (nullStorage) Read(context.Context, string) (io.ReadCloser, error)   { return nil, nil }
func (nullStorage) List(context.Context, string) ([]plugin.Object, error) { return nil, nil }
func (nullStorage) Delete(context.Context, string) error                  { return nil }

func sweepSecretStore() *fakeSweepStore {
	return &fakeSweepStore{
		storages: map[int64]*Storage{7: {
			ID: 7, Name: "remote", Type: "sweepcapturefs",
			Config: json.RawMessage(`{"path":"/backups","private_key_ref":"secret://sftp/key"}`),
		}},
	}
}

// TestSweeperResolvesStorageSecrets covers the half of the gap a run does not:
// retention opens the same storage later, from its own code path, and would
// otherwise fail on every destination whose key lives in the secret store.
func TestSweeperResolvesStorageSecrets(t *testing.T) {
	sweepCapture = nil
	res := NewSecretResolver(
		refStore{secrets: map[string]*Secret{"secret://sftp/key": {Name: "sftp/key"}}},
		staticSecretOpener{value: []byte("PRIVATE KEY")},
	)
	sw := NewSweeper(sweepSecretStore(), SweeperConfig{
		Now:     func() time.Time { return sweepNow },
		Secrets: res,
	}, nil)

	if _, err := sw.storageOpener()(context.Background(), 7); err != nil {
		t.Fatalf("open storage: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(sweepCapture, &cfg); err != nil {
		t.Fatalf("captured config %s: %v", sweepCapture, err)
	}
	if cfg["private_key"] != "PRIVATE KEY" {
		t.Fatalf("private_key = %v, want the resolved secret", cfg["private_key"])
	}
}

// TestSweeperWithoutResolverFailsLoudly: a sweeper wired without a resolver must
// refuse the storage rather than build it with an empty key — a silent fallback
// here would delete nothing and report success.
func TestSweeperWithoutResolverFailsLoudly(t *testing.T) {
	sweepCapture = nil
	sw := NewSweeper(sweepSecretStore(), SweeperConfig{
		Now: func() time.Time { return sweepNow },
	}, nil)

	_, err := sw.storageOpener()(context.Background(), 7)
	if err == nil {
		t.Fatal("open storage = nil error, want a failure without a secret opener")
	}
	if !strings.Contains(err.Error(), "private_key_ref") {
		t.Fatalf("err = %v, want the unresolved key named", err)
	}
	if sweepCapture != nil {
		t.Fatalf("storage was built with %s, want no plugin at all", sweepCapture)
	}
}
