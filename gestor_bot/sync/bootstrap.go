package sync

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/model"
)

// BootstrapAgent instala/atualiza o agente Go na VPS secundária usando SSH.
// Primeiro tenta copiar o binário atual. Se o serviço não subir por incompatibilidade
// de arquitetura/glibc/libsqlite, envia o código-fonte e compila na própria VPS.
func (m *Manager) BootstrapAgent(ctx context.Context, srv model.Server) error {
	return m.bootstrapAgent(ctx, srv)
}

func (m *Manager) bootstrapAgent(ctx context.Context, srv model.Server) error {
	host := strings.TrimSpace(srv.Host)
	if host == "" {
		return errors.New("IP/host vazio")
	}
	user := strings.TrimSpace(srv.SSHUser)
	if user == "" {
		user = "root"
	}
	sshPort := srv.SSHPort
	if sshPort == 0 {
		sshPort = 22
	}
	agentPort := nonZero(srv.AgentPort, m.cfg.RemoteAgentPort, 8787)
	srv = m.ensureServerAgentToken(ctx, srv)
	token := strings.TrimSpace(first(srv.AgentToken, m.cfg.RemoteAgentToken))
	if token == "" {
		return errors.New("não foi possível gerar token do agente")
	}
	password := strings.TrimSpace(srv.SSHPassword)
	if password == "" {
		return errors.New("agente offline e senha SSH/root não cadastrada para instalar")
	}
	if _, err := exec.LookPath("sshpass"); err != nil {
		return errors.New("sshpass não instalado no servidor principal")
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("binário local não encontrado: %w", err)
	}
	if st, err := os.Stat(bin); err != nil || st.IsDir() {
		return errors.New("binário local inválido para copiar à VPS")
	}

	bootCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	remote := user + "@" + host
	remoteTmp := "/tmp/primecel-gestor-agent-bin"
	if out, err := m.runSSH(bootCtx, password, sshPort, remote, "mkdir -p /etc/primecel-gestor /usr/local/bin /tmp"); err != nil {
		return fmt.Errorf("SSH falhou: %s", compactErr(err, out))
	}
	if out, err := m.runSCP(bootCtx, password, sshPort, bin, remote+":"+remoteTmp); err != nil {
		return fmt.Errorf("envio do binário falhou: %s", compactErr(err, out))
	}
	script := remoteInstallScript(remoteTmp, agentPort, token)
	if out, err := m.runSSH(bootCtx, password, sshPort, remote, script); err == nil {
		return nil
	} else if !shouldTrySourceBuild(out) {
		return fmt.Errorf("serviço do agente falhou: %s", compactErr(err, out))
	}

	if err := m.bootstrapAgentFromSource(bootCtx, password, sshPort, remote, agentPort, token); err != nil {
		return err
	}
	return nil
}

func (m *Manager) bootstrapAgentFromSource(ctx context.Context, password string, sshPort int, remote string, agentPort int, token string) error {
	srcDir, err := locateSourceDir()
	if err != nil {
		return fmt.Errorf("serviço do agente falhou e não foi possível compilar na VPS: %s", err.Error())
	}
	archive, err := createSourceArchive(srcDir)
	if err != nil {
		return fmt.Errorf("serviço do agente falhou e não foi possível preparar fonte: %s", err.Error())
	}
	defer os.Remove(archive)
	remoteArchive := "/tmp/primecel-gestor-src.tar.gz"
	if out, err := m.runSCP(ctx, password, sshPort, archive, remote+":"+remoteArchive); err != nil {
		return fmt.Errorf("serviço do agente falhou; envio do código-fonte falhou: %s", compactErr(err, out))
	}
	if out, err := m.runSSH(ctx, password, sshPort, remote, remoteBuildInstallScript(remoteArchive, agentPort, token)); err != nil {
		return fmt.Errorf("serviço do agente falhou; compilação na VPS falhou: %s", compactErr(err, out))
	}
	return nil
}

func remoteInstallScript(remoteTmp string, port int, token string) string {
	token = sanitizeEnvValue(token)
	return fmt.Sprintf(`set -e
export DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then
  apt-get update -y >/dev/null 2>&1 || true
  apt-get install -y ca-certificates libsqlite3-0 >/dev/null 2>&1 || true
fi
mkdir -p /etc/primecel-gestor /usr/local/bin
install -m 755 %s /usr/local/bin/primecel-gestor
if ! /usr/local/bin/primecel-gestor version >/tmp/primecel-agent-version.log 2>&1; then
  echo "binario_incompativel: $(cat /tmp/primecel-agent-version.log 2>/dev/null)"
  exit 12
fi
%s
%s
systemctl daemon-reload
systemctl enable primecel-gestor-agent >/dev/null 2>&1 || true
systemctl restart primecel-gestor-agent
sleep 2
if ! systemctl is-active --quiet primecel-gestor-agent; then
  echo "agente_nao_iniciou"
  systemctl status primecel-gestor-agent --no-pager -l 2>&1 | tail -35 || true
  journalctl -u primecel-gestor-agent -n 35 --no-pager 2>&1 || true
  exit 13
fi
`, shellQuote(remoteTmp), remoteConfigEnv(port, token), remoteSystemdUnit())
}

func remoteBuildInstallScript(remoteArchive string, port int, token string) string {
	token = sanitizeEnvValue(token)
	return fmt.Sprintf(`set -e
export DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then
  apt-get update -y >/dev/null 2>&1 || true
  apt-get install -y ca-certificates curl wget tar gzip build-essential gcc g++ pkg-config libsqlite3-dev >/dev/null 2>&1 || true
fi
need_go=1
if command -v go >/dev/null 2>&1; then
  gov="$(go version | awk '{print $3}' | sed 's/^go//')"
  major="${gov%%.*}"
  rest="${gov#*.}"
  minor="${rest%%.*}"
  if [ "$major" -gt 1 ] 2>/dev/null || { [ "$major" -eq 1 ] 2>/dev/null && [ "$minor" -ge 23 ] 2>/dev/null; }; then
    need_go=0
  fi
fi
if [ "$need_go" = "1" ]; then
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) goarch="amd64" ;;
    aarch64|arm64) goarch="arm64" ;;
    *) echo "arquitetura não suportada para Go: $arch"; exit 21 ;;
  esac
  gofile="go1.23.6.linux-${goarch}.tar.gz"
  gourl="https://go.dev/dl/${gofile}"
  rm -f "/tmp/${gofile}"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$gourl" -o "/tmp/${gofile}"
  else
    wget -q "$gourl" -O "/tmp/${gofile}"
  fi
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "/tmp/${gofile}"
fi
export PATH=/usr/local/go/bin:/usr/bin:/bin:$PATH
rm -rf /tmp/primecel-gestor-src
mkdir -p /tmp/primecel-gestor-src
mkdir -p /opt/primecel-gestor-agent-src
tar -xzf %s -C /tmp/primecel-gestor-src
cp -a /tmp/primecel-gestor-src/. /opt/primecel-gestor-agent-src/
cd /opt/primecel-gestor-agent-src
CGO_ENABLED=1 go build -o /usr/local/bin/primecel-gestor ./cmd/primecel-gestor
chmod 755 /usr/local/bin/primecel-gestor
if ! /usr/local/bin/primecel-gestor version >/tmp/primecel-agent-version.log 2>&1; then
  echo "binario_compilado_invalido: $(cat /tmp/primecel-agent-version.log 2>/dev/null)"
  exit 22
fi
%s
%s
systemctl daemon-reload
systemctl enable primecel-gestor-agent >/dev/null 2>&1 || true
systemctl restart primecel-gestor-agent
sleep 2
if ! systemctl is-active --quiet primecel-gestor-agent; then
  echo "agente_nao_iniciou_apos_compilar"
  systemctl status primecel-gestor-agent --no-pager -l 2>&1 | tail -35 || true
  journalctl -u primecel-gestor-agent -n 35 --no-pager 2>&1 || true
  exit 23
fi
`, shellQuote(remoteArchive), remoteConfigEnv(port, token), remoteSystemdUnit())
}

func remoteConfigEnv(port int, token string) string {
	return fmt.Sprintf(`cat > /etc/primecel-gestor/config.env <<'ENVEOF'
BOT_DATA_DIR=/etc/primecel-gestor
GESTOR_DB_PATH=/etc/primecel-gestor/gestor.db
USUARIOS_DB_PATH=/root/usuarios.db
SSH_SHELL=/bin/false
REMOTE_AGENT_PORT=%d
REMOTE_AGENT_TOKEN=%s
PRINCIPAL_MANAGER_ONLY=0
XRAY_CREATE_ENABLED=1
ENABLE_DIRECT_XRAY_CONFIG=1
XRAY_CONFIG_PATHS=/usr/local/etc/xray/config.json,/etc/xray/config.json,/opt/xray/config.json
XRAY_INBOUND_TAG=inbound-dragoncore
XRAY_PROTOCOL=vless
DRAGONCORE_XRAY_PROTOCOL=xhttp
ENABLE_DRAGONCORE_PG=1
DRAGONCORE_DB=dragoncore
DRAGONCORE_PSQL_BIN=psql
DRAGONCORE_PSQL_USER=postgres
ENVEOF
chmod 600 /etc/primecel-gestor/config.env`, port, token)
}

func remoteSystemdUnit() string {
	return `cat > /etc/systemd/system/primecel-gestor-agent.service <<'UNITEOF'
[Unit]
Description=PrimeCel Gestor Remote Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/primecel-gestor/config.env
ExecStart=/usr/local/bin/primecel-gestor agent --start --host 0.0.0.0 --port ${REMOTE_AGENT_PORT}
Restart=always
RestartSec=3
User=root

[Install]
WantedBy=multi-user.target
UNITEOF`
}

func sanitizeEnvValue(s string) string {
	return strings.NewReplacer("\n", "", "\r", "", "'", "", "\"", "").Replace(strings.TrimSpace(s))
}

func shouldTrySourceBuild(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "binario_incompativel") || strings.Contains(low, "exec format") || strings.Contains(low, "glibc_") || strings.Contains(low, "no such file or directory") || strings.Contains(low, "libsqlite") || strings.Contains(low, "agente_nao_iniciou")
}

func (m *Manager) runSSH(ctx context.Context, password string, port int, remote string, command string) (string, error) {
	args := []string{"-p", password, "ssh", "-q", "-o", "LogLevel=ERROR", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=10", "-p", strconv.Itoa(port), remote, command}
	out, err := exec.CommandContext(ctx, "sshpass", args...).CombinedOutput()
	return string(out), err
}

func (m *Manager) runSCP(ctx context.Context, password string, port int, src string, dst string) (string, error) {
	args := []string{"-p", password, "scp", "-q", "-o", "LogLevel=ERROR", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=10", "-P", strconv.Itoa(port), src, dst}
	out, err := exec.CommandContext(ctx, "sshpass", args...).CombinedOutput()
	return string(out), err
}

func locateSourceDir() (string, error) {
	seen := map[string]bool{}
	var candidates []string
	if v := strings.TrimSpace(os.Getenv("PRIMECEL_SOURCE_DIR")); v != "" {
		candidates = append(candidates, v)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		candidates = append(candidates, d, filepath.Dir(d), filepath.Join(d, "primecel-gestor"), "/opt/primecel-gestor")
	} else {
		candidates = append(candidates, "/opt/primecel-gestor")
	}
	for _, c := range candidates {
		c = filepath.Clean(c)
		if c == "." || seen[c] {
			continue
		}
		seen[c] = true
		if hasProjectSource(c) {
			return c, nil
		}
	}
	return "", errors.New("fonte do projeto não encontrada em /opt/primecel-gestor")
}

func hasProjectSource(dir string) bool {
	if st, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil || st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(dir, "cmd", "primecel-gestor", "main.go")); err != nil || st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(dir, "gestor_bot")); err != nil || !st.IsDir() {
		return false
	}
	return true
}

func createSourceArchive(srcDir string) (string, error) {
	tmp, err := os.CreateTemp("", "primecel-gestor-src-*.tar.gz")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer tmp.Close()
	gz := gzip.NewWriter(tmp)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	root := filepath.Clean(srcDir)
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		base := filepath.Base(path)
		if info.IsDir() && shouldSkipSourceDir(base, name) {
			return filepath.SkipDir
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func shouldSkipSourceDir(base string, rel string) bool {
	skip := map[string]bool{".git": true, "node_modules": true, "backups": true, "whatsapp-auth": true, "tmp": true, "dist": true, "build": true}
	if skip[base] {
		return true
	}
	return strings.Contains(rel, "/.git/") || strings.Contains(rel, "/node_modules/")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func compactErr(err error, out string) string {
	msg := strings.TrimSpace(out)
	if msg == "" && err != nil {
		msg = err.Error()
	}
	lines := strings.Split(strings.ReplaceAll(msg, "\r", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		low := strings.ToLower(ln)
		if strings.Contains(low, "permanently added") || strings.Contains(low, "known hosts") {
			continue
		}
		kept = append(kept, ln)
	}
	msg = strings.Join(kept, " ")
	if msg == "" && err != nil {
		msg = err.Error()
	}
	for strings.Contains(msg, "  ") {
		msg = strings.ReplaceAll(msg, "  ", " ")
	}
	if len([]rune(msg)) > 220 {
		r := []rune(msg)
		msg = string(r[:220]) + "..."
	}
	return msg
}
