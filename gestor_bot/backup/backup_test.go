package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/mirrors"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
	"primecel-gestor/gestor_bot/system"
	"primecel-gestor/gestor_bot/xray"
)

func TestCreateAndCleanImport(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Config{DataDir: filepath.Join(dir, "data"), DBPath: filepath.Join(dir, "data", "gestor.db"), UsuariosDBPath: filepath.Join(dir, "usuarios.db"), RemoteAgentPort: 8787}
	cfg.Xray.EnableDirectConfig = false
	cfg.Xray.EnableDragonCorePG = false
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.UpsertAccount(ctx, model.Account{Username: "Cliente1", Password: "12345", LimitConnections: 1, ExpiresAt: now.AddDate(0, 0, 30), ExpiryDate: now.AddDate(0, 0, 30).Format("2006-01-02"), OwnerType: "admin", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertServer(ctx, model.Server{Name: "Sv1", Host: "127.0.0.1", SSHUser: "root", SSHPort: 22, AgentPort: 8787, AgentToken: "tok", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	mw := mirrors.NewWriter(cfg, st)
	mgr := NewManager(cfg, st, mw, system.NewLocalManager(cfg), xray.NewManager(cfg))
	rep, err := mgr.Create(ctx, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Path == "" || rep.Accounts != 1 || rep.Servers != 1 {
		t.Fatalf("bad create report: %+v", rep)
	}
	if _, err := os.Stat(rep.Path); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(ctx, model.Account{Username: "Outro1", Password: "22222", LimitConnections: 1, ExpiresAt: now.AddDate(0, 0, 30), ExpiryDate: now.AddDate(0, 0, 30).Format("2006-01-02"), OwnerType: "admin", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	imp, err := mgr.Import(ctx, ImportOptions{File: rep.Path, Clean: true, ConfirmText: "IMPORTAR"})
	if err != nil {
		t.Fatal(err)
	}
	if imp.LegacyReport.AccountsImported != 1 {
		t.Fatalf("bad import report: %+v", imp)
	}
	if a, _ := st.FindAccount(ctx, "Cliente1"); a == nil {
		t.Fatal("Cliente1 not imported")
	}
	if a, _ := st.FindAccount(ctx, "Outro1"); a != nil {
		t.Fatal("clean import kept stale account")
	}
	if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "servers.conf")); err != nil || string(b) == "" {
		t.Fatalf("servers.conf not regenerated: %v %q", err, string(b))
	}
}
