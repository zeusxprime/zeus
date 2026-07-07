package mirrors

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
	"primecel-gestor/gestor_bot/system"
)

type Writer struct {
	cfg config.Config
	st  *store.DB
}

func NewWriter(cfg config.Config, st *store.DB) *Writer { return &Writer{cfg: cfg, st: st} }
func (w *Writer) RefreshAll(ctx context.Context) error {
	if err := w.WriteUsersJSONL(ctx); err != nil {
		return err
	}
	if err := w.WriteCheckUserExpirations(ctx); err != nil {
		return err
	}
	if err := w.WriteResellersJSON(ctx); err != nil {
		return err
	}
	if err := w.WriteServersConf(ctx); err != nil {
		return err
	}
	return w.WriteUsuariosDB(ctx)
}
func (w *Writer) WriteUsersJSONL(ctx context.Context) error {
	accs, err := w.st.ListAccounts(ctx, true)
	if err != nil {
		return err
	}
	sort.Slice(accs, func(i, j int) bool { return accs[i].UpdatedAt.Before(accs[j].UpdatedAt) })
	paths := []string{filepath.Join(w.cfg.DataDir, "users.jsonl"), "/etc/tg-access-bot/users.jsonl"}
	for _, path := range uniqueMirrorPaths(paths) {
		if err := w.writeUsersJSONLPath(path, accs); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) writeUsersJSONLPath(path string, accs []model.Account) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	for _, a := range accs {
		ev := map[string]any{"username": a.Username, "password": a.Password, "uuid": a.UUID, "limit": a.LimitConnections, "expiry": a.ExpiryDate, "expires_at": a.ExpiresAt.Format(time.RFC3339), "owner_telegram_id": a.OwnerTelegramID, "owner_name": a.OwnerName, "owner_type": a.OwnerType, "trial": a.IsTrial, "is_trial": a.IsTrial, "credit_counted": a.CreditCounted, "client_whatsapp": a.ClientWhatsApp, "monthly_value": a.MonthlyValue, "status": a.Status, "action": "upsert", "deleted": a.DeletedAt != nil || strings.EqualFold(a.Status, "deleted"), "updated_at": a.UpdatedAt.Format(time.RFC3339)}
		b, _ := json.Marshal(ev)
		if _, err := bw.Write(b); err != nil {
			_ = f.Close()
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (w *Writer) WriteCheckUserExpirations(ctx context.Context) error {
	accs, err := w.st.ListAccounts(ctx, false)
	if err != nil {
		return err
	}
	paths := []string{
		"/etc/DragonTeste/expirations.db",
		"/root/usuarios_expiracao.db",
		"/root/checkuser_expirations.db",
	}
	if custom := strings.TrimSpace(os.Getenv("CHECKUSER_EXACT_EXPIRATIONS_DB")); custom != "" {
		paths = append([]string{custom}, paths...)
	}
	entries := map[string]string{}
	for _, a := range accs {
		if a.DeletedAt != nil || a.Status == "deleted" || strings.TrimSpace(a.Username) == "" || a.ExpiresAt.IsZero() {
			continue
		}
		entries[a.Username] = a.ExpiresAt.UTC().Format(time.RFC3339)
	}
	for _, path := range uniqueMirrorPaths(paths) {
		if err := writeExpirationDB(path, entries); err != nil {
			return err
		}
	}
	for _, dir := range []string{"/etc/DragonTeste/expirations", "/root/usuarios_expiracao"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for username, exact := range entries {
			for _, name := range []string{username, strings.ToLower(username)} {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(exact+"\n"), 0o644); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func writeExpirationDB(path string, entries map[string]string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	current := []string{}
	seen := map[string]bool{}
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) > 0 {
				key := strings.ToLower(fields[0])
				if _, ok := entries[fields[0]]; ok {
					continue
				}
				matched := false
				for username := range entries {
					if strings.EqualFold(username, fields[0]) {
						matched = true
						break
					}
				}
				if matched || seen[key] {
					continue
				}
				seen[key] = true
			}
			current = append(current, line)
		}
	}
	for username, exact := range entries {
		current = append(current, username+" "+exact)
	}
	return os.WriteFile(path, []byte(strings.Join(current, "\n")+"\n"), 0o644)
}

func uniqueMirrorPaths(paths []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

func (w *Writer) WriteResellersJSON(ctx context.Context) error {
	rs, err := w.st.ListResellers(ctx)
	if err != nil {
		return err
	}
	m := map[string]any{}
	for _, r := range rs {
		m[itoa(r.TelegramID)] = r
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	paths := []string{filepath.Join(w.cfg.DataDir, "resellers.json"), "/etc/tg-access-bot/resellers.json"}
	for _, path := range uniqueMirrorPaths(paths) {
		if err := writeJSONFile(path, b, 0600); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONFile(path string, b []byte, perm os.FileMode) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (w *Writer) WriteServersConf(ctx context.Context) error {
	servers, err := w.st.ListServers(ctx)
	if err != nil {
		return err
	}
	path := filepath.Join(w.cfg.DataDir, "servers.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	wrt := bufio.NewWriter(f)
	for _, srv := range servers {
		name := srv.Name
		if name == "" {
			name = srv.Host
		}
		user := srv.SSHUser
		if user == "" {
			user = "root"
		}
		port := srv.SSHPort
		if port == 0 {
			port = 22
		}
		agentPort := srv.AgentPort
		if agentPort == 0 {
			agentPort = w.cfg.RemoteAgentPort
		}
		if agentPort == 0 {
			agentPort = 8787
		}
		_, _ = fmt.Fprintf(wrt, "%s|%s|%s|%d||%s|%d|%s\n", name, user, srv.Host, port, srv.SSHPassword, agentPort, srv.AgentToken)
	}
	if err := wrt.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (w *Writer) WriteUsuariosDB(ctx context.Context) error {
	accs, err := w.st.ListAccounts(ctx, false)
	if err != nil {
		return err
	}
	var entries []system.UsuariosEntry
	now := time.Now().UTC()
	for _, a := range accs {
		if a.Status == "active" && a.ExpiresAt.After(now) {
			entries = append(entries, system.UsuariosEntry{Username: a.Username, Password: a.Password, Limit: a.LimitConnections, ExpiryDate: system.AccessExpiryDate(a)})
		}
	}
	return system.WriteUsuariosDB(w.cfg.UsuariosDBPath, entries)
}
func (w *Writer) UpsertAccount(ctx context.Context, a model.Account) error {
	_ = a
	return w.RefreshAll(ctx)
}
func (w *Writer) RemoveAccount(ctx context.Context, username string) error {
	_ = username
	return w.RefreshAll(ctx)
}
func itoa(n int64) string { return strconvFormat(n) }
func strconvFormat(n int64) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 20)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
