package api

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/plugin"
)

const (
	// capacityTimeout bounds one storage's measurement. It covers an SSH
	// handshake for sftp, and keeps a dead host or a hung mount (stale NFS)
	// from holding the list for longer than that.
	capacityTimeout = 5 * time.Second
	// capacityTTL is how long a measurement is reused. The dashboard polls the
	// storage list every 30s; without it every poll would dial each sftp host.
	// Free space moves slowly enough that a minute-old figure is still true.
	capacityTTL = time.Minute
)

// capacityCache measures storage volumes for GET /storages and remembers the
// result per storage for capacityTTL. Failures are remembered too: a host that
// is down should cost one timeout per minute, not one per poll.
type capacityCache struct {
	res *core.SecretResolver
	now func() time.Time

	mu      sync.Mutex
	entries map[int64]capacityEntry
}

type capacityEntry struct {
	at   time.Time
	typ  string
	cfg  []byte // config the figure was measured with; an edit invalidates it
	capa *capacityDTO
}

func newCapacityCache(res *core.SecretResolver, now func() time.Time) *capacityCache {
	return &capacityCache{res: res, now: now, entries: map[int64]capacityEntry{}}
}

// measureAll returns one capacity per storage, in order; nil means unknown.
// Stale entries are measured concurrently, so N slow hosts cost one timeout.
func (c *capacityCache) measureAll(ctx context.Context, storages []core.Storage) []*capacityDTO {
	out := make([]*capacityDTO, len(storages))
	var wg sync.WaitGroup
	for i, st := range storages {
		if capa, ok := c.cached(st); ok {
			out[i] = capa
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = c.measure(ctx, st)
			// A measurement cut short by the client going away says nothing
			// about the storage, so it must not be remembered as "unknown".
			if ctx.Err() == nil {
				c.store(st, out[i])
			}
		}()
	}
	wg.Wait()
	return out
}

func (c *capacityCache) cached(st core.Storage) (*capacityDTO, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[st.ID]
	if !ok || e.typ != st.Type || !bytes.Equal(e.cfg, st.Config) || c.now().Sub(e.at) >= capacityTTL {
		return nil, false
	}
	return e.capa, true
}

func (c *capacityCache) store(st core.Storage, capa *capacityDTO) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[st.ID] = capacityEntry{at: c.now(), typ: st.Type, cfg: bytes.Clone(st.Config), capa: capa}
}

// measure builds st's plugin and asks it for its volume size when it can
// report one. Every failure — an unresolvable secret, an unreachable host, a
// server without statvfs — yields nil: capacity is a hint on a list response,
// and one broken destination must not hide the rest.
func (c *capacityCache) measure(ctx context.Context, st core.Storage) *capacityDTO {
	config, err := c.res.Resolve(ctx, st.Config)
	if err != nil {
		return nil
	}
	storage, err := plugin.Storages.Create(st.Type, config)
	if err != nil {
		return nil
	}
	defer core.CloseStorage(storage)
	cr, ok := storage.(plugin.CapacityReporter)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, capacityTimeout)
	defer cancel()
	capa, err := cr.Capacity(ctx)
	if err != nil {
		return nil
	}
	return &capacityDTO{TotalBytes: capa.Total, FreeBytes: capa.Free}
}
