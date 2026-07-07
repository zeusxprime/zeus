package xray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
)

type Result struct {
	Username             string `json:"username"`
	UUID                 string `json:"uuid"`
	Expiry               string `json:"expiry"`
	Limit                int    `json:"limit"`
	Protocol             string `json:"protocol"`
	Link                 string `json:"link"`
	ConfigPath           string `json:"config_path,omitempty"`
	DragonCoreRegistered bool   `json:"dragoncore_registered"`
	XrayConfigUpdated    bool   `json:"xray_config_updated"`
	RestartInfo          string `json:"restart_info,omitempty"`
}

type ApplyOptions struct {
	SafeRestart bool
	NoRestart   bool
}

type Manager struct {
	cfg config.Config
}

func NewManager(cfg config.Config) *Manager { return &Manager{cfg: cfg} }

func (m *Manager) ApplyAccount(ctx context.Context, a model.Account, opts ApplyOptions) (*Result, error) {
	if a.Username == "" {
		return nil, errors.New("usuário vazio")
	}
	uuid, err := NormalizeUUID(a.UUID)
	if err != nil {
		return nil, fmt.Errorf("UUID inválido para %s: %w", a.Username, err)
	}
	path, cfg, err := m.LoadConfig()
	if err != nil {
		return nil, err
	}
	updated := false
	restartInfo := ""
	var configForLink map[string]any
	if path != "" && cfg != nil && m.cfg.Xray.EnableDirectConfig {
		prev, _ := os.ReadFile(path)
		changed, err := m.UpsertClient(cfg, a.Username, uuid)
		if err != nil {
			return nil, err
		}
		logChanged := m.EnsureAccessLog(cfg)
		if changed || logChanged {
			if err := backupFile(path, "bak"); err != nil {
				return nil, err
			}
			if err := writeJSONAtomic(path, cfg); err != nil {
				return nil, err
			}
			if !opts.NoRestart && m.cfg.Xray.RestartCommand != "" {
				if opts.SafeRestart {
					ok, info := m.SafeRestart(ctx, path, string(prev), cfg)
					restartInfo = info
					if !ok {
						return nil, errors.New(info)
					}
				} else {
					info, err := runShell(ctx, 35*time.Second, m.cfg.Xray.RestartCommand)
					restartInfo = strings.TrimSpace(info)
					if err != nil {
						return nil, fmt.Errorf("restart xray: %w: %s", err, info)
					}
				}
			}
			updated = true
		}
		configForLink = cfg
	}
	pg := m.RegisterDragonCore(ctx, a.Username, uuid, a.ExpiryDate)
	return &Result{Username: a.Username, UUID: uuid, Expiry: a.ExpiryDate, Limit: a.LimitConnections, Protocol: m.protocol(), Link: m.BuildVlessLink(a.Username, uuid, configForLink), ConfigPath: path, DragonCoreRegistered: pg, XrayConfigUpdated: updated, RestartInfo: restartInfo}, nil
}

func (m *Manager) RemoveAccount(ctx context.Context, username, uuid string, opts ApplyOptions) error {
	path, cfg, err := m.LoadConfig()
	if err != nil {
		return err
	}
	if path != "" && cfg != nil && m.cfg.Xray.EnableDirectConfig {
		prev, _ := os.ReadFile(path)
		changed, err := m.RemoveClient(cfg, username, uuid)
		if err != nil {
			return err
		}
		if changed {
			if err := backupFile(path, "bak-remove"); err != nil {
				return err
			}
			if err := writeJSONAtomic(path, cfg); err != nil {
				return err
			}
			if !opts.NoRestart && m.cfg.Xray.RestartCommand != "" {
				if opts.SafeRestart {
					ok, info := m.SafeRestart(ctx, path, string(prev), cfg)
					if !ok {
						return errors.New(info)
					}
				} else if _, err := runShell(ctx, 35*time.Second, m.cfg.Xray.RestartCommand); err != nil {
					return err
				}
			}
		}
	}
	_ = m.UnregisterDragonCore(ctx, username, uuid)
	return nil
}

// PruneClientsNotDesired remove do Xray clientes gerenciados que não fazem parte
// do snapshot ativo do principal. É usado nas VPS secundárias para a quantidade
// de UUIDs bater com o bot, evitando sobras antigas após remover/expirar contas.
func (m *Manager) PruneClientsNotDesired(ctx context.Context, desired map[string]bool, opts ApplyOptions) (int, error) {
	desired = normalizeDesiredUsers(desired)
	path, cfg, err := m.LoadConfig()
	if err != nil {
		return 0, err
	}
	if path == "" || cfg == nil || !m.cfg.Xray.EnableDirectConfig {
		return 0, nil
	}
	in := m.SelectInbound(cfg)
	if in == nil {
		return 0, nil
	}
	settingsObj, _ := in["settings"].(map[string]any)
	if settingsObj == nil {
		return 0, nil
	}
	clients, _ := settingsObj["clients"].([]any)
	if len(clients) == 0 {
		return 0, nil
	}
	out := make([]any, 0, len(clients))
	removed := 0
	removedPairs := make([][2]string, 0)
	for _, item := range clients {
		client, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		email := strings.TrimSpace(str(client["email"]))
		uuid := strings.TrimSpace(str(client["id"]))
		key := strings.ToLower(email)
		if email == "" || !managedXrayEmail(email) || desired[key] {
			out = append(out, item)
			continue
		}
		removed++
		removedPairs = append(removedPairs, [2]string{email, uuid})
	}
	if removed == 0 {
		return 0, nil
	}
	prev, _ := os.ReadFile(path)
	settingsObj["clients"] = out
	if err := backupFile(path, "bak-prune"); err != nil {
		return removed, err
	}
	if err := writeJSONAtomic(path, cfg); err != nil {
		return removed, err
	}
	for _, p := range removedPairs {
		_ = m.UnregisterDragonCore(ctx, p[0], p[1])
	}
	if !opts.NoRestart && m.cfg.Xray.RestartCommand != "" {
		if opts.SafeRestart {
			ok, info := m.SafeRestart(ctx, path, string(prev), cfg)
			if !ok {
				return removed, errors.New(info)
			}
		} else if _, err := runShell(ctx, 35*time.Second, m.cfg.Xray.RestartCommand); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func normalizeDesiredUsers(in map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range in {
		if !v {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" {
			out[k] = true
		}
	}
	return out
}

func managedXrayEmail(email string) bool {
	email = strings.TrimSpace(email)
	if email == "" {
		return false
	}
	return managedXrayEmailRe.MatchString(email)
}

func (m *Manager) ApplyBatch(ctx context.Context, accounts []model.Account, opts ApplyOptions) (map[string]*Result, error) {
	results := map[string]*Result{}
	path, cfg, err := m.LoadConfig()
	if err != nil {
		return results, err
	}
	changed := false
	var prev []byte
	if path != "" && cfg != nil && m.cfg.Xray.EnableDirectConfig {
		prev, _ = os.ReadFile(path)
		for _, a := range accounts {
			if a.UUID == "" {
				continue
			}
			uuid, err := NormalizeUUID(a.UUID)
			if err != nil {
				results[a.Username] = &Result{Username: a.Username, UUID: a.UUID, RestartInfo: "uuid_invalido"}
				continue
			}
			c, err := m.UpsertClient(cfg, a.Username, uuid)
			if err != nil {
				return results, err
			}
			changed = changed || c
			if m.RegisterDragonCore(ctx, a.Username, uuid, a.ExpiryDate) {
				// registrado no resultado abaixo.
			}
			results[a.Username] = &Result{Username: a.Username, UUID: uuid, Expiry: a.ExpiryDate, Limit: a.LimitConnections, Protocol: m.protocol(), Link: m.BuildVlessLink(a.Username, uuid, cfg), ConfigPath: path}
		}
		changed = m.EnsureAccessLog(cfg) || changed
		if changed {
			if err := backupFile(path, "bak-batch"); err != nil {
				return results, err
			}
			if err := writeJSONAtomic(path, cfg); err != nil {
				return results, err
			}
			if !opts.NoRestart && m.cfg.Xray.RestartCommand != "" {
				if opts.SafeRestart {
					ok, info := m.SafeRestart(ctx, path, string(prev), cfg)
					for k := range results {
						results[k].RestartInfo = info
						results[k].XrayConfigUpdated = ok
					}
					if !ok {
						return results, errors.New(info)
					}
				} else if info, err := runShell(ctx, 35*time.Second, m.cfg.Xray.RestartCommand); err != nil {
					return results, fmt.Errorf("restart xray: %w: %s", err, info)
				}
			}
		}
	}
	return results, nil
}

func (m *Manager) LoadConfig() (string, map[string]any, error) {
	for _, path := range m.cfg.Xray.ConfigPaths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(b, &data); err != nil {
			return "", nil, fmt.Errorf("config xray inválido %s: %w", path, err)
		}
		return path, data, nil
	}
	return "", nil, nil
}

func (m *Manager) SelectInbound(cfg map[string]any) map[string]any {
	inbounds, _ := cfg["inbounds"].([]any)
	requiredNetwork := strings.ToLower(strings.TrimSpace(m.cfg.Xray.LinkNetwork))
	protocol := strings.ToLower(strings.TrimSpace(m.cfg.Xray.Protocol))
	if protocol == "" {
		protocol = "vless"
	}
	if tag := m.cfg.Xray.InboundTag; tag != "" {
		for _, it := range inbounds {
			in, ok := it.(map[string]any)
			if ok && str(in["tag"]) == tag && strings.EqualFold(str(in["protocol"]), protocol) && hasClients(in) {
				return in
			}
		}
	}
	for _, it := range inbounds {
		in, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if !strings.EqualFold(str(in["protocol"]), protocol) || !hasClients(in) {
			continue
		}
		if requiredNetwork != "" && inboundNetwork(in) != requiredNetwork {
			continue
		}
		return in
	}
	for _, it := range inbounds {
		in, ok := it.(map[string]any)
		if ok && strings.EqualFold(str(in["protocol"]), protocol) && hasClients(in) {
			return in
		}
	}
	return nil
}

func (m *Manager) UpsertClient(cfg map[string]any, username, uuid string) (bool, error) {
	in := m.SelectInbound(cfg)
	if in == nil {
		return false, errors.New("inbound VLESS com clients[] não encontrado")
	}
	settingsObj, _ := in["settings"].(map[string]any)
	if settingsObj == nil {
		settingsObj = map[string]any{}
		in["settings"] = settingsObj
	}
	clients, _ := settingsObj["clients"].([]any)
	changed := false
	for _, item := range clients {
		client, ok := item.(map[string]any)
		if !ok {
			continue
		}
		email := strings.TrimSpace(str(client["email"]))
		cid := strings.TrimSpace(str(client["id"]))
		if email == username {
			if cid != uuid {
				client["id"] = uuid
				changed = true
			}
			if toInt(client["level"]) != 0 {
				client["level"] = float64(0)
				changed = true
			}
			if m.cfg.Xray.Flow != "" {
				if str(client["flow"]) != m.cfg.Xray.Flow {
					client["flow"] = m.cfg.Xray.Flow
					changed = true
				}
			} else if _, ok := client["flow"]; ok {
				delete(client, "flow")
				changed = true
			}
			return changed, nil
		}
		if cid == uuid && email != "" && email != username {
			return false, fmt.Errorf("UUID %s já está vinculado a %s", uuid, email)
		}
	}
	client := map[string]any{"id": uuid, "email": username, "level": float64(0)}
	if m.cfg.Xray.Flow != "" {
		client["flow"] = m.cfg.Xray.Flow
	}
	settingsObj["clients"] = append(clients, client)
	return true, nil
}

func (m *Manager) RemoveClient(cfg map[string]any, username, uuid string) (bool, error) {
	in := m.SelectInbound(cfg)
	if in == nil {
		return false, nil
	}
	settingsObj, _ := in["settings"].(map[string]any)
	clients, _ := settingsObj["clients"].([]any)
	out := make([]any, 0, len(clients))
	changed := false
	for _, item := range clients {
		client, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		email := strings.TrimSpace(str(client["email"]))
		cid := strings.TrimSpace(str(client["id"]))
		if strings.EqualFold(email, username) || (uuid != "" && strings.EqualFold(cid, uuid)) {
			changed = true
			continue
		}
		out = append(out, item)
	}
	if changed {
		settingsObj["clients"] = out
	}
	return changed, nil
}

func (m *Manager) EnsureAccessLog(cfg map[string]any) bool {
	path := "/var/log/xray/access.log"
	for _, p := range m.cfg.Xray.AccessLogPaths {
		if strings.TrimSpace(p) != "" {
			path = strings.TrimSpace(p)
			break
		}
	}
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	logCfg, _ := cfg["log"].(map[string]any)
	if logCfg == nil {
		logCfg = map[string]any{}
		cfg["log"] = logCfg
	}
	changed := false
	if str(logCfg["access"]) != path {
		logCfg["access"] = path
		changed = true
	}
	if strings.TrimSpace(str(logCfg["loglevel"])) == "" {
		logCfg["loglevel"] = "warning"
		changed = true
	}
	return changed
}

func (m *Manager) BuildVlessLink(username, uuid string, cfg map[string]any) string {
	p := m.extractLinkParams(cfg)
	q := url.Values{}
	q.Set("encryption", "none")
	q.Set("security", def(p["security"], "none"))
	q.Set("type", def(p["network"], "tcp"))
	if p["sni"] != "" {
		q.Set("sni", p["sni"])
	}
	if p["header_host"] != "" {
		q.Set("host", p["header_host"])
	}
	if p["path"] != "" {
		q.Set("path", p["path"])
	}
	if m.cfg.Xray.Flow != "" {
		q.Set("flow", m.cfg.Xray.Flow)
	}
	for _, item := range strings.Split(m.cfg.Xray.LinkExtra, "&") {
		if k, v, ok := strings.Cut(item, "="); ok && strings.TrimSpace(k) != "" {
			q.Set(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return fmt.Sprintf("vless://%s@%s:%s?%s#%s", uuid, p["host"], p["port"], q.Encode(), url.QueryEscape(username))
}

func (m *Manager) extractLinkParams(cfg map[string]any) map[string]string {
	p := map[string]string{"host": m.cfg.ServerHost, "port": strconv.Itoa(m.cfg.Xray.LinkPort), "security": m.cfg.Xray.LinkSecurity, "network": m.cfg.Xray.LinkNetwork, "sni": m.cfg.Xray.LinkSNI, "header_host": m.cfg.Xray.LinkHost, "path": m.cfg.Xray.LinkPath}
	if p["host"] == "" {
		p["host"] = "127.0.0.1"
	}
	if cfg == nil {
		return p
	}
	in := m.SelectInbound(cfg)
	if in == nil {
		return p
	}
	if port := str(in["port"]); port != "" {
		p["port"] = port
	}
	stream, _ := in["streamSettings"].(map[string]any)
	if sec := str(stream["security"]); sec != "" {
		p["security"] = sec
	}
	if netw := str(stream["network"]); netw != "" {
		p["network"] = netw
	}
	for _, key := range []string{p["network"] + "Settings", "wsSettings", "xhttpSettings", "httpSettings", "grpcSettings", "tcpSettings"} {
		obj, _ := stream[key].(map[string]any)
		if obj == nil {
			continue
		}
		if path := str(obj["path"]); path != "" {
			p["path"] = path
		}
		headers, _ := obj["headers"].(map[string]any)
		if host := str(headers["Host"]); host != "" {
			p["header_host"] = host
		}
		if host := str(obj["host"]); host != "" {
			p["header_host"] = host
		}
		break
	}
	if tls, _ := stream["tlsSettings"].(map[string]any); tls != nil {
		if sni := str(tls["serverName"]); sni != "" {
			p["sni"] = sni
		}
	}
	if reality, _ := stream["realitySettings"].(map[string]any); reality != nil {
		if names, _ := reality["serverNames"].([]any); len(names) > 0 {
			p["sni"] = str(names[0])
		}
	}
	return p
}

func (m *Manager) RegisterDragonCore(ctx context.Context, username, uuid, expiry string) bool {
	if !m.cfg.Xray.EnableDragonCorePG {
		return false
	}
	if _, err := exec.LookPath(m.cfg.Xray.DragonCorePSQLBin); err != nil {
		return false
	}
	sql := fmt.Sprintf("CREATE TABLE IF NOT EXISTS xray (id SERIAL PRIMARY KEY, uuid TEXT, nick TEXT, expiry DATE, protocol TEXT); UPDATE xray SET uuid = %s, nick = %s, expiry = %s, protocol = %s WHERE nick = %s OR uuid = %s; INSERT INTO xray (uuid, nick, expiry, protocol) SELECT %s, %s, %s, %s WHERE NOT EXISTS (SELECT 1 FROM xray WHERE nick = %s OR uuid = %s);", sqlQuote(uuid), sqlQuote(username), sqlQuote(expiry), sqlQuote(m.protocol()), sqlQuote(username), sqlQuote(uuid), sqlQuote(uuid), sqlQuote(username), sqlQuote(expiry), sqlQuote(m.protocol()), sqlQuote(username), sqlQuote(uuid))
	candidates := [][]string{{"sudo", "-u", m.cfg.Xray.DragonCorePSQLUser, m.cfg.Xray.DragonCorePSQLBin, "-d", m.cfg.Xray.DragonCoreDB, "-v", "ON_ERROR_STOP=1", "-c", sql}, {m.cfg.Xray.DragonCorePSQLBin, "-d", m.cfg.Xray.DragonCoreDB, "-v", "ON_ERROR_STOP=1", "-c", sql}}
	for _, args := range candidates {
		if err := runCmd(ctx, 20*time.Second, args[0], args[1:]...); err == nil {
			return true
		}
	}
	return false
}

func (m *Manager) UnregisterDragonCore(ctx context.Context, username, uuid string) bool {
	if !m.cfg.Xray.EnableDragonCorePG {
		return false
	}
	if _, err := exec.LookPath(m.cfg.Xray.DragonCorePSQLBin); err != nil {
		return false
	}
	cond := "nick = " + sqlQuote(username)
	if uuid != "" {
		cond += " OR uuid = " + sqlQuote(uuid)
	}
	sql := "DELETE FROM xray WHERE " + cond + ";"
	for _, args := range [][]string{{"sudo", "-u", m.cfg.Xray.DragonCorePSQLUser, m.cfg.Xray.DragonCorePSQLBin, "-d", m.cfg.Xray.DragonCoreDB, "-v", "ON_ERROR_STOP=1", "-c", sql}, {m.cfg.Xray.DragonCorePSQLBin, "-d", m.cfg.Xray.DragonCoreDB, "-v", "ON_ERROR_STOP=1", "-c", sql}} {
		if err := runCmd(ctx, 20*time.Second, args[0], args[1:]...); err == nil {
			return true
		}
	}
	return false
}

func (m *Manager) SafeRestart(ctx context.Context, path, previous string, cfg map[string]any) (bool, string) {
	ok, info := TestConfig(ctx, path)
	if !ok {
		_ = os.WriteFile(path, []byte(previous), 0600)
		return false, "config_test_failed:" + info
	}
	before := listeningPorts(ctx)
	expected := inboundPorts(cfg)
	if m.cfg.Xray.RestartCommand == "" {
		return true, "config_written_no_restart:" + info
	}
	if output, err := runShell(ctx, 35*time.Second, m.cfg.Xray.RestartCommand); err != nil {
		_ = os.WriteFile(path, []byte(previous), 0600)
		_, _ = runShell(ctx, 35*time.Second, m.cfg.Xray.RestartCommand)
		return false, "restart_failed_rollback:" + err.Error() + ":" + output
	}
	time.Sleep(2 * time.Second)
	after := listeningPorts(ctx)
	check := expected
	if len(check) == 0 {
		check = before
	}
	if len(check) > 0 && len(after) > 0 && !intersects(check, after) {
		_ = os.WriteFile(path, []byte(previous), 0600)
		_, _ = runShell(ctx, 35*time.Second, m.cfg.Xray.RestartCommand)
		return false, "port_check_failed_rollback"
	}
	return true, "restart_ok:" + info
}

func TestConfig(ctx context.Context, path string) (bool, string) {
	candidates := [][]string{{"/usr/local/bin/xray", "run", "-test", "-config", path}, {"xray", "run", "-test", "-config", path}, {"/usr/local/bin/xray", "test", "-config", path}, {"xray", "test", "-config", path}, {"xray", "test", "-c", path}, {"xray", "-test", "-config", path}, {"v2ray", "test", "-config", path}, {"v2ray", "test", "-c", path}}
	saw := false
	var outs []string
	for _, args := range candidates {
		if strings.Contains(args[0], "/") {
			if _, err := os.Stat(args[0]); err != nil {
				continue
			}
		} else if _, err := exec.LookPath(args[0]); err != nil {
			continue
		}
		saw = true
		out, err := runCmdOutput(ctx, 20*time.Second, args[0], args[1:]...)
		if err == nil {
			return true, strings.TrimSpace(out)
		}
		outs = append(outs, strings.Join(args, " ")+" => "+strings.TrimSpace(out)+" "+err.Error())
	}
	if !saw {
		return true, "config_test_skipped:no_cli"
	}
	return false, strings.Join(outs, "; ")
}

func inboundNetwork(in map[string]any) string {
	stream, _ := in["streamSettings"].(map[string]any)
	return strings.ToLower(strings.TrimSpace(str(stream["network"])))
}
func hasClients(in map[string]any) bool {
	settingsObj, _ := in["settings"].(map[string]any)
	_, ok := settingsObj["clients"].([]any)
	return ok
}
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case json.Number:
		return string(t)
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
}
func toInt(v any) int { n, _ := strconv.Atoi(str(v)); return n }
func def(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}
func (m *Manager) protocol() string {
	if m.cfg.Xray.DragonCoreXrayProtocol != "" {
		return m.cfg.Xray.DragonCoreXrayProtocol
	}
	return "xhttp"
}
func sqlQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

var managedXrayEmailRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,11}$`)

var uuidRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$`)

func NormalizeUUID(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if !uuidRe.MatchString(v) {
		return "", errors.New("formato UUID inválido")
	}
	v = strings.ReplaceAll(v, "-", "")
	return fmt.Sprintf("%s-%s-%s-%s-%s", v[:8], v[8:12], v[12:16], v[16:20], v[20:]), nil
}

func backupFile(path, suffix string) error {
	if path == "" {
		return nil
	}
	in, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	backup := fmt.Sprintf("%s.%s-%s", path, suffix, time.Now().Format("20060102150405"))
	return os.WriteFile(backup, in, 0600)
}
func writeJSONAtomic(path string, data any) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func runCmd(ctx context.Context, timeout time.Duration, name string, args ...string) error {
	_, err := runCmdOutput(ctx, timeout, name, args...)
	return err
}
func runCmdOutput(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	b, err := cmd.CombinedOutput()
	return string(b), err
}
func runShell(ctx context.Context, timeout time.Duration, command string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/bash", "-lc", command)
	b, err := cmd.CombinedOutput()
	return string(b), err
}
func listeningPorts(ctx context.Context) map[int]bool {
	out, err := runCmdOutput(ctx, 5*time.Second, "ss", "-ltn")
	if err != nil {
		return nil
	}
	ports := map[int]bool{}
	for _, tok := range strings.Fields(strings.ReplaceAll(out, "*:", "0.0.0.0:")) {
		if !strings.Contains(tok, ":") {
			continue
		}
		tail := tok[strings.LastIndex(tok, ":")+1:]
		if n, err := strconv.Atoi(tail); err == nil {
			ports[n] = true
		}
	}
	return ports
}
func inboundPorts(cfg map[string]any) map[int]bool {
	res := map[int]bool{}
	inbounds, _ := cfg["inbounds"].([]any)
	for _, it := range inbounds {
		in, _ := it.(map[string]any)
		if n, err := strconv.Atoi(str(in["port"])); err == nil && n > 0 {
			res[n] = true
		}
	}
	return res
}
func intersects(a, b map[int]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}
func sortedKeys(m map[string]*Result) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var _ = sortedKeys
