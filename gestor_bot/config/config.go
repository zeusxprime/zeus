package config

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	BotToken                string
	AdminIDs                []int64
	AdminDisplayName        string
	WhatsAppAdminNumbers    []string
	ServerHost              string
	SSHPorts                []string
	SSHShell                string
	DataDir                 string
	DBPath                  string
	UsuariosDBPath          string
	CheckUserDBPath         string
	PrincipalManagerOnly    bool
	RemoteAgentPort         int
	RemoteAgentToken        string
	RemoteVPSConfig         string
	CheckUserHost           string
	CheckUserPort           int
	CheckUserPublicURL      string
	CheckUserCentralMode    bool
	VPNDNSDomain            string
	VPNDNSEnabled           bool
	VPNDNSInterval          int
	CloudflareToken         string
	VPSSuspensionInterval   int
	OnlineXrayWindowSeconds int
	XrayCreateEnabled       bool
	WhatsAppAuthDir         string
	Xray                    XrayConfig
}

type XrayConfig struct {
	EnableDirectConfig     bool
	ConfigPaths            []string
	InboundTag             string
	Protocol               string
	Flow                   string
	RestartCommand         string
	EnableDragonCorePG     bool
	DragonCoreDB           string
	DragonCorePSQLBin      string
	DragonCorePSQLUser     string
	DragonCoreXrayProtocol string
	LinkPort               int
	LinkSecurity           string
	LinkNetwork            string
	LinkSNI                string
	LinkHost               string
	LinkPath               string
	LinkExtra              string
	AccessLogPaths         []string
}

func Load(path string) (Config, error) {
	env := map[string]string{}
	if path == "" {
		path = firstExisting(os.Getenv("CONFIG_ENV"), os.Getenv("PRIMECEL_ENV_FILE"), "/etc/primecel-gestor/config.env", filepath.Join(os.Getenv("PWD"), "config.env"))
	}
	if path != "" {
		m, err := readEnv(path)
		if err != nil {
			return Config{}, err
		}
		for k, v := range m {
			env[k] = v
		}
	}
	// tokens.env é o local central compartilhado por bot, instalador e CheckUser.
	// Quando existir, ele tem prioridade sobre config.env para segredos.
	for _, tokenPath := range tokenEnvPaths(path) {
		if tokenPath == "" {
			continue
		}
		if m, err := readEnv(tokenPath); err == nil {
			for k, v := range m {
				if strings.HasSuffix(k, "_TOKEN") || k == "CLOUDFLARE_API_TOKEN" {
					env[k] = v
				}
			}
		}
	}
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i > 0 {
			env[e[:i]] = e[i+1:]
		}
	}
	cfg := Config{
		BotToken:                get(env, "BOT_TOKEN", ""),
		AdminIDs:                parseInt64List(get(env, "ADMIN_IDS", "")),
		AdminDisplayName:        get(env, "ADMIN_DISPLAY_NAME", "Admin"),
		WhatsAppAdminNumbers:    parseCSV(get(env, "WHATSAPP_ADMIN_NUMBERS", "")),
		ServerHost:              get(env, "SERVER_HOST", ""),
		SSHPorts:                parseCSVDefault(get(env, "SSH_PORTS", "22,80,443")),
		SSHShell:                get(env, "SSH_SHELL", "/bin/false"),
		DataDir:                 get(env, "BOT_DATA_DIR", "/etc/primecel-gestor"),
		UsuariosDBPath:          get(env, "USUARIOS_DB_PATH", "/root/usuarios.db"),
		CheckUserDBPath:         get(env, "CHECKUSER_DB_PATH", "/root/db.sqlite3"),
		PrincipalManagerOnly:    parseBool(get(env, "PRINCIPAL_MANAGER_ONLY", "0")),
		RemoteAgentPort:         parseInt(get(env, "REMOTE_AGENT_PORT", "8787"), 8787),
		RemoteAgentToken:        get(env, "REMOTE_AGENT_TOKEN", ""),
		RemoteVPSConfig:         get(env, "REMOTE_VPS_CONFIG", "/etc/primecel-gestor/servers.conf"),
		CheckUserHost:           get(env, "CHECKUSER_HOST", "0.0.0.0"),
		CheckUserPort:           parseInt(get(env, "CHECKUSER_PORT", "2052"), 2052),
		CheckUserPublicURL:      get(env, "CHECKUSER_PUBLIC_URL", ""),
		CheckUserCentralMode:    parseBool(get(env, "CHECKUSER_CENTRAL_MODE", "1")),
		VPNDNSDomain:            get(env, "VPN_DNS_DOMAIN", ""),
		VPNDNSEnabled:           parseBool(get(env, "VPN_DNS_ENABLED", "0")),
		VPNDNSInterval:          parseInt(get(env, "VPN_DNS_INTERVAL", "60"), 60),
		CloudflareToken:         get(env, "CLOUDFLARE_API_TOKEN", ""),
		VPSSuspensionInterval:   parseInt(get(env, "VPS_SUSPENSION_INTERVAL", "5"), 5),
		OnlineXrayWindowSeconds: parseInt(get(env, "ONLINE_XRAY_WINDOW_SECONDS", "30"), 30),
		XrayCreateEnabled:       parseBool(get(env, "XRAY_CREATE_ENABLED", "1")),
	}
	cfg.DBPath = get(env, "GESTOR_DB_PATH", filepath.Join(cfg.DataDir, "gestor.db"))
	cfg.WhatsAppAuthDir = get(env, "WHATSAPP_AUTH_DIR", filepath.Join(cfg.DataDir, "whatsapp-auth"))
	cfg.Xray = XrayConfig{
		EnableDirectConfig:     parseBool(get(env, "ENABLE_DIRECT_XRAY_CONFIG", "1")),
		ConfigPaths:            parseCSVDefault(get(env, "XRAY_CONFIG_PATHS", "/usr/local/etc/xray/config.json,/etc/xray/config.json,/opt/xray/config.json")),
		InboundTag:             get(env, "XRAY_INBOUND_TAG", "inbound-dragoncore"),
		Protocol:               get(env, "XRAY_PROTOCOL", "vless"),
		Flow:                   get(env, "XRAY_FLOW", ""),
		RestartCommand:         getAllowEmpty(env, "XRAY_RESTART_COMMAND", "systemctl restart xray || systemctl restart xray.service || systemctl restart v2ray || true"),
		EnableDragonCorePG:     parseBool(get(env, "ENABLE_DRAGONCORE_PG", "1")),
		DragonCoreDB:           get(env, "DRAGONCORE_DB", "dragoncore"),
		DragonCorePSQLBin:      get(env, "DRAGONCORE_PSQL_BIN", "psql"),
		DragonCorePSQLUser:     get(env, "DRAGONCORE_PSQL_USER", "postgres"),
		DragonCoreXrayProtocol: get(env, "DRAGONCORE_XRAY_PROTOCOL", "xhttp"),
		LinkPort:               parseInt(get(env, "XRAY_LINK_PORT", "443"), 443),
		LinkSecurity:           get(env, "XRAY_LINK_SECURITY", "tls"),
		LinkNetwork:            get(env, "XRAY_LINK_NETWORK", "xhttp"),
		LinkSNI:                get(env, "XRAY_LINK_SNI", ""), LinkHost: get(env, "XRAY_LINK_HOST", ""), LinkPath: get(env, "XRAY_LINK_PATH", "/"), LinkExtra: get(env, "XRAY_LINK_EXTRA", ""),
		AccessLogPaths: parseCSVDefault(get(env, "XRAY_ACCESS_LOG_PATHS", "/var/log/xray/access.log,/usr/local/var/log/xray/access.log,/usr/local/etc/xray/access.log,/var/log/v2ray/access.log")),
	}
	if legacyOrReservedDNS(cfg.VPNDNSDomain) {
		cfg.VPNDNSDomain = ""
		cfg.VPNDNSEnabled = false
	}
	if cfg.DataDir == "" {
		return cfg, errors.New("BOT_DATA_DIR vazio")
	}
	return cfg, nil
}

func legacyOrReservedDNS(domain string) bool {
	d := strings.Trim(strings.ToLower(domain), ". ")
	switch d {
	case "", "0":
		return false
	case "sv.primecel.shop", "server.primecel.shop", "vpn.primecel.shop", "dns.443.primecel.shop", "dns.8443.primecel.shop", "xray.primecel.shop":
		return true
	default:
		return false
	}
}

func tokenEnvPaths(configPath string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	add(os.Getenv("TOKEN_FILE"))
	if configPath != "" {
		add(filepath.Join(filepath.Dir(configPath), "tokens.env"))
	}
	add("/etc/primecel-gestor/tokens.env")
	return out
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
func readEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.Trim(strings.TrimSpace(line[i+1:]), "\"")
		out[k] = v
	}
	return out, sc.Err()
}
func get(m map[string]string, k, d string) string {
	if v, ok := m[k]; ok && v != "" {
		return v
	}
	return d
}
func parseCSV(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
func parseCSVDefault(s string) []string { return parseCSV(s) }
func parseBool(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "1" || s == "true" || s == "yes" || s == "sim" || s == "on"
}
func parseInt(s string, d int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return d
	}
	return n
}
func parseInt64List(s string) []int64 {
	var out []int64
	for _, p := range parseCSV(s) {
		n, err := strconv.ParseInt(p, 10, 64)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}

func getAllowEmpty(m map[string]string, k, d string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return d
}
