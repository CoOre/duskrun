package plugin

import (
	"fmt"
	"sort"
	"sync"
)

// Factory builds a plugin instance of type T from its opaque JSON config
// (the connector_config / storage config / dumper options blob on the row).
// Config is raw JSON bytes; a nil/empty slice means "no config".
type Factory[T any] func(cfg []byte) (T, error)

// Registry is a typed, concurrency-safe map from plugin key to Factory. One
// Registry exists per plugin kind (see the package-level vars below). New
// implementations register in their package's init(), so the core learns about
// them purely through blank imports — no central switch to edit.
type Registry[T any] struct {
	kind string
	mu   sync.RWMutex
	m    map[string]Factory[T]
}

// NewRegistry creates an empty registry labelled with a kind (for errors).
func NewRegistry[T any](kind string) *Registry[T] {
	return &Registry[T]{kind: kind, m: make(map[string]Factory[T])}
}

// Register binds a factory to key. It panics on a duplicate key because that is
// always a programming error (two plugins claiming the same name at init time).
func (r *Registry[T]) Register(key string, f Factory[T]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[key]; dup {
		panic(fmt.Sprintf("plugin: %s %q already registered", r.kind, key))
	}
	r.m[key] = f
}

// Create instantiates the plugin bound to key with cfg (raw JSON bytes).
func (r *Registry[T]) Create(key string, cfg []byte) (T, error) {
	r.mu.RLock()
	f, ok := r.m[key]
	r.mu.RUnlock()
	if !ok {
		var zero T
		return zero, fmt.Errorf("%w: %s %q", ErrNotRegistered, r.kind, key)
	}
	return f(cfg)
}

// Has reports whether key is registered.
func (r *Registry[T]) Has(key string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.m[key]
	return ok
}

// Names returns the registered keys, sorted — for CLI listings and the UI's
// "N plugins available" counter.
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Package-level registries, one per pipeline node. Concrete plugin packages
// (internal/connector/direct, internal/dumper/postgres, ...) call these from
// init(); cmd/duskrun blank-imports those packages to populate them.
var (
	Connectors = NewRegistry[Connector]("connector")
	Dumpers    = NewRegistry[Dumper]("dumper")
	Codecs     = NewRegistry[Codec]("codec")
	Storages   = NewRegistry[Storage]("storage")
	Notifiers  = NewRegistry[Notifier]("notifier")
)
