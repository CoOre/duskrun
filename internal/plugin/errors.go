package plugin

import "errors"

// ErrModeUnsupported is returned by a Dumper when asked for a mode it does not
// implement (e.g. a stream-only dumper's DumpStaged).
var ErrModeUnsupported = errors.New("plugin: dump mode unsupported")

// ErrNotRegistered is returned by a Registry when no factory is bound to a key.
var ErrNotRegistered = errors.New("plugin: not registered")
