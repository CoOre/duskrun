package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/sshx"
)

// Setting keys. Values are strings; this file owns their meaning.
const (
	settingInstanceName    = "instance_name"
	settingHostKeyDefault  = "ssh_host_key_mode_default"
	defaultInstanceName    = "Duskrun"
	defaultHostKeyModeName = sshx.ModeTOFU
)

// registerSettingRoutes wires the settings page. Reading is open to every role
// (the page also shows read-only deployment facts); writing is admin-only.
func (s *server) registerSettingRoutes(r chi.Router) {
	s.get(r, "/settings", core.RoleViewer, s.getSettings)
	s.patch(r, "/settings", core.RoleAdmin, s.updateSettings)
}

// instanceDTO is the read-only "Instance" block: values that come from the
// environment and a restart, reported with the variable that sets them so the
// page does not imply it can change them.
type instanceDTO struct {
	Version       string `json:"version"`
	DBPath        string `json:"db_path"`
	Workers       int    `json:"workers"`
	RetentionCron string `json:"retention_cron"`
	ListenAddr    string `json:"listen_addr"`
	// Timezone is what cron expressions are interpreted in. Shown, never set:
	// changing it would move every existing schedule (see TZ §10.2).
	Timezone       string `json:"timezone"`
	TimezoneOffset string `json:"timezone_offset"`
	Now            string `json:"now"`
}

type settingsDTO struct {
	InstanceName string      `json:"instance_name"`
	HostKeyMode  string      `json:"ssh_host_key_mode_default"`
	Instance     instanceDTO `json:"instance"`
}

func (s *server) getSettings(w http.ResponseWriter, r *http.Request) {
	out := settingsDTO{
		InstanceName: defaultInstanceName,
		HostKeyMode:  defaultHostKeyModeName,
		Instance:     s.instanceInfo(),
	}
	if s.d.Store != nil {
		ctx := r.Context()
		var err error
		if out.InstanceName, err = s.d.Store.GetSetting(ctx, settingInstanceName, defaultInstanceName); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if out.HostKeyMode, err = s.d.Store.GetSetting(ctx, settingHostKeyDefault, defaultHostKeyModeName); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// instanceInfo renders the read-only block from the process configuration.
func (s *server) instanceInfo() instanceDTO {
	zone, offset, now := nowInfo(s.now())
	return instanceDTO{
		Version:        s.d.Instance.Version,
		DBPath:         s.d.Instance.DBPath,
		Workers:        s.d.Instance.Workers,
		RetentionCron:  s.d.Instance.RetentionCron,
		ListenAddr:     s.d.Instance.ListenAddr,
		Timezone:       zone,
		TimezoneOffset: offset,
		Now:            now,
	}
}

type updateSettingsReq struct {
	InstanceName *string `json:"instance_name"`
	HostKeyMode  *string `json:"ssh_host_key_mode_default"`
}

// updateSettings writes the editable keys. Omitted fields keep their value.
func (s *server) updateSettings(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	var req updateSettingsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	kv := map[string]string{}
	if req.InstanceName != nil {
		name := strings.TrimSpace(*req.InstanceName)
		if name == "" || len([]rune(name)) > 64 {
			writeError(w, http.StatusBadRequest, "instance_name must be 1..64 characters")
			return
		}
		kv[settingInstanceName] = name
	}
	if req.HostKeyMode != nil {
		mode := strings.TrimSpace(*req.HostKeyMode)
		if mode != sshx.ModeTOFU && mode != sshx.ModeStrict {
			writeError(w, http.StatusBadRequest, "ssh_host_key_mode_default must be tofu or strict")
			return
		}
		kv[settingHostKeyDefault] = mode
	}
	if err := s.d.Store.PutSettings(r.Context(), kv); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.getSettings(w, r)
}

// HostKeyModeDefault is the mode applied to a newly created ssh-tunnel
// connection that does not name one.
//
// Deliberately only a default for new connections: the mode lives in each
// connection's connector_config, and rewriting existing rows from this page
// would silently weaken verification on tunnels that are already working.
func (s *server) hostKeyModeDefault(r *http.Request) string {
	if s.d.Store == nil {
		return defaultHostKeyModeName
	}
	mode, err := s.d.Store.GetSetting(r.Context(), settingHostKeyDefault, defaultHostKeyModeName)
	if err != nil {
		s.d.Log.Warn("read default host-key mode", "err", err)
		return defaultHostKeyModeName
	}
	return mode
}

// applyHostKeyDefault stamps the instance default into a new ssh-tunnel
// connection's config when the request did not name a mode. Other connector
// types are returned untouched.
func applyHostKeyDefault(connectorType string, cfg json.RawMessage, mode string) (json.RawMessage, error) {
	if connectorType != "ssh-tunnel" || mode == "" {
		return cfg, nil
	}
	fields := map[string]json.RawMessage{}
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &fields); err != nil {
			return nil, fmt.Errorf("connector_config: %w", err)
		}
		// A literal null unmarshals into a nil map without erroring, and
		// assigning into that panics. Treat it as the empty object it means.
		if fields == nil {
			fields = map[string]json.RawMessage{}
		}
	}
	if raw, ok := fields["host_key_mode"]; ok {
		var v string
		// An explicit empty string means "unset" the same way a missing key
		// does — the connector applies its own default to both.
		if err := json.Unmarshal(raw, &v); err == nil && v != "" {
			return cfg, nil
		}
	}
	enc, err := json.Marshal(mode)
	if err != nil {
		return nil, err
	}
	fields["host_key_mode"] = enc
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func nowInfo(now time.Time) (string, string, string) {
	zone, offset := now.Zone()
	sign := "+"
	if offset < 0 {
		sign, offset = "-", -offset
	}
	off := time.Duration(offset) * time.Second
	return zone,
		sign + time.Time{}.Add(off).Format("15:04"),
		now.Format(time.RFC3339)
}
