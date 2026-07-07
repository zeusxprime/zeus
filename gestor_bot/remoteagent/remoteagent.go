package remoteagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"primecel-gestor/gestor_bot/accounts"
	"primecel-gestor/gestor_bot/checkuserdb"
	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/online"
	"primecel-gestor/gestor_bot/store"
)

const AgentName = "primecel-gestor-go"

type Services struct {
	Config   config.Config
	Store    *store.DB
	Accounts *accounts.Service
	Version  string
}

type Server struct {
	svc         Services
	mu          sync.Mutex
	onlinesAt   time.Time
	onlinesBody []byte
}

func NewServer(svc Services) *Server { return &Server{svc: svc} }

type SyncRequest struct {
	Token          string           `json:"token"`
	Action         string           `json:"action"`
	Args           []string         `json:"args"`
	Username       string           `json:"username"`
	Password       string           `json:"password"`
	Expiry         string           `json:"expiry"`
	ExpiresAt      string           `json:"expires_at"`
	Limit          int              `json:"limit"`
	UUID           string           `json:"uuid"`
	XrayEnabled    bool             `json:"xray_enabled"`
	OwnerID        int64            `json:"owner_telegram_id"`
	OwnerName      string           `json:"owner_name"`
	OwnerType      string           `json:"owner_type"`
	ClientWhatsApp string           `json:"client_whatsapp"`
	MonthlyValue   float64          `json:"monthly_value"`
	Payload        json.RawMessage  `json:"payload"`
	Resellers      json.RawMessage  `json:"resellers"`
	Accesses       []SnapshotAccess `json:"accesses"`
	Usernames      []string         `json:"usernames"`
}

type SnapshotAccess struct {
	Username        string  `json:"username"`
	Password        string  `json:"password"`
	Expiry          string  `json:"expiry"`
	ExpiresAt       string  `json:"expires_at"`
	Limit           int     `json:"limit"`
	UUID            string  `json:"uuid"`
	OwnerTelegramID int64   `json:"owner_telegram_id"`
	OwnerName       string  `json:"owner_name"`
	OwnerType       string  `json:"owner_type"`
	IsTrial         bool    `json:"is_trial"`
	ClientWhatsApp  string  `json:"client_whatsapp"`
	MonthlyValue    float64 `json:"monthly_value"`
	XrayEnabled     bool    `json:"xray_enabled"`
}

type SyncResponse struct {
	OK      bool     `json:"ok"`
	Agent   string   `json:"agent"`
	Version string   `json:"version"`
	Action  string   `json:"action,omitempty"`
	Output  string   `json:"output,omitempty"`
	Error   string   `json:"error,omitempty"`
	Applied int      `json:"applied,omitempty"`
	Failed  int      `json:"failed,omitempty"`
	Details []string `json:"details,omitempty"`
}

func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/onlines", s.handlePublicOnlines)
	mux.HandleFunc("/sync", s.handleSync)
	mux.HandleFunc("/online-summary", s.handleOnlineSummary)
	mux.HandleFunc("/online-count", s.handleOnlineCount)
	mux.HandleFunc("/details", s.handleDetails)
	mux.HandleFunc("/details/", s.handleDetails)
	return mux
}

func (s *Server) handlePublicOnlines(w http.ResponseWriter, r *http.Request) {
	// API pública de leitura, compatível com a ideia da porta 81 /onlines.
	// Não altera contas, não sincroniza, não reinicia serviços e não exige token.
	// Mantém cache curtíssimo para evitar sobrecarga quando o menu for atualizado em sequência.
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	now := time.Now()
	s.mu.Lock()
	if len(s.onlinesBody) > 0 && now.Sub(s.onlinesAt) <= 2*time.Second {
		body := append([]byte(nil), s.onlinesBody...)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
		return
	}
	s.mu.Unlock()

	sum, err := online.NewManager(s.svc.Config, s.svc.Store).AgentPublicSummary(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "agent": AgentName, "version": s.svc.Version, "error": err.Error()})
		return
	}
	users := make([]map[string]any, 0, len(sum.Users))
	for _, it := range sum.Users {
		if strings.TrimSpace(it.Username) == "" || it.Connections <= 0 {
			continue
		}
		users = append(users, map[string]any{
			"user":           it.Username,
			"username":       it.Username,
			"connections":    it.Connections,
			"limit":          it.Limit,
			"sources":        it.Sources,
			"modes":          it.Modes,
			"ip":             it.IP,
			"connected_time": it.ConnectedTime,
		})
	}
	totalConnections := 0
	for _, it := range sum.Users {
		if it.Connections > 0 {
			totalConnections += it.Connections
		}
	}
	body, _ := json.Marshal(map[string]any{
		"ok":      true,
		"agent":   AgentName,
		"version": s.svc.Version,
		"storage": "live",
		"count":   totalConnections,
		"users":   users,
		"time":    now.UTC().Format(time.RFC3339),
	})
	s.mu.Lock()
	s.onlinesAt = now
	s.onlinesBody = append([]byte(nil), body...)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"agent":             AgentName,
		"version":           s.svc.Version,
		"remote_agent_mode": true,
		"time":              time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, SyncResponse{OK: false, Agent: AgentName, Version: s.svc.Version, Error: "method not allowed"})
		return
	}
	var req SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, SyncResponse{OK: false, Agent: AgentName, Version: s.svc.Version, Error: "json inválido: " + err.Error()})
		return
	}
	if err := s.checkToken(r, req.Token); err != nil {
		writeJSON(w, http.StatusUnauthorized, SyncResponse{OK: false, Agent: AgentName, Version: s.svc.Version, Error: err.Error()})
		return
	}
	resp := s.dispatch(r.Context(), req)
	status := http.StatusOK
	if !resp.OK {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, resp)
}

func (s *Server) checkToken(r *http.Request, bodyToken string) error {
	expected := strings.TrimSpace(s.svc.Config.RemoteAgentToken)
	if expected == "" {
		return nil
	}
	got := strings.TrimSpace(r.Header.Get("X-Primecel-Agent-Token"))
	if got == "" {
		got = strings.TrimSpace(bodyToken)
	}
	if got == expected {
		return nil
	}
	return errors.New("token do agente inválido")
}

func (s *Server) dispatch(ctx context.Context, req SyncRequest) SyncResponse {
	action := strings.TrimSpace(req.Action)
	if action == "" {
		action = strings.TrimSpace(req.Username)
	}
	switch action {
	case "create", "restore":
		acc, err := s.accountFromRequest(req)
		if err != nil {
			return s.err(action, err)
		}
		if _, err := s.svc.Accounts.ApplyRemote(ctx, acc, agentActor(acc)); err != nil {
			return s.err(action, err)
		}
		return s.ok(action, "conta aplicada", 1, nil)
	case "remove", "delete":
		username := first(req.Username, argValue(req.Args, "--username"))
		if username == "" {
			return s.err(action, errors.New("username obrigatório"))
		}
		if err := s.svc.Accounts.Remove(ctx, model.Actor{Role: model.RoleAdmin, IsAdmin: true}, username); err != nil {
			return s.err(action, err)
		}
		return s.ok(action, "conta removida", 1, nil)
	case "renew":
		username := first(req.Username, argValue(req.Args, "--username"))
		days := intValue(first(strconv.Itoa(req.Limit), argValue(req.Args, "--days")), 30)
		if v := argValue(req.Args, "--days"); v != "" {
			days = intValue(v, 30)
		}
		if username == "" {
			return s.err(action, errors.New("username obrigatório"))
		}
		if _, err := s.svc.Accounts.Renew(ctx, model.Actor{Role: model.RoleAdmin, IsAdmin: true}, username, days); err != nil {
			return s.err(action, err)
		}
		return s.ok(action, "conta renovada", 1, nil)
	case "limit":
		username := first(req.Username, argValue(req.Args, "--username"))
		limit := req.Limit
		if v := argValue(req.Args, "--limit"); v != "" {
			limit = intValue(v, 1)
		}
		if username == "" {
			return s.err(action, errors.New("username obrigatório"))
		}
		if _, err := s.svc.Accounts.ChangeLimit(ctx, model.Actor{Role: model.RoleAdmin, IsAdmin: true}, username, limit); err != nil {
			return s.err(action, err)
		}
		return s.ok(action, "limite alterado", 1, nil)
	case "password":
		username := first(req.Username, argValue(req.Args, "--username"))
		pass := first(req.Password, argValue(req.Args, "--password"))
		if username == "" || pass == "" {
			return s.err(action, errors.New("username e password obrigatórios"))
		}
		if _, err := s.svc.Accounts.ChangePassword(ctx, model.Actor{Role: model.RoleAdmin, IsAdmin: true}, username, pass); err != nil {
			return s.err(action, err)
		}
		return s.ok(action, "senha alterada", 1, nil)
	case "state-snapshot":
		return s.applyStateSnapshot(ctx, req)
	case "server-reboot", "server-restart", "reboot", "restart":
		return s.rebootServer(ctx, action)
	case "deviceid-user":
		username := first(req.Username, argValue(req.Args, "--username"))
		if username == "" {
			return s.err(action, errors.New("username obrigatório"))
		}
		if err := s.svc.Store.ClearDevicesForUser(ctx, username, false); err != nil {
			return s.err(action, err)
		}
		if err := checkuserdb.ClearUser(ctx, s.svc.Config.CheckUserDBPath, username); err != nil {
			return s.err(action, err)
		}
		return s.ok(action, "aparelhos limpos", 1, nil)
	case "deviceid-users":
		usernames := uniqueDeviceUsernames(req.Usernames)
		if len(usernames) == 0 {
			return s.err(action, errors.New("nenhum usuário informado"))
		}
		for _, username := range usernames {
			if err := s.svc.Store.ClearDevicesForUser(ctx, username, false); err != nil {
				return s.err(action, err)
			}
		}
		if err := checkuserdb.ClearUsers(ctx, s.svc.Config.CheckUserDBPath, usernames); err != nil {
			return s.err(action, err)
		}
		return s.ok(action, "aparelhos do escopo limpos", len(usernames), nil)
	case "deviceid-scope":
		accs, err := s.svc.Store.ListAccounts(ctx, false)
		if err != nil {
			return s.err(action, err)
		}
		for _, a := range accs {
			_ = s.svc.Store.ClearDevicesForUser(ctx, a.Username, false)
		}
		if err := checkuserdb.ClearAll(ctx, s.svc.Config.CheckUserDBPath); err != nil {
			return s.err(action, err)
		}
		return s.ok(action, "aparelhos do escopo limpos", len(accs), nil)
	case "details":
		username := first(req.Username, argValue(req.Args, "--username"))
		if username == "" {
			return s.err(action, errors.New("username obrigatório"))
		}
		acc, err := s.svc.Store.FindAccount(ctx, username)
		if err != nil || acc == nil {
			return s.err(action, errors.New("conta não encontrada"))
		}
		b, _ := json.Marshal(acc)
		return s.ok(action, string(b), 1, nil)
	case "online-count", "online-summary":
		sum, err := online.NewManager(s.svc.Config, s.svc.Store).LocalSummary(ctx)
		if err != nil {
			return s.err(action, err)
		}
		b, _ := json.Marshal(sum)
		return s.ok(action, string(b), sum.Count, nil)
	default:
		return s.err(action, fmt.Errorf("ação não suportada pelo agente Go: %s", action))
	}
}

func (s *Server) rebootServer(ctx context.Context, action string) SyncResponse {
	rebootCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd := `nohup sh -c 'sleep 2; (systemctl reboot || /sbin/reboot || reboot)' >/dev/null 2>&1 & echo REBOOT_SCHEDULED`
	out, err := exec.CommandContext(rebootCtx, "bash", "-lc", cmd).CombinedOutput()
	if err != nil {
		return s.err(action, fmt.Errorf("falha ao agendar reinício: %s", strings.TrimSpace(string(out))))
	}
	return s.ok(action, "reinício agendado", 1, []string{strings.TrimSpace(string(out))})
}

func (s *Server) applyStateSnapshot(ctx context.Context, req SyncRequest) SyncResponse {
	var details []string
	applied, failed := 0, 0
	desired := make(map[string]bool, len(req.Accesses))
	desiredXray := make(map[string]bool, len(req.Accesses))
	for _, item := range req.Accesses {
		acc := accountFromSnapshot(item)
		key := strings.ToLower(strings.TrimSpace(acc.Username))
		if key != "" {
			desired[key] = true
			if item.XrayEnabled && strings.TrimSpace(item.UUID) != "" {
				desiredXray[key] = true
			}
		}
		_, err := s.svc.Accounts.ApplyRemote(ctx, acc, agentActor(acc))
		if err != nil {
			failed++
			details = append(details, acc.Username+": "+err.Error())
			continue
		}
		applied++
	}
	removed, removeFailed, pruneDetails := s.svc.Accounts.PruneRemoteToSnapshot(ctx, desired, desiredXray)
	details = append(details, pruneDetails...)
	failed += removeFailed
	if removed > 0 {
		applied += removed
	}
	if failed > 0 {
		return SyncResponse{OK: false, Agent: AgentName, Version: s.svc.Version, Action: "state-snapshot", Applied: applied, Failed: failed, Details: details, Error: "falhas ao aplicar snapshot"}
	}
	return s.ok("state-snapshot", "snapshot aplicado", applied, details)
}

func (s *Server) reconcileMissingSnapshotAccounts(ctx context.Context, desired map[string]bool, details *[]string) (int, int) {
	if desired == nil {
		desired = map[string]bool{}
	}
	local, err := s.svc.Store.ListAccounts(ctx, false)
	if err != nil {
		*details = append(*details, "reconciliação: "+err.Error())
		return 0, 1
	}
	removed, failed := 0, 0
	admin := model.Actor{Role: model.RoleAdmin, IsAdmin: true, Name: "Sync"}
	for _, acc := range local {
		if acc.DeletedAt != nil || acc.Status == "deleted" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(acc.Username))
		if key == "" || desired[key] {
			continue
		}
		if err := s.svc.Accounts.Remove(ctx, admin, acc.Username); err != nil {
			failed++
			*details = append(*details, acc.Username+": remover ausente no principal: "+err.Error())
			continue
		}
		removed++
	}
	return removed, failed
}

func (s *Server) handleOnlineSummary(w http.ResponseWriter, r *http.Request) { s.writeOnline(w, r) }
func (s *Server) handleOnlineCount(w http.ResponseWriter, r *http.Request)   { s.writeOnline(w, r) }
func (s *Server) writeOnline(w http.ResponseWriter, r *http.Request) {
	if err := s.checkToken(r, r.URL.Query().Get("token")); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sum, err := online.NewManager(s.svc.Config, s.svc.Store).LocalSummary(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sum)
}
func (s *Server) handleDetails(w http.ResponseWriter, r *http.Request) {
	if err := s.checkToken(r, r.URL.Query().Get("token")); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	username := strings.TrimPrefix(r.URL.Path, "/details/")
	if username == "" || username == "/details" {
		username = r.URL.Query().Get("username")
	}
	acc, err := s.svc.Store.FindAccount(r.Context(), username)
	if err != nil || acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "conta não encontrada"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc})
}

func (s *Server) accountFromRequest(req SyncRequest) (model.Account, error) {
	username := first(req.Username, argValue(req.Args, "--username"))
	password := first(req.Password, argValue(req.Args, "--password"))
	uuid := first(req.UUID, argValue(req.Args, "--uuid"))
	expiry := first(req.Expiry, argValue(req.Args, "--expiry"))
	expiresAt := first(req.ExpiresAt, argValue(req.Args, "--expires-at"))
	limit := req.Limit
	if v := argValue(req.Args, "--limit"); v != "" {
		limit = intValue(v, 1)
	}
	ownerID := req.OwnerID
	if v := argValue(req.Args, "--owner-id"); v != "" {
		ownerID = int64(intValue(v, 0))
	}
	ownerName := first(req.OwnerName, argValue(req.Args, "--owner-name"))
	ownerType := first(req.OwnerType, argValue(req.Args, "--owner-type"), "admin")
	clientWhatsApp := first(req.ClientWhatsApp, argValue(req.Args, "--client-whatsapp"))
	monthlyValue := req.MonthlyValue
	if v := argValue(req.Args, "--monthly-value"); v != "" {
		monthlyValue = floatValue(v, 0)
	}
	if username == "" || password == "" {
		return model.Account{}, errors.New("username/password obrigatórios")
	}
	if limit <= 0 {
		limit = 1
	}
	exp := parseExpiry(expiry, expiresAt)
	if expiry == "" {
		expiry = exp.AddDate(0, 0, -1).Format("2006-01-02")
	}
	return model.Account{Username: username, Password: password, UUID: uuid, LimitConnections: limit, ExpiryDate: expiry, ExpiresAt: exp, OwnerTelegramID: ownerID, OwnerName: ownerName, OwnerType: ownerType, Status: "active", XrayEnabled: req.XrayEnabled && uuid != "", ClientWhatsApp: clientWhatsApp, MonthlyValue: monthlyValue}, nil
}
func accountFromSnapshot(i SnapshotAccess) model.Account {
	exp := parseExpiry(i.Expiry, i.ExpiresAt)
	expiry := i.Expiry
	if expiry == "" {
		expiry = exp.AddDate(0, 0, -1).Format("2006-01-02")
	}
	limit := i.Limit
	if limit <= 0 {
		limit = 1
	}
	ownerType := i.OwnerType
	if ownerType == "" {
		ownerType = "admin"
	}
	return model.Account{Username: i.Username, Password: i.Password, UUID: i.UUID, LimitConnections: limit, ExpiryDate: expiry, ExpiresAt: exp, OwnerTelegramID: i.OwnerTelegramID, OwnerName: i.OwnerName, OwnerType: ownerType, Status: "active", IsTrial: i.IsTrial, XrayEnabled: i.XrayEnabled, ClientWhatsApp: i.ClientWhatsApp, MonthlyValue: i.MonthlyValue}
}
func parseExpiry(expiry, expiresAt string) time.Time {
	if t, ok := parseAnyTime(expiresAt); ok {
		return t
	}
	if t, ok := parseAnyTime(expiry); ok {
		return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.Local).UTC()
	}
	return time.Now().UTC().AddDate(0, 0, 30)
}
func parseAnyTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02", "02/01/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
func agentActor(a model.Account) model.Actor {
	role := model.ActorRole(a.OwnerType)
	if role == "" {
		role = model.RoleAdmin
	}
	return model.Actor{TelegramID: a.OwnerTelegramID, Name: a.OwnerName, Role: role, IsAdmin: true}
}
func first(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func argValue(args []string, key string) string {
	for i, v := range args {
		if v == key && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
func intValue(s string, d int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return d
	}
	return n
}
func floatValue(s string, d float64) float64 {
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", "."), 64)
	if err != nil {
		return d
	}
	return f
}
func (s *Server) ok(action, out string, applied int, details []string) SyncResponse {
	return SyncResponse{OK: true, Agent: AgentName, Version: s.svc.Version, Action: action, Output: out, Applied: applied, Details: details}
}
func (s *Server) err(action string, err error) SyncResponse {
	return SyncResponse{OK: false, Agent: AgentName, Version: s.svc.Version, Action: action, Error: err.Error()}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func uniqueDeviceUsernames(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}
