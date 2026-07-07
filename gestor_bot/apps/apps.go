package apps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
)

type Manager struct {
	Config config.Config
	Store  *store.DB
}

func NewManager(cfg config.Config, st *store.DB) *Manager { return &Manager{Config: cfg, Store: st} }
func (m *Manager) appsDir() string                        { return filepath.Join(m.Config.DataDir, "apps") }

type ImportOptions struct{ Name, Version, SourcePath, FileID, FileUniqueID, FileName, MimeType string }

func (m *Manager) Import(ctx context.Context, opt ImportOptions) (model.App, error) {
	name := strings.TrimSpace(opt.Name)
	if name == "" {
		return model.App{}, errors.New("nome do app obrigatório")
	}
	if opt.Version == "" {
		opt.Version = "1.0"
	}
	if err := os.MkdirAll(m.appsDir(), 0700); err != nil {
		return model.App{}, err
	}
	dest := ""
	fn := opt.FileName
	if opt.SourcePath != "" {
		if strings.ToLower(filepath.Ext(opt.SourcePath)) != ".apk" {
			return model.App{}, errors.New("somente .apk é aceito")
		}
		b, err := os.ReadFile(opt.SourcePath)
		if err != nil {
			return model.App{}, err
		}
		if fn == "" {
			fn = safeName(name) + ".apk"
		}
		dest = filepath.Join(m.appsDir(), fn)
		if err := os.WriteFile(dest, b, 0600); err != nil {
			return model.App{}, err
		}
	}
	app := model.App{Name: name, Version: opt.Version, FileID: opt.FileID, FileUniqueID: opt.FileUniqueID, FileName: fn, MimeType: nonEmpty(opt.MimeType, "application/vnd.android.package-archive"), Path: dest, UpdatedAt: time.Now().UTC()}
	if err := m.Store.UpsertApp(ctx, app); err != nil {
		return model.App{}, err
	}
	_ = m.ExportJSON(ctx)
	return app, nil
}
func (m *Manager) List(ctx context.Context) ([]model.App, error) { return m.Store.ListApps(ctx) }
func (m *Manager) Remove(ctx context.Context, name string) error {
	app, _ := m.Store.FindApp(ctx, name)
	if app != nil && app.Path != "" {
		_ = os.Remove(app.Path)
	}
	if err := m.Store.DeleteApp(ctx, name); err != nil {
		return err
	}
	return m.ExportJSON(ctx)
}
func (m *Manager) ExportJSON(ctx context.Context) error {
	apps, err := m.Store.ListApps(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.appsDir(), 0700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(apps, "", "  ")
	return os.WriteFile(filepath.Join(m.appsDir(), "apps.json"), b, 0600)
}
func safeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "app"
	}
	return b.String()
}
func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
