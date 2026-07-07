package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/legacy"
	"primecel-gestor/gestor_bot/mirrors"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
	remotesync "primecel-gestor/gestor_bot/sync"
	"primecel-gestor/gestor_bot/system"
	"primecel-gestor/gestor_bot/xray"
)

const Marker = "tg-access-bot-backup"

var portableFiles = []string{
	"payments.json",
	"backup_settings.json",
	"suspensions.json",
	"whatsapp_sessions.json",
}

type Manager struct {
	cfg     config.Config
	st      *store.DB
	mirrors *mirrors.Writer
	sys     system.Manager
	xray    *xray.Manager
}

func NewManager(cfg config.Config, st *store.DB, mw *mirrors.Writer, sys system.Manager, xm *xray.Manager) *Manager {
	return &Manager{cfg: cfg, st: st, mirrors: mw, sys: sys, xray: xm}
}

type CreateOptions struct {
	Output string
}

type CreateReport struct {
	Path          string   `json:"path"`
	Files         []string `json:"files"`
	Accounts      int      `json:"accounts"`
	Resellers     int      `json:"resellers"`
	Servers       int      `json:"servers"`
	CreatedAt     string   `json:"created_at"`
	SizeBytes     int64    `json:"size_bytes"`
	PrunedBackups []string `json:"pruned_backups,omitempty"`
}

type ImportOptions struct {
	File        string
	Clean       bool
	ConfirmText string
	SyncRemotes bool
}

type ImportReport struct {
	File           string        `json:"file"`
	Clean          bool          `json:"clean"`
	SyncedRemovals int           `json:"synced_removals"`
	LocalRemovals  int           `json:"local_removals"`
	XrayRemovals   int           `json:"xray_removals"`
	LegacyReport   legacy.Report `json:"legacy_report"`
	CopiedPortable []string      `json:"copied_portable,omitempty"`
	Warnings       []string      `json:"warnings,omitempty"`
}

func (m *Manager) Create(ctx context.Context, opt CreateOptions) (CreateReport, error) {
	if m.mirrors != nil {
		if err := m.mirrors.RefreshAll(ctx); err != nil {
			return CreateReport{}, err
		}
	}
	out := strings.TrimSpace(opt.Output)
	if out == "" {
		out = filepath.Join(m.cfg.DataDir, "backups", "backup-painel.tar.gz")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return CreateReport{}, err
	}
	tmp := out + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return CreateReport{}, err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return CreateReport{}, err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	rep := CreateReport{Path: out, CreatedAt: time.Now().UTC().Format(time.RFC3339)}

	accs, _ := m.st.ListAccounts(ctx, false)
	rs, _ := m.st.ListResellers(ctx)
	svs, _ := m.st.ListServers(ctx)
	rep.Accounts, rep.Resellers, rep.Servers = len(accs), len(rs), len(svs)
	manifest := map[string]any{
		"marker":     Marker,
		"schema":     "primecel-gestor-go-v1",
		"created_at": rep.CreatedAt,
		"host":       hostname(),
		"contents":   []string{"bot_data/users.jsonl", "bot_data/resellers.json", "bot_data/servers.conf", "system/usuarios.db", "credentials/bot_credentials.env", "credentials/internal_saved_tokens.env"},
	}
	if err := addJSON(tw, "manifest.json", manifest); err != nil {
		return CreateReport{}, closeErr(f, gz, tw, err)
	}
	rep.Files = append(rep.Files, "manifest.json")

	files := map[string]string{
		"bot_data/users.jsonl":    filepath.Join(m.cfg.DataDir, "users.jsonl"),
		"bot_data/resellers.json": filepath.Join(m.cfg.DataDir, "resellers.json"),
		"bot_data/servers.conf":   filepath.Join(m.cfg.DataDir, "servers.conf"),
		"system/usuarios.db":      m.cfg.UsuariosDBPath,
	}
	if err := m.writeServersConf(ctx); err != nil {
		return CreateReport{}, closeErr(f, gz, tw, err)
	}
	for arc, src := range files {
		ok, err := addFileIfExists(tw, arc, src)
		if err != nil {
			return CreateReport{}, closeErr(f, gz, tw, err)
		}
		if ok {
			rep.Files = append(rep.Files, arc)
		}
	}
	for _, name := range portableFiles {
		ok, err := addFileIfExists(tw, filepath.Join("bot_data", name), filepath.Join(m.cfg.DataDir, name))
		if err != nil {
			return CreateReport{}, closeErr(f, gz, tw, err)
		}
		if ok {
			rep.Files = append(rep.Files, filepath.Join("bot_data", name))
		}
	}
	botCred := buildBotCredentials(m.cfg)
	if err := addBytes(tw, "credentials/bot_credentials.env", []byte(botCred), 0600, time.Now()); err != nil {
		return CreateReport{}, closeErr(f, gz, tw, err)
	}
	rep.Files = append(rep.Files, "credentials/bot_credentials.env")
	tokenCred := buildTokenCredentials(m.cfg)
	if v, _ := m.st.GetSetting(ctx, "cloudflare_token"); strings.TrimSpace(v) != "" {
		tokenCred = "CLOUDFLARE_API_TOKEN=" + quoteEnv(strings.TrimSpace(v)) + "\n"
	}
	if err := addBytes(tw, "credentials/internal_saved_tokens.env", []byte(tokenCred), 0600, time.Now()); err != nil {
		return CreateReport{}, closeErr(f, gz, tw, err)
	}
	rep.Files = append(rep.Files, "credentials/internal_saved_tokens.env")

	if err := tw.Close(); err != nil {
		return CreateReport{}, closeErr(f, gz, nil, err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		return CreateReport{}, err
	}
	if err := f.Close(); err != nil {
		return CreateReport{}, err
	}
	if err := os.Rename(tmp, out); err != nil {
		return CreateReport{}, err
	}
	st, _ := os.Stat(out)
	if st != nil {
		rep.SizeBytes = st.Size()
	}
	pruned, _ := pruneBackups(filepath.Dir(out), filepath.Base(out))
	rep.PrunedBackups = pruned
	sort.Strings(rep.Files)
	return rep, nil
}

func (m *Manager) Import(ctx context.Context, opt ImportOptions) (ImportReport, error) {
	if strings.TrimSpace(opt.File) == "" {
		return ImportReport{}, errors.New("arquivo de backup obrigatório")
	}
	rep := ImportReport{File: opt.File, Clean: opt.Clean}
	if opt.Clean && strings.TrimSpace(opt.ConfirmText) != "IMPORTAR" {
		return rep, errors.New("importação limpa exige confirmação: --confirm IMPORTAR")
	}
	tmp, err := os.MkdirTemp("", "primecel-import-*")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(tmp)
	if err := extractSafe(opt.File, tmp); err != nil {
		return rep, err
	}
	if err := validateManifest(filepath.Join(tmp, "manifest.json")); err != nil {
		return rep, err
	}

	if opt.Clean {
		current, _ := m.st.ListAccounts(ctx, false)
		if opt.SyncRemotes {
			sm := remotesync.NewManager(m.cfg, m.st)
			for _, a := range current {
				_, _ = sm.SyncRemove(ctx, a.Username)
				rep.SyncedRemovals++
			}
		}
		for _, a := range current {
			if m.xray != nil && a.UUID != "" {
				if err := m.xray.RemoveAccount(ctx, a.Username, a.UUID, xray.ApplyOptions{SafeRestart: true, NoRestart: true}); err == nil {
					rep.XrayRemovals++
				}
			}
			if m.sys != nil {
				if err := m.sys.RemoveAccount(ctx, a.Username); err == nil {
					rep.LocalRemovals++
				}
			}
		}
		if err := m.cleanCurrent(ctx); err != nil {
			return rep, err
		}
	}

	srcDir := filepath.Join(tmp, "bot_data")
	if _, err := os.Stat(srcDir); err != nil {
		srcDir = tmp
	}
	for _, name := range portableFiles {
		if copied, err := copyIfExists(filepath.Join(srcDir, name), filepath.Join(m.cfg.DataDir, name), 0600); err != nil {
			return rep, err
		} else if copied {
			rep.CopiedPortable = append(rep.CopiedPortable, name)
		}
	}
	lr, err := legacy.ImportLegacy(ctx, legacy.ImportOptions{FromDir: srcDir, Config: m.cfg, Store: m.st, DryRun: false})
	if err != nil {
		return rep, err
	}
	rep.LegacyReport = lr
	if m.mirrors != nil {
		if err := m.mirrors.RefreshAll(ctx); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func (m *Manager) cleanCurrent(ctx context.Context) error {
	statements := []string{
		`DELETE FROM devices`, `DELETE FROM device_users`, `DELETE FROM account_events`, `DELETE FROM accounts`,
		`DELETE FROM reseller_credit_movements`, `DELETE FROM resellers`, `DELETE FROM servers`,
	}
	for _, q := range statements {
		if err := m.st.Exec(ctx, q); err != nil {
			return err
		}
	}
	paths := []string{filepath.Join(m.cfg.DataDir, "users.jsonl"), filepath.Join(m.cfg.DataDir, "resellers.json"), filepath.Join(m.cfg.DataDir, "servers.conf"), m.cfg.UsuariosDBPath}
	for _, p := range paths {
		_ = os.Remove(p)
	}
	return nil
}

func (m *Manager) writeServersConf(ctx context.Context) error {
	servers, err := m.st.ListServers(ctx)
	if err != nil {
		return err
	}
	path := filepath.Join(m.cfg.DataDir, "servers.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	for _, s := range servers {
		line := fmt.Sprintf("%s|%s|%s|%d||%s|%d|%s\n", first(s.Name, s.Host), first(s.SSHUser, "root"), s.Host, nonZero(s.SSHPort, 22), s.SSHPassword, nonZero(s.AgentPort, m.cfg.RemoteAgentPort, 8787), s.AgentToken)
		if _, err := io.WriteString(f, line); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func addJSON(tw *tar.Writer, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return addBytes(tw, name, append(b, '\n'), 0600, time.Now())
}
func addFileIfExists(tw *tar.Writer, arc, src string) (bool, error) {
	st, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if st.IsDir() {
		return false, nil
	}
	f, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer f.Close()
	hdr := &tar.Header{Name: filepath.ToSlash(arc), Mode: 0600, Size: st.Size(), ModTime: st.ModTime()}
	if err := tw.WriteHeader(hdr); err != nil {
		return false, err
	}
	_, err = io.Copy(tw, f)
	return true, err
}
func addBytes(tw *tar.Writer, name string, b []byte, mode int64, mod time.Time) error {
	if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
		return fmt.Errorf("nome tar inseguro: %s", name)
	}
	if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: mode, Size: int64(len(b)), ModTime: mod}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}
func closeErr(f *os.File, gz *gzip.Writer, tw *tar.Writer, err error) error {
	if tw != nil {
		_ = tw.Close()
	}
	if gz != nil {
		_ = gz.Close()
	}
	if f != nil {
		_ = f.Close()
	}
	return err
}

func extractSafe(path, dst string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	allowed := map[string]bool{"manifest.json": true}
	for _, n := range []string{"users.jsonl", "resellers.json", "servers.conf", "payments.json", "backup_settings.json", "suspensions.json", "whatsapp_sessions.json"} {
		allowed["bot_data/"+n] = true
		allowed[n] = true
	}
	allowed["system/usuarios.db"] = true
	allowed["credentials/bot_credentials.env"] = true
	allowed["credentials/internal_saved_tokens.env"] = true
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(hdr.Name))
		if name == "." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
			return fmt.Errorf("entrada tar insegura: %s", hdr.Name)
		}
		if !allowed[name] {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) && filepath.Clean(target) != filepath.Clean(dst) {
			return fmt.Errorf("path traversal: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}
func validateManifest(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return errors.New("manifest.json ausente no backup")
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return errors.New("manifest.json inválido")
	}
	if strings.TrimSpace(fmt.Sprint(m["marker"])) != Marker {
		return fmt.Errorf("backup incompatível: marker %v", m["marker"])
	}
	return nil
}
func copyIfExists(src, dst string, mode os.FileMode) (bool, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return false, err
	}
	return true, os.WriteFile(dst, b, mode)
}
func pruneBackups(dir, keep string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		name := e.Name()
		if name == keep {
			continue
		}
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".zip") || strings.HasPrefix(name, "primecel_bot_backup_") {
			p := filepath.Join(dir, name)
			if err := os.Remove(p); err == nil {
				removed = append(removed, p)
			}
		}
	}
	return removed, nil
}
func buildBotCredentials(cfg config.Config) string {
	return "BOT_TOKEN=" + quoteEnv(cfg.BotToken) + "\nADMIN_IDS=" + quoteEnv(int64s(cfg.AdminIDs)) + "\nADMIN_DISPLAY_NAME=" + quoteEnv(cfg.AdminDisplayName) + "\nWHATSAPP_ADMIN_NUMBERS=" + quoteEnv(strings.Join(cfg.WhatsAppAdminNumbers, ",")) + "\n"
}
func buildTokenCredentials(cfg config.Config) string {
	return "CLOUDFLARE_API_TOKEN=" + quoteEnv(cfg.CloudflareToken) + "\n"
}
func quoteEnv(s string) string { return strings.ReplaceAll(s, "\n", "") }
func int64s(v []int64) string {
	parts := make([]string, 0, len(v))
	for _, n := range v {
		parts = append(parts, fmt.Sprint(n))
	}
	return strings.Join(parts, ",")
}
func hostname() string { h, _ := os.Hostname(); return h }
func first(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func nonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

var _ model.Account
