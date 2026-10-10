package api

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/plugin"
)

// countingStorage reports a fixed capacity and counts how often it was asked.
type countingStorage struct{ plugin.Storage }

var capacityCalls atomic.Int64

func (countingStorage) Capacity(context.Context) (plugin.Capacity, error) {
	capacityCalls.Add(1)
	return plugin.Capacity{Total: 100, Free: 40}, nil
}

func (countingStorage) Write(context.Context, string, io.Reader) (plugin.ObjectMeta, error) {
	return plugin.ObjectMeta{}, nil
}

func init() {
	plugin.Storages.Register("captest", func([]byte) (plugin.Storage, error) { return countingStorage{}, nil })
}

// The dashboard polls GET /storages every 30s; a remote storage must not be
// re-measured (an SSH dial for sftp) on every poll, but an edit must show.
func TestCapacityCacheReusesUntilTTLOrEdit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newCapacityCache(nil, func() time.Time { return now })
	st := core.Storage{ID: 1, Type: "captest", Config: json.RawMessage(`{"a":1}`)}
	ctx := context.Background()
	start := capacityCalls.Load()
	calls := func() int64 { return capacityCalls.Load() - start }

	got := c.measureAll(ctx, []core.Storage{st})
	if got[0] == nil || got[0].TotalBytes != 100 || got[0].FreeBytes != 40 {
		t.Fatalf("capacity = %+v, want total=100 free=40", got[0])
	}
	c.measureAll(ctx, []core.Storage{st})
	if calls() != 1 {
		t.Fatalf("calls within TTL = %d, want 1", calls())
	}

	st.Config = json.RawMessage(`{"a":2}`)
	c.measureAll(ctx, []core.Storage{st})
	if calls() != 2 {
		t.Fatalf("calls after config edit = %d, want 2", calls())
	}

	now = now.Add(capacityTTL)
	c.measureAll(ctx, []core.Storage{st})
	if calls() != 3 {
		t.Fatalf("calls after TTL = %d, want 3", calls())
	}
}
