package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/plugin"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

var (
	errDatabaseListUnsupported = errors.New("database listing is not supported for this engine")
	errDatabaseUsernameMissing = errors.New("database username is required for live database checks")
)

// connCheckStage names a phase of the live connection check. It drives which
// checklist item the API paints red; the raw error stays server-side.
type connCheckStage string

const (
	stageConnector connCheckStage = "connector" // resolve tunnel secret, build+Open connector
	stageAuth      connCheckStage = "auth"      // DB client present, creds resolved, authenticate
	stageCatalog   connCheckStage = "catalog"   // list databases
)

// connCheckError tags a stage failure with a UI error code so the handler can
// build the checklist and message/hint without parsing tool stderr. The wrapped
// err is for logs only and never reaches the client.
type connCheckError struct {
	Stage connCheckStage
	Code  string
	err   error
}

func (e *connCheckError) Error() string { return e.err.Error() }
func (e *connCheckError) Unwrap() error { return e.err }

func supportsDatabaseList(engine string) bool {
	return engine == "postgres" || engine == "mysql"
}

// listDatabases is the plain lister behind GET /connections/{id}/databases: it
// returns names or an opaque error, without stage instrumentation.
func listDatabases(ctx context.Context, res *core.SecretResolver, conn core.Connection) ([]string, error) {
	cfg, err := resolveListConnectorConfig(ctx, res, conn.ConnectorConfig)
	if err != nil {
		return nil, err
	}
	connector, err := plugin.Connectors.Create(conn.ConnectorType, cfg)
	if err != nil {
		return nil, err
	}
	defer connector.Close()

	ep, err := connector.Open(ctx)
	if err != nil {
		return nil, err
	}
	creds, err := resolveListCreds(ctx, res, conn)
	if err != nil {
		return nil, err
	}
	return listDatabasesOnEndpoint(ctx, conn.Engine, ep, creds)
}

// checkConnection is the staged variant behind POST /connections/test. It runs
// the same probe as listDatabases but reports, via *connCheckError, exactly
// which stage failed (connector → auth → catalog) so the UI can show an honest
// checklist. toolCheck verifies the engine's DB client (psql/mysql) is present.
func checkConnection(ctx context.Context, res *core.SecretResolver, toolCheck func(string) error, conn core.Connection) ([]string, error) {
	// --- connector stage: resolve the tunnel key secret, build and open it ---
	cfg, err := resolveListConnectorConfig(ctx, res, conn.ConnectorConfig)
	if err != nil {
		return nil, connectorStageErr(err)
	}
	connector, err := plugin.Connectors.Create(conn.ConnectorType, cfg)
	if err != nil {
		return nil, &connCheckError{Stage: stageConnector, Code: "invalid_config", err: err}
	}
	defer connector.Close()
	ep, err := connector.Open(ctx)
	if err != nil {
		return nil, &connCheckError{Stage: stageConnector, Code: "connect_failed", err: err}
	}

	// An engine with no catalog probe (mongodb, redis) can only be verified this
	// far: there is no list query to run, and a missing DB username is normal
	// there — redis has no user by default. Stopping here reports the untried
	// stages as skipped; running them would fail every healthy connection.
	// direct/socket Open never touches the network, so the endpoint is dialed
	// here — otherwise a mistyped host or a closed port would pass the check.
	if !supportsDatabaseList(conn.Engine) {
		if err := dialEndpoint(ctx, ep); err != nil {
			return nil, &connCheckError{Stage: stageConnector, Code: "connect_failed", err: err}
		}
		return nil, nil
	}

	// --- auth stage: DB client present, password secret resolved ---
	if toolCheck != nil {
		if err := toolCheck(conn.Engine); err != nil {
			return nil, &connCheckError{Stage: stageAuth, Code: "tool_missing", err: err}
		}
	}
	creds, err := resolveListCreds(ctx, res, conn)
	if err != nil {
		return nil, authStageErr(err)
	}

	// --- catalog stage: the list query also proves auth on success ---
	dbs, listErr := listDatabasesOnEndpoint(ctx, conn.Engine, ep, creds)
	if listErr == nil {
		return dbs, nil
	}
	if errors.Is(listErr, errDatabaseListUnsupported) {
		return nil, &connCheckError{Stage: stageCatalog, Code: "unsupported_engine", err: listErr}
	}
	// The list query failed. Disambiguate auth-vs-catalog with a trivial probe
	// instead of parsing psql/mysql stderr: if even SELECT 1 fails, it's auth;
	// if the probe connects but the catalog query didn't, blame the catalog.
	if _, authErr := authProbe(ctx, conn.Engine, ep, creds); authErr != nil {
		return nil, &connCheckError{Stage: stageAuth, Code: "connect_failed", err: authErr}
	}
	return nil, &connCheckError{Stage: stageCatalog, Code: "query_failed", err: listErr}
}

// endpointDialTimeout bounds the reachability probe for engines checked by
// dial alone, so an unroutable host doesn't hang the request.
const endpointDialTimeout = 5 * time.Second

// dialEndpoint proves the endpoint accepts connections (TCP or unix socket).
func dialEndpoint(ctx context.Context, ep plugin.Endpoint) error {
	ctx, cancel := context.WithTimeout(ctx, endpointDialTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, ep.Network, ep.Address)
	if err != nil {
		return err
	}
	return c.Close()
}

// connectorStageErr / authStageErr map a resolve error to a stage, surfacing a
// missing secret distinctly from a generic failure.
func connectorStageErr(err error) *connCheckError {
	code := "connect_failed"
	if errors.Is(err, sqlite.ErrNotFound) {
		code = "secret_not_found"
	}
	return &connCheckError{Stage: stageConnector, Code: code, err: err}
}

func authStageErr(err error) *connCheckError {
	code := "connect_failed"
	switch {
	case errors.Is(err, sqlite.ErrNotFound):
		code = "secret_not_found"
	case errors.Is(err, errDatabaseUsernameMissing):
		code = "username_required"
	}
	return &connCheckError{Stage: stageAuth, Code: code, err: err}
}

func listDatabasesOnEndpoint(ctx context.Context, engine string, ep plugin.Endpoint, creds plugin.Credentials) ([]string, error) {
	switch engine {
	case "postgres":
		return listPostgresDatabases(ctx, ep, creds)
	case "mysql":
		return listMySQLDatabases(ctx, ep, creds)
	default:
		return nil, fmt.Errorf("%w: %s", errDatabaseListUnsupported, engine)
	}
}

// authProbe runs a trivial SELECT 1 to distinguish an auth failure from a
// catalog-query failure without inspecting tool stderr.
func authProbe(ctx context.Context, engine string, ep plugin.Endpoint, creds plugin.Credentials) ([]byte, error) {
	switch engine {
	case "postgres":
		return runPostgresQuery(ctx, ep, creds, "SELECT 1")
	case "mysql":
		return runMySQLQuery(ctx, ep, creds, "SELECT 1")
	default:
		return nil, fmt.Errorf("%w: %s", errDatabaseListUnsupported, engine)
	}
}

func resolveListCreds(ctx context.Context, res *core.SecretResolver, conn core.Connection) (plugin.Credentials, error) {
	username := strings.TrimSpace(conn.Username)
	if username == "" {
		return plugin.Credentials{}, errDatabaseUsernameMissing
	}
	creds := plugin.Credentials{Username: username}
	if conn.SecretRef == "" {
		return creds, nil
	}
	pw, err := res.Open(ctx, conn.SecretRef)
	if err != nil {
		return plugin.Credentials{}, err
	}
	creds.Password = string(pw)
	return creds, nil
}

// resolveListConnectorConfig substitutes the connector's secret references the
// same way the executor does, so a probe reaches the host with the same key the
// scheduled run would use.
func resolveListConnectorConfig(ctx context.Context, res *core.SecretResolver, raw json.RawMessage) (json.RawMessage, error) {
	cfg, err := res.Resolve(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("connector config: %w", err)
	}
	return cfg, nil
}

func listPostgresDatabases(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials) ([]string, error) {
	query := `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname`
	out, err := runPostgresQuery(ctx, ep, cr, query)
	if err != nil {
		return nil, err
	}
	return parseDatabaseNames(out), nil
}

// runPostgresQuery runs one psql query, trying the postgres then template1
// maintenance database so a locked-down default DB doesn't fail the whole probe.
func runPostgresQuery(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, query string) ([]byte, error) {
	var lastErr error
	for _, db := range []string{"postgres", "template1"} {
		args, err := postgresListArgs(ep, cr.Username, db, query)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, "psql", args...)
		cmd.Env = append(os.Environ(), "PGPASSWORD="+cr.Password, "PGCONNECT_TIMEOUT=5")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return out, nil
		}
		lastErr = fmt.Errorf("postgres query via %s: %w: %s", db, err, strings.TrimSpace(string(out)))
	}
	return nil, lastErr
}

func postgresListArgs(ep plugin.Endpoint, user, db, query string) ([]string, error) {
	args := []string{"--no-password", "-Atq", "-c", query, "-d", db}
	switch ep.Network {
	case "tcp":
		host, port, err := net.SplitHostPort(ep.Address)
		if err != nil {
			return nil, fmt.Errorf("postgres: bad tcp address %q: %w", ep.Address, err)
		}
		args = append(args, "-h", host, "-p", port)
	case "unix":
		args = append(args, "-h", ep.Address)
	default:
		return nil, fmt.Errorf("postgres: unsupported endpoint network %q", ep.Network)
	}
	if user != "" {
		args = append(args, "-U", user)
	}
	return args, nil
}

func listMySQLDatabases(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials) ([]string, error) {
	out, err := runMySQLQuery(ctx, ep, cr, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	return parseDatabaseNames(out), nil
}

func runMySQLQuery(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, sql string) ([]byte, error) {
	args, err := mysqlQueryArgs(ep, cr.Username, sql)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "mysql", args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+cr.Password)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("mysql query: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func mysqlQueryArgs(ep plugin.Endpoint, user, sql string) ([]string, error) {
	args := []string{"--batch", "--skip-column-names", "--connect-timeout=5", "-e", sql}
	switch ep.Network {
	case "tcp":
		host, port, err := net.SplitHostPort(ep.Address)
		if err != nil {
			return nil, fmt.Errorf("mysql: bad tcp address %q: %w", ep.Address, err)
		}
		args = append(args, "--protocol=tcp", "-h", host, "-P", port)
	case "unix":
		args = append(args, "--protocol=socket", "--socket="+ep.Address)
	default:
		return nil, fmt.Errorf("mysql: unsupported endpoint network %q", ep.Network)
	}
	if user != "" {
		args = append(args, "-u", user)
	}
	return args, nil
}

func parseDatabaseNames(out []byte) []string {
	seen := make(map[string]struct{})
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
