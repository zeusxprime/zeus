package system

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
)

type Manager interface {
	ApplyAccount(ctx context.Context, a model.Account) error
	ChangePassword(ctx context.Context, username, password string) error
	ChangeLimit(ctx context.Context, username string, limit int) error
	RemoveAccount(ctx context.Context, username string) error
	UserExists(ctx context.Context, username string) bool
	UpsertUsuariosDB(ctx context.Context, a model.Account) error
	RemoveUsuariosDB(ctx context.Context, username string) error
}

type LocalManager struct {
	cfg config.Config
	dry bool
}

func NewLocalManager(cfg config.Config) *LocalManager {
	return &LocalManager{cfg: cfg, dry: os.Geteuid() != 0}
}

var safeUser = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]{3,11}$`)

func (m *LocalManager) ApplyAccount(ctx context.Context, a model.Account) error {
	if !safeUser.MatchString(a.Username) {
		return fmt.Errorf("usuário inválido para Linux: %s", a.Username)
	}
	if m.cfg.PrincipalManagerOnly {
		return m.UpsertUsuariosDB(ctx, a)
	}
	accessExpiry := AccessExpiryDate(a)
	if !m.UserExists(ctx, a.Username) {
		if err := m.createLinuxUser(ctx, a.Username, accessExpiry); err != nil {
			return err
		}
	}
	if err := m.ChangePassword(ctx, a.Username, a.Password); err != nil {
		return err
	}
	if accessExpiry != "" {
		_ = m.run(ctx, 10*time.Second, "usermod", "-e", accessExpiry, a.Username)
		_ = m.run(ctx, 10*time.Second, "chage", "-E", accessExpiry, a.Username)
	}
	return m.UpsertUsuariosDB(ctx, a)
}

func (m *LocalManager) createLinuxUser(ctx context.Context, username, expiry string) error {
	attempts := [][]string{
		{"useradd", "-M", "-s", m.cfg.SSHShell, username},
		{"useradd", "--badname", "-M", "-s", m.cfg.SSHShell, username},
	}
	if expiry != "" {
		attempts = append([][]string{
			{"useradd", "-M", "-s", m.cfg.SSHShell, "-e", expiry, username},
			{"useradd", "--badname", "-M", "-s", m.cfg.SSHShell, "-e", expiry, username},
		}, attempts...)
	}
	var last error
	for _, cmd := range attempts {
		err := m.run(ctx, 10*time.Second, cmd[0], cmd[1:]...)
		if err == nil || strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return nil
		}
		last = err
	}
	return last
}

// AccessExpiryDate é a data usada por Linux/chage e usuarios.db.
// Teste continua com validade real por hora em Account.ExpiresAt.
// Para evitar login inválido imediato no app, o backend de acesso por data recebe uma data segura.
func AccessExpiryDate(a model.Account) string {
	expiry := strings.TrimSpace(a.ExpiryDate)
	if a.IsTrial && !a.ExpiresAt.IsZero() && a.ExpiresAt.After(time.Now().UTC()) {
		return a.ExpiresAt.Local().AddDate(0, 0, 1).Format("2006-01-02")
	}
	return expiry
}

func (m *LocalManager) ChangePassword(ctx context.Context, username, password string) error {
	if m.cfg.PrincipalManagerOnly {
		return nil
	}
	cmd := exec.CommandContext(ctx, "chpasswd")
	cmd.Stdin = strings.NewReader(username + ":" + password + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("chpasswd: %v: %s", err, string(out))
	}
	return nil
}
func (m *LocalManager) ChangeLimit(ctx context.Context, username string, limit int) error { return nil }
func (m *LocalManager) RemoveAccount(ctx context.Context, username string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil
	}
	if !m.cfg.PrincipalManagerOnly {
		// Bloqueio em camadas: mata sessões, bloqueia senha, expira e remove o usuário Linux.
		// Assim evita conta vencida continuar conectando por sessão SSH/Dropbear aberta.
		_ = m.run(ctx, 5*time.Second, "pkill", "-KILL", "-u", username)
		if m.UserExists(ctx, username) {
			_ = m.run(ctx, 8*time.Second, "usermod", "-L", username)
			_ = m.run(ctx, 8*time.Second, "usermod", "-e", "1970-01-02", username)
			_ = m.run(ctx, 8*time.Second, "chage", "-E", "1970-01-02", username)
			_ = m.run(ctx, 15*time.Second, "userdel", "-f", username)
		}
	}
	return m.RemoveUsuariosDB(ctx, username)
}
func (m *LocalManager) UserExists(ctx context.Context, username string) bool {
	err := exec.CommandContext(ctx, "id", "-u", username).Run()
	return err == nil
}
func (m *LocalManager) run(ctx context.Context, timeout time.Duration, name string, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, string(out))
	}
	return nil
}

type UsuariosEntry struct {
	Username, Password string
	Limit              int
	ExpiryDate         string
}

func (m *LocalManager) UpsertUsuariosDB(ctx context.Context, a model.Account) error {
	var last error
	for _, path := range usuariosDBPaths(m.cfg.UsuariosDBPath) {
		entries, _ := ReadUsuariosDB(path)
		found := false
		accessExpiry := AccessExpiryDate(a)
		for i := range entries {
			if strings.EqualFold(entries[i].Username, a.Username) {
				entries[i] = UsuariosEntry{a.Username, a.Password, a.LimitConnections, accessExpiry}
				found = true
			}
		}
		if !found {
			entries = append(entries, UsuariosEntry{a.Username, a.Password, a.LimitConnections, accessExpiry})
		}
		if err := WriteUsuariosDB(path, entries); err != nil {
			last = err
		}
	}
	return last
}
func (m *LocalManager) RemoveUsuariosDB(ctx context.Context, username string) error {
	var last error
	for _, path := range usuariosDBPaths(m.cfg.UsuariosDBPath) {
		entries, _ := ReadUsuariosDB(path)
		out := entries[:0]
		for _, e := range entries {
			if !strings.EqualFold(e.Username, username) {
				out = append(out, e)
			}
		}
		if err := WriteUsuariosDB(path, out); err != nil {
			last = err
		}
	}
	return last
}
func usuariosDBPaths(primary string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, path := range []string{primary, "/root/usuarios.db", "/etc/primecel-gestor/usuarios.db", "/etc/tg-access-bot/usuarios.db"} {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}
func ReadUsuariosDB(path string) ([]UsuariosEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []UsuariosEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) >= 4 {
			out = append(out, UsuariosEntry{Username: parts[0], Password: parts[1], Limit: atoi(parts[2]), ExpiryDate: parts[3]})
		}
	}
	return out, sc.Err()
}
func WriteUsuariosDB(path string, entries []UsuariosEntry) error {
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Username) < strings.ToLower(entries[j].Username)
	})
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range entries {
		fmt.Fprintf(w, "%s %s %d %s\n", e.Username, e.Password, e.Limit, e.ExpiryDate)
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	_ = f.Sync()
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func atoi(s string) int { var n int; fmt.Sscanf(s, "%d", &n); return n }
