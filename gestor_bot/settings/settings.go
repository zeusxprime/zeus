package settings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/store"
)

type Manager struct {
	Config config.Config
	Store  *store.DB
}

func NewManager(cfg config.Config, st *store.DB) *Manager { return &Manager{Config: cfg, Store: st} }
func (m *Manager) SetAdminDisplayName(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "0" || strings.EqualFold(name, "admin") {
		name = "Admin"
	}
	if len([]rune(name)) > 32 {
		return errors.New("nome máximo: 32 caracteres")
	}
	return m.setEnvSetting(ctx, "ADMIN_DISPLAY_NAME", name, "admin_display_name")
}
func (m *Manager) SetWhatsAppAdmins(ctx context.Context, phones string) error {
	nums := digitsList(phones)
	return m.setEnvSetting(ctx, "WHATSAPP_ADMIN_NUMBERS", strings.Join(nums, ","), "whatsapp_admin_numbers")
}
func (m *Manager) SetPrincipalManagerOnly(ctx context.Context, enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	return m.setEnvSetting(ctx, "PRINCIPAL_MANAGER_ONLY", v, "principal_manager_only")
}
func (m *Manager) SetCloudflareToken(ctx context.Context, token string) error {
	token = strings.TrimSpace(token)
	// Fonte central única do Cloudflare no sistema Go: SQLite settings.cloudflare_token.
	// Instalador principal e CheckUser também leem/gravam essa mesma chave.
	return m.Store.SetSetting(ctx, "cloudflare_token", token)
}

func (m *Manager) SetXrayCreateEnabled(ctx context.Context, enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	return m.setEnvSetting(ctx, "XRAY_CREATE_ENABLED", v, "xray_create_enabled")
}
func (m *Manager) SetVPNDNS(ctx context.Context, domain string, enabled bool) error {
	_ = domain
	_ = enabled
	_ = m.setEnvSetting(ctx, "VPN_DNS_DOMAIN", "", "vpn_dns_domain")
	_ = m.setEnvSetting(ctx, "VPN_DNS_ENABLED", "0", "vpn_dns_enabled")
	return errors.New("DNS VPS foi removido; servidores usam vpn.primecel.shop")
}
func (m *Manager) setEnvSetting(ctx context.Context, envKey, value, settingKey string) error {
	if err := m.Store.SetSetting(ctx, settingKey, value); err != nil {
		return err
	}
	return upsertEnv(filepath.Join(m.Config.DataDir, "config.env"), envKey, value)
}
func upsertEnv(path, key, value string) error {
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	data := []byte{}
	if b, err := os.ReadFile(path); err == nil {
		data = b
	}
	lines := strings.Split(string(data), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, key+"=") {
			lines[i] = key + "=" + quoteIfNeeded(value)
			found = true
		}
	}
	if !found {
		lines = append(lines, key+"="+quoteIfNeeded(value))
	}
	out := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	return os.WriteFile(path, []byte(out), 0600)
}
func quoteIfNeeded(v string) string {
	if strings.ContainsAny(v, " \t#\n\"") {
		v = strings.ReplaceAll(v, "\"", "\\\"")
		return "\"" + v + "\""
	}
	return v
}
func digitsList(s string) []string {
	re := regexp.MustCompile(`\d+`)
	var out []string
	for _, m := range re.FindAllString(s, -1) {
		if len(m) >= 10 {
			out = append(out, m)
		}
	}
	return out
}
