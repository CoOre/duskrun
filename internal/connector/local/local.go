// Package local implements the two connectors that need no tunnel: `direct`
// (TCP host:port) and `socket` (unix socket path). The `ssh-tunnel` connector
// (M4) will decorate these.
package local

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Connectors.Register("direct", newDirect)
	plugin.Connectors.Register("socket", newSocket)
}

// directConfig is the `direct` connector config.
type directConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func newDirect(raw []byte) (plugin.Connector, error) {
	var c directConfig
	if err := unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.Host == "" || c.Port == 0 {
		return nil, fmt.Errorf("direct: host and port are required")
	}
	ep := plugin.Endpoint{Network: "tcp", Address: net.JoinHostPort(c.Host, strconv.Itoa(c.Port))}
	return staticConnector{ep}, nil
}

// socketConfig is the `socket` connector config.
type socketConfig struct {
	Path string `json:"path"`
}

func newSocket(raw []byte) (plugin.Connector, error) {
	var c socketConfig
	if err := unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.Path == "" {
		return nil, fmt.Errorf("socket: path is required")
	}
	return staticConnector{plugin.Endpoint{Network: "unix", Address: c.Path}}, nil
}

// staticConnector returns a fixed endpoint; nothing to tear down.
type staticConnector struct{ ep plugin.Endpoint }

func (s staticConnector) Open(context.Context) (plugin.Endpoint, error) { return s.ep, nil }
func (staticConnector) Close() error                                    { return nil }

func unmarshal(raw []byte, v any) error {
	if len(raw) == 0 {
		return fmt.Errorf("connector: empty config")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("connector: bad config: %w", err)
	}
	return nil
}
