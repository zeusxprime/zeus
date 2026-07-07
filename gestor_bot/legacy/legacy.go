package legacy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
	"primecel-gestor/gestor_bot/system"
)

const DefaultLegacyDir = "/etc/tg-access-bot"

type ImportOptions struct {
	FromDir string
	Config  config.Config
	Store   *store.DB
	DryRun  bool
}
type Report struct {
	From              string   `json:"from"`
	DryRun            bool     `json:"dry_run"`
	AccountsDetected  int      `json:"accounts_detected"`
	AccountsImported  int      `json:"accounts_imported"`
	DeletedAccounts   int      `json:"deleted_accounts"`
	ResellersDetected int      `json:"resellers_detected"`
	ResellersImported int      `json:"resellers_imported"`
	ServersDetected   int      `json:"servers_detected"`
	Conflicts         []string `json:"conflicts,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
}

type legacyAcc struct {
	Username, Password, UUID, ExpiryDate, OwnerName, OwnerType, ClientWhatsApp string
	MonthlyValue                                                               float64
	Limit                                                                      int
	OwnerID                                                                    int64
	ExpiresAt                                                                  time.Time
	IsTrial, Deleted, CreditCounted                                            bool
	UpdatedAt                                                                  time.Time
}

func ImportLegacy(ctx context.Context, opt ImportOptions) (Report, error) {
	if opt.FromDir == "" {
		opt.FromDir = DefaultLegacyDir
	}
	rep := Report{From: opt.FromDir, DryRun: opt.DryRun}
	accs, warns := readUsersJSONL(filepath.Join(opt.FromDir, "users.jsonl"))
	rep.Warnings = append(rep.Warnings, warns...)
	mergeUsuariosDB(accs, opt.Config.UsuariosDBPath)
	rep.AccountsDetected = len(accs)
	for _, a := range accs {
		if a.Deleted {
			rep.DeletedAccounts++
			continue
		}
		if a.Username == "" || a.Password == "" {
			rep.Conflicts = append(rep.Conflicts, "conta ignorada sem usuário/senha: "+a.Username)
			continue
		}
		if !opt.DryRun {
			ma := model.Account{Username: a.Username, Password: a.Password, UUID: a.UUID, LimitConnections: defaultInt(a.Limit, 1), ExpiresAt: defaultTime(a.ExpiresAt, expiryToExpiresAt(a.ExpiryDate)), ExpiryDate: firstNonEmpty(a.ExpiryDate, time.Now().AddDate(0, 0, 30).Format("2006-01-02")), OwnerTelegramID: a.OwnerID, OwnerName: a.OwnerName, OwnerType: firstNonEmpty(a.OwnerType, "admin"), Status: "active", IsTrial: a.IsTrial, XrayEnabled: a.UUID != "", CreditCounted: a.CreditCounted, ClientWhatsApp: digits(a.ClientWhatsApp), MonthlyValue: a.MonthlyValue, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			if err := opt.Store.UpsertAccount(ctx, ma); err != nil {
				return rep, err
			}
			_ = opt.Store.UpsertDeviceUser(ctx, ma.Username, ma.UUID, ma.LimitConnections)
		}
		rep.AccountsImported++
	}
	rs, rw := readResellers(filepath.Join(opt.FromDir, "resellers.json"))
	rep.Warnings = append(rep.Warnings, rw...)
	rep.ResellersDetected = len(rs)
	for _, r := range rs {
		if !opt.DryRun {
			if err := opt.Store.UpsertReseller(ctx, r); err != nil {
				rep.Conflicts = append(rep.Conflicts, "revenda não importada "+r.Name+": "+err.Error())
				continue
			}
		}
		rep.ResellersImported++
	}
	servers, _ := readServers(filepath.Join(opt.FromDir, "servers.conf"), opt.Config)
	rep.ServersDetected = len(servers)
	if !opt.DryRun {
		for _, srv := range servers {
			if err := opt.Store.UpsertServer(ctx, srv); err != nil {
				rep.Conflicts = append(rep.Conflicts, "servidor não importado "+srv.Host+": "+err.Error())
			}
		}
		_ = system.WriteUsuariosDB(opt.Config.UsuariosDBPath, accountsToUsuarios(accs))
	}
	return rep, nil
}

func readUsersJSONL(path string) (map[string]legacyAcc, []string) {
	out := map[string]legacyAcc{}
	var warns []string
	f, err := os.Open(path)
	if err != nil {
		return out, []string{"users.jsonl não encontrado: " + path}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		txt := strings.TrimSpace(sc.Text())
		if txt == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(txt), &m); err != nil {
			warns = append(warns, "linha users.jsonl inválida "+strconv.Itoa(line))
			continue
		}
		u := firstMap(m, "username", "user", "usuario", "login")
		if u == "" {
			continue
		}
		acc := out[strings.ToLower(u)]
		acc.Username = u
		acc.Password = firstNonEmpty(firstMap(m, "password", "senha", "pass"), acc.Password)
		acc.UUID = firstNonEmpty(firstMap(m, "uuid", "user_uuid", "xray_uuid", "id_uuid"), acc.UUID)
		acc.ExpiryDate = firstNonEmpty(normDate(firstMap(m, "expiry", "validity", "validade", "expiration_date")), acc.ExpiryDate)
		if ex := parseAnyTime(firstMap(m, "expires_at", "expiry_at")); !ex.IsZero() {
			acc.ExpiresAt = ex
		}
		acc.Limit = defaultInt(intVal(m, "limit", "limit_connections", "limite"), acc.Limit)
		acc.OwnerID = defaultInt64(int64Val(m, "owner_telegram_id", "owner_id", "telegram_id"), acc.OwnerID)
		acc.OwnerName = firstNonEmpty(firstMap(m, "owner_name", "vendedor", "reseller_name"), acc.OwnerName)
		acc.OwnerType = firstNonEmpty(firstMap(m, "owner_type", "tipo_dono"), acc.OwnerType)
		acc.ClientWhatsApp = firstNonEmpty(firstMap(m, "client_whatsapp", "whatsapp_phone", "whatsapp"), acc.ClientWhatsApp)
		acc.MonthlyValue = defaultFloat(floatVal(m, "monthly_value", "valor_mensal", "monthly_price", "valor"), acc.MonthlyValue)
		acc.IsTrial = boolVal(m, "trial", "is_trial", "teste")
		if boolVal(m, "deleted", "removed") || strings.Contains(strings.ToLower(firstMap(m, "action", "event", "event_type")), "delete") || strings.Contains(strings.ToLower(firstMap(m, "action", "event", "event_type")), "remove") {
			acc.Deleted = true
		} else {
			acc.Deleted = false
		}
		if _, ok := m["credit_counted"]; ok {
			acc.CreditCounted = boolVal(m, "credit_counted")
		} else if acc.OwnerType != "" && acc.OwnerType != "admin" {
			acc.CreditCounted = true
		}
		acc.UpdatedAt = time.Now().UTC()
		out[strings.ToLower(u)] = acc
	}
	return out, warns
}
func mergeUsuariosDB(accs map[string]legacyAcc, path string) {
	entries, err := system.ReadUsuariosDB(path)
	if err != nil {
		return
	}
	for _, e := range entries {
		key := strings.ToLower(e.Username)
		a := accs[key]
		if a.Username == "" {
			a.Username = e.Username
		}
		if a.Password == "" {
			a.Password = e.Password
		}
		if a.Limit == 0 {
			a.Limit = e.Limit
		}
		if a.ExpiryDate == "" {
			a.ExpiryDate = e.ExpiryDate
		}
		accs[key] = a
	}
}
func accountsToUsuarios(accs map[string]legacyAcc) []system.UsuariosEntry {
	var out []system.UsuariosEntry
	for _, a := range accs {
		if !a.Deleted && a.Username != "" {
			out = append(out, system.UsuariosEntry{Username: a.Username, Password: a.Password, Limit: defaultInt(a.Limit, 1), ExpiryDate: a.ExpiryDate})
		}
	}
	return out
}

func readResellers(path string) ([]model.Reseller, []string) {
	var warns []string
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, []string{"resellers.json não encontrado: " + path}
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, []string{"resellers.json inválido"}
	}
	now := time.Now().UTC()
	var out []model.Reseller
	switch v := raw.(type) {
	case map[string]any:
		for k, val := range v {
			if m, ok := val.(map[string]any); ok {
				id := int64Val(map[string]any{"id": k}, "id")
				if id == 0 {
					id = int64Val(m, "telegram_id", "id")
				}
				r := resellerFromMap(id, m, now)
				out = append(out, r)
			}
		}
	case []any:
		for _, val := range v {
			if m, ok := val.(map[string]any); ok {
				id := int64Val(m, "telegram_id", "id")
				out = append(out, resellerFromMap(id, m, now))
			}
		}
	}
	return out, warns
}
func resellerFromMap(id int64, m map[string]any, now time.Time) model.Reseller {
	parent := int64Val(m, "parent_telegram_id", "parent_id")
	level := intVal(m, "level")
	if level == 0 && parent != 0 {
		level = 1
	}
	exp := parseAnyTime(firstMap(m, "expires_at", "valid_until", "validade"))
	if exp.IsZero() {
		exp = time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC)
	}
	return model.Reseller{TelegramID: id, Name: firstNonEmpty(firstMap(m, "name", "username", "user"), strconv.FormatInt(id, 10)), WhatsAppPhone: digits(firstMap(m, "whatsapp_phone", "whatsapp", "phone")), Password: firstMap(m, "password", "senha"), Credits: defaultInt(intVal(m, "credits", "access_limit", "limite"), 0), Active: !hasFalse(m, "active", "enabled"), MaxDays: defaultInt(intVal(m, "max_days"), 3650), MaxLimit: defaultInt(intVal(m, "max_limit"), 999), AllowXray: !hasFalse(m, "allow_xray", "can_create_xray"), AllowSubReseller: level == 0 && !hasFalse(m, "allow_subreseller", "can_create_subresellers"), ExpiresAt: exp, ParentTelegramID: parent, Level: level, MonthlyPrice: floatVal(m, "monthly_price", "valor_mensal"), PendingMonthlyPrice: floatVal(m, "pending_monthly_price"), PendingMonthlyDifference: floatVal(m, "pending_monthly_difference"), CreatedAt: now, UpdatedAt: now}
}
func readServers(path string, cfg config.Config) ([]model.Server, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []model.Server
	now := time.Now().UTC()
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		srv, ok := parseServerLine(l, cfg, now)
		if !ok || srv.Host == "" {
			continue
		}
		key := strings.ToLower(srv.SSHUser + "@" + srv.Host + ":" + strconv.Itoa(srv.SSHPort))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, srv)
	}
	return out, nil
}

func parseServerLine(line string, cfg config.Config, now time.Time) (model.Server, bool) {
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	server := model.Server{SSHUser: "root", SSHPort: 22, AgentPort: defaultInt(cfg.RemoteAgentPort, 8787), AgentToken: cfg.RemoteAgentToken, Enabled: true, CreatedAt: now, UpdatedAt: now}
	parseLogin := func(login string) (string, string) {
		if strings.Contains(login, "@") {
			p := strings.SplitN(login, "@", 2)
			return firstNonEmpty(p[0], "root"), p[1]
		}
		return "root", login
	}
	switch {
	case len(parts) >= 8:
		server.Name, server.SSHUser, server.Host = parts[0], firstNonEmpty(parts[1], "root"), parts[2]
		server.SSHPort = defaultInt(parseIntLoose(parts[3]), 22)
		server.SSHPassword = parts[5]
		server.AgentPort = defaultInt(parseIntLoose(parts[6]), defaultInt(cfg.RemoteAgentPort, 8787))
		server.AgentToken = firstNonEmpty(parts[7], cfg.RemoteAgentToken)
	case len(parts) >= 6:
		server.Name, server.SSHUser, server.Host = parts[0], firstNonEmpty(parts[1], "root"), parts[2]
		server.SSHPort = defaultInt(parseIntLoose(parts[3]), 22)
		server.SSHPassword = parts[5]
	case len(parts) >= 5:
		server.Name, server.SSHUser, server.Host = parts[0], firstNonEmpty(parts[1], "root"), parts[2]
		server.SSHPort = defaultInt(parseIntLoose(parts[3]), 22)
	case len(parts) == 4:
		server.Name = parts[0]
		server.SSHUser, server.Host = parseLogin(parts[1])
		server.SSHPort = defaultInt(parseIntLoose(parts[2]), 22)
	case len(parts) == 3:
		server.Name = parts[0]
		server.SSHUser, server.Host = parseLogin(parts[1])
		server.SSHPort = defaultInt(parseIntLoose(parts[2]), 22)
	case len(parts) == 2:
		server.Name = parts[0]
		server.SSHUser, server.Host = parseLogin(parts[1])
	default:
		return server, false
	}
	server.Name = firstNonEmpty(server.Name, server.Host)
	return server, true
}

func parseIntLoose(s string) int { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }

func firstMap(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			s := strings.TrimSpace(strings.Trim(fmt.Sprint(v), "\""))
			if s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}
func intVal(m map[string]any, keys ...string) int { return int(int64Val(m, keys...)) }
func int64Val(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch x := v.(type) {
			case float64:
				return int64(x)
			case int:
				return int64(x)
			case int64:
				return x
			case string:
				n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
				return n
			}
		}
	}
	return 0
}
func floatVal(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch x := v.(type) {
			case float64:
				return x
			case int:
				return float64(x)
			case string:
				f, _ := strconv.ParseFloat(strings.ReplaceAll(x, ",", "."), 64)
				return f
			}
		}
	}
	return 0
}
func boolVal(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch x := v.(type) {
			case bool:
				return x
			case float64:
				return x != 0
			case string:
				x = strings.ToLower(strings.TrimSpace(x))
				return x == "1" || x == "true" || x == "sim" || x == "yes" || x == "ativo"
			}
		}
	}
	return false
}
func hasFalse(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch x := v.(type) {
			case bool:
				return !x
			case float64:
				return x == 0
			case string:
				x = strings.ToLower(strings.TrimSpace(x))
				return x == "0" || x == "false" || x == "nao" || x == "não" || x == "no" || x == "bloqueado"
			}
		}
	}
	return false
}
func parseAnyTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	layouts := []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02", "02/01/2006", "02-01-2006"}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
func normDate(s string) string {
	if t := parseAnyTime(s); !t.IsZero() {
		return t.Format("2006-01-02")
	}
	return s
}
func expiryToExpiresAt(date string) time.Time {
	if date == "" {
		return time.Now().UTC().AddDate(0, 0, 30)
	}
	t := parseAnyTime(date)
	if t.IsZero() {
		return time.Now().UTC().AddDate(0, 0, 30)
	}
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.Local).UTC()
}
func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
func defaultInt(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}
func defaultInt64(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}
func defaultFloat(a, b float64) float64 {
	if a != 0 {
		return a
	}
	return b
}
func defaultTime(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}
func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
