package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/duskrun/duskrun/internal/store/sqlite"
)

type connectionTestStatus string

const (
	connectionTestOK      connectionTestStatus = "ok"
	connectionTestFailed  connectionTestStatus = "failed"
	connectionTestSkipped connectionTestStatus = "skipped" // stage not attempted for this engine
)

type connectionTestCheck struct {
	Key    string               `json:"key"`
	Status connectionTestStatus `json:"status"`
	Label  string               `json:"label"`
}

type connectionTestError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

type connectionTestResponse struct {
	Status    connectionTestStatus  `json:"status"`
	LatencyMS int64                 `json:"latency_ms"`
	Checks    []connectionTestCheck `json:"checks"`
	Databases []string              `json:"databases,omitempty"`
	Error     *connectionTestError  `json:"error,omitempty"`
}

// liveCheckOrder is the staged checklist the test endpoint reports, in order.
// The connector/auth/catalog stages mirror checkConnection's phases.
var liveCheckOrder = []struct {
	stage      connCheckStage
	key, label string
}{
	{"config", "config", "Конфигурация"},
	{stageConnector, "connector", "Туннель / endpoint"},
	{stageAuth, "auth", "Авторизация в БД"},
	{stageCatalog, "catalog", "Список баз"},
}

func (s *server) testConnection(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req createConnectionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeConnectionTestFailed(w, start, stageChecks("config"), connectionTestError{
			Code:    "invalid_config",
			Message: "Не удалось прочитать конфигурацию соединения",
			Hint:    "Проверьте заполненные поля и повторите проверку",
		})
		return
	}

	conn, err := buildConnection(req, 0)
	if err != nil {
		s.writeConnectionTestFailed(w, start, stageChecks("config"), connectionTestError{
			Code:    "invalid_config",
			Message: "Конфигурация соединения неполная",
			Hint:    err.Error(),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	databases, err := s.d.ConnectionCheck(ctx, conn)
	if err != nil {
		stage, testErr := classifyConnectionCheck(err)
		s.d.Log.Warn("connection test failed", "engine", conn.Engine, "connector_type", conn.ConnectorType, "stage", stage, "code", testErr.Code, "err", err)
		s.writeConnectionTestFailed(w, start, stageChecks(stage), testErr)
		return
	}

	writeJSON(w, http.StatusOK, connectionTestResponse{
		Status:    connectionTestOK,
		LatencyMS: elapsedMS(start),
		Checks:    successChecks(conn.Engine),
		Databases: databases,
	})
}

// stageChecks builds the checklist for a run whose first failure was at
// failed. Stages before it are ok, failed is failed, and stages after it are
// omitted (not attempted). An empty failed means every stage passed.
func stageChecks(failed connCheckStage) []connectionTestCheck {
	checks := make([]connectionTestCheck, 0, len(liveCheckOrder))
	for _, c := range liveCheckOrder {
		if failed == "" {
			checks = append(checks, okCheck(c.key, c.label))
			continue
		}
		if c.stage == failed {
			checks = append(checks, failedCheck(c.key, c.label))
			return checks
		}
		checks = append(checks, okCheck(c.key, c.label))
	}
	return checks
}

// successChecks builds the checklist for a passing test. checkConnection stops
// after the connector stage for an engine with no catalog probe, so the stages
// it never attempted are reported as skipped instead of painted green.
func successChecks(engine string) []connectionTestCheck {
	if supportsDatabaseList(engine) {
		return stageChecks("") // all stages ok
	}
	checks := make([]connectionTestCheck, 0, len(liveCheckOrder))
	for _, c := range liveCheckOrder {
		if c.stage == stageAuth || c.stage == stageCatalog {
			checks = append(checks, skippedCheck(c.key, c.label))
			continue
		}
		checks = append(checks, okCheck(c.key, c.label))
	}
	return checks
}

// classifyConnectionCheck maps a checkConnection error to the failed stage and a
// sanitized, user-facing error. Opaque errors (e.g. injected stubs) fall back to
// sentinel matching, then to a generic connector-stage failure.
func classifyConnectionCheck(err error) (connCheckStage, connectionTestError) {
	stage := stageConnector
	code := "connect_failed"
	var ce *connCheckError
	switch {
	case errors.As(err, &ce):
		stage, code = ce.Stage, ce.Code
	case errors.Is(err, errDatabaseListUnsupported):
		stage, code = stageCatalog, "unsupported_engine"
	case errors.Is(err, sqlite.ErrNotFound):
		stage, code = stageAuth, "secret_not_found"
	}
	return stage, connectionTestErrorFor(code)
}

func (s *server) writeConnectionTestFailed(w http.ResponseWriter, start time.Time, checks []connectionTestCheck, err connectionTestError) {
	writeJSON(w, http.StatusOK, connectionTestResponse{
		Status:    connectionTestFailed,
		LatencyMS: elapsedMS(start),
		Checks:    checks,
		Error:     &err,
	})
}

func okCheck(key, label string) connectionTestCheck {
	return connectionTestCheck{Key: key, Status: connectionTestOK, Label: label}
}

func failedCheck(key, label string) connectionTestCheck {
	return connectionTestCheck{Key: key, Status: connectionTestFailed, Label: label}
}

func skippedCheck(key, label string) connectionTestCheck {
	return connectionTestCheck{Key: key, Status: connectionTestSkipped, Label: label}
}

func elapsedMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

// connectionTestErrorFor maps an internal error code to a sanitized message and
// hint. It never echoes host/user/DSN or tool stderr — those go to logs only.
func connectionTestErrorFor(code string) connectionTestError {
	switch code {
	case "secret_not_found":
		return connectionTestError{code, "Не найден секрет для проверки соединения", "Проверьте secret_ref или private_key_ref в настройках соединения"}
	case "unsupported_engine":
		return connectionTestError{code, "Проверка соединения для этого движка пока не поддерживается", "Выберите PostgreSQL или MySQL для live-проверки"}
	case "tool_missing":
		return connectionTestError{code, "На хосте duskrun не найден psql или mysql", "Установите CLI-инструмент выбранного движка на сервере duskrun"}
	case "username_required":
		return connectionTestError{code, "Не указан пользователь БД", "Укажите пользователя БД для live-проверки соединения"}
	case "invalid_config":
		return connectionTestError{code, "Конфигурация соединения неполная", "Проверьте параметры подключения и туннеля"}
	case "query_failed":
		return connectionTestError{code, "Подключение есть, но список баз получить не удалось", "Проверьте права пользователя на просмотр списка баз"}
	default: // connect_failed
		return connectionTestError{code, "Не удалось подключиться к базе данных", "Проверьте host, port, пользователя, секрет и параметры туннеля"}
	}
}
