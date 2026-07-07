package sync

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
)

type Manager struct {
	cfg config.Config
	st  *store.DB
	hc  *http.Client
}

func NewManager(cfg config.Config, st *store.DB) *Manager {
	return &Manager{cfg: cfg, st: st, hc: &http.Client{Timeout: 8 * time.Second}}
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

type Request struct {
	Token          string           `json:"token,omitempty"`
	Action         string           `json:"action"`
	Accesses       []SnapshotAccess `json:"accesses,omitempty"`
	Username       string           `json:"username,omitempty"`
	Password       string           `json:"password,omitempty"`
	Expiry         string           `json:"expiry,omitempty"`
	ExpiresAt      string           `json:"expires_at,omitempty"`
	Limit          int              `json:"limit,omitempty"`
	UUID           string           `json:"uuid,omitempty"`
	ClientWhatsApp string           `json:"client_whatsapp,omitempty"`
	MonthlyValue   float64          `json:"monthly_value,omitempty"`
	Usernames      []string         `json:"usernames,omitempty"`
}

type Response struct {
	OK      bool     `json:"ok"`
	Agent   string   `json:"agent"`
	Version string   `json:"version"`
	Action  string   `json:"action"`
	Output  string   `json:"output"`
	Error   string   `json:"error"`
	Applied int      `json:"applied"`
	Failed  int      `json:"failed"`
	Details []string `json:"details"`
}

type ServerResult struct {
	Server model.Server `json:"server"`
	OK     bool         `json:"ok"`
	Error  string       `json:"error,omitempty"`
	Resp   Response     `json:"response"`
}

func (m *Manager) SyncStateSnapshot(ctx context.Context) ([]ServerResult, error) {
	return m.SyncStateSnapshotProgress(ctx, nil)
}

// SyncStateSnapshotProgress sincroniza todas as VPS e chama progress após cada servidor.
// Usado pelo Telegram para mostrar a progressão real da sincronização.
func (m *Manager) SyncStateSnapshotProgress(ctx context.Context, progress func(ServerResult, int, int)) ([]ServerResult, error) {
	servers, err := m.st.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	accesses, err := m.buildSnapshotAccesses(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]ServerResult, 0, len(servers))
	total := len(servers)
	for i, srv := range servers {
		idx := i + 1
		if progress != nil {
			// Evento visual de início: permite ao Telegram mostrar qual servidor
			// está sincronizando antes de a chamada remota terminar. Não entra
			// em results e não altera a aplicação/sincronização real.
			progress(ServerResult{Server: srv, Resp: Response{Action: "syncing"}}, idx, total)
		}
		res := m.syncOneStateSnapshot(ctx, srv, accesses)
		results = append(results, res)
		if progress != nil {
			progress(res, idx, total)
		}
	}
	return results, nil
}

func (m *Manager) buildSnapshotAccesses(ctx context.Context) ([]SnapshotAccess, error) {
	accs, err := m.st.ListAccounts(ctx, false)
	if err != nil {
		return nil, err
	}
	accesses := make([]SnapshotAccess, 0, len(accs))
	now := time.Now().UTC()
	for _, a := range accs {
		if a.DeletedAt != nil || a.Status != "active" {
			continue
		}
		// A secundária deve refletir as contas realmente ativas do painel.
		// Contas expiradas saem do snapshot para serem removidas/reconciliadas
		// do Linux, usuarios.db, CheckUser e Xray na VPS secundária.
		if !a.ExpiresAt.After(now) {
			continue
		}
		accesses = append(accesses, snapshotFromAccount(a))
	}
	return accesses, nil
}

func (m *Manager) syncOneStateSnapshot(ctx context.Context, srv model.Server, accesses []SnapshotAccess) ServerResult {
	srv = m.ensureServerAgentToken(ctx, srv)
	req := Request{Action: "state-snapshot", Token: first(srv.AgentToken, m.cfg.RemoteAgentToken), Accesses: accesses}

	// A sincronização do bot Python 024 era tolerante: preparava a VPS por SSH
	// e aplicava o estado real no Linux/usuarios.db/Xray. No Go, se o agente
	// antigo estiver online, ele pode responder OK sem refletir no DragonCore.
	// Por isso, quando existe senha SSH cadastrada, atualizamos/configuramos o
	// agente antes e finalizamos com uma reconciliação legada via SSH.
	if strings.TrimSpace(srv.SSHPassword) != "" {
		_ = m.bootstrapAgent(ctx, srv)
	}

	resp, err := m.post(ctx, srv, req)
	if err != nil && shouldBootstrapAfterPost(err) {
		bootErr := m.bootstrapAgent(ctx, srv)
		if bootErr == nil {
			resp, err = m.post(ctx, srv, req)
		}
		if err != nil || bootErr != nil {
			fbResp, fbErr := m.syncViaSSH(ctx, srv, req)
			if fbErr == nil {
				resp, err = fbResp, nil
			} else {
				err = fmt.Errorf("agente remoto offline; fallback SSH falhou: %s", compactErr(fbErr, ""))
			}
		}
	}
	if err == nil && resp.OK && strings.TrimSpace(srv.SSHPassword) != "" && syncSSHVerifyEnabled() {
		sshResp, sshErr := m.syncViaSSH(ctx, srv, req)
		if sshErr == nil {
			resp = sshResp
		} else {
			err = fmt.Errorf("verificação/aplicação SSH falhou: %s", compactErr(sshErr, ""))
		}
	}
	res := ServerResult{Server: srv, Resp: resp, OK: err == nil && resp.OK}
	if err != nil {
		res.Error = friendlySyncError(err)
	}
	if !resp.OK && resp.Error != "" {
		res.Error = friendlySyncError(errors.New(resp.Error))
	}
	_ = m.st.AddServerSyncLog(ctx, srv.Host, "state-snapshot", res.OK, first(res.Error, resp.Output))
	return res
}

func (m *Manager) RestartServersProgress(ctx context.Context, progress func(ServerResult, int, int)) ([]ServerResult, error) {
	servers, err := m.st.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]ServerResult, 0, len(servers))
	total := len(servers)
	for i, srv := range servers {
		idx := i + 1
		if progress != nil {
			progress(ServerResult{Server: srv, Resp: Response{Action: "restarting"}}, idx, total)
		}
		res := m.restartOneServer(ctx, srv)
		results = append(results, res)
		if progress != nil {
			progress(res, idx, total)
		}
	}
	return results, nil
}

func (m *Manager) RestartServers(ctx context.Context) ([]ServerResult, error) {
	return m.RestartServersProgress(ctx, nil)
}

func (m *Manager) restartOneServer(ctx context.Context, srv model.Server) ServerResult {
	srv = m.ensureServerAgentToken(ctx, srv)
	req := Request{Action: "server-reboot", Token: first(srv.AgentToken, m.cfg.RemoteAgentToken)}
	resp, err := m.post(ctx, srv, req)
	if err != nil || !resp.OK {
		sshResp, sshErr := m.restartViaSSH(ctx, srv)
		if sshErr == nil {
			resp, err = sshResp, nil
		} else if err == nil {
			err = sshErr
		} else {
			err = fmt.Errorf("agente remoto falhou e fallback SSH falhou: %s", compactErr(sshErr, ""))
		}
	}
	res := ServerResult{Server: srv, Resp: resp, OK: err == nil && resp.OK}
	if err != nil {
		res.Error = friendlySyncError(err)
	}
	if !resp.OK && resp.Error != "" {
		res.Error = friendlySyncError(errors.New(resp.Error))
	}
	_ = m.st.AddServerSyncLog(ctx, srv.Host, "server-reboot", res.OK, first(res.Error, resp.Output))
	return res
}

func (m *Manager) restartViaSSH(ctx context.Context, srv model.Server) (Response, error) {
	password := strings.TrimSpace(srv.SSHPassword)
	if password == "" {
		return Response{}, errors.New("senha SSH/root não cadastrada para reiniciar")
	}
	if _, lpErr := exec.LookPath("sshpass"); lpErr != nil {
		return Response{}, errors.New("sshpass não instalado no servidor principal")
	}
	user := strings.TrimSpace(srv.SSHUser)
	if user == "" {
		user = "root"
	}
	sshPort := srv.SSHPort
	if sshPort == 0 {
		sshPort = 22
	}
	remote := user + "@" + strings.TrimSpace(srv.Host)
	rebootCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cmd := `nohup sh -c 'sleep 2; (systemctl reboot || /sbin/reboot || reboot)' >/dev/null 2>&1 & echo REBOOT_SCHEDULED`
	out, err := m.runSSH(rebootCtx, password, sshPort, remote, cmd)
	if err != nil && !strings.Contains(out, "REBOOT_SCHEDULED") {
		return Response{}, fmt.Errorf("reinício via SSH falhou: %s", compactErr(err, out))
	}
	return Response{OK: true, Agent: "ssh-fallback", Action: "server-reboot", Output: "reinício enviado via SSH"}, nil
}

func (m *Manager) SyncRemove(ctx context.Context, username string) ([]ServerResult, error) {
	return m.syncSimple(ctx, Request{Action: "remove", Username: username})
}
func (m *Manager) SyncPassword(ctx context.Context, username, password string) ([]ServerResult, error) {
	return m.syncSimple(ctx, Request{Action: "password", Username: username, Password: password})
}
func (m *Manager) SyncLimit(ctx context.Context, username string, limit int) ([]ServerResult, error) {
	return m.syncSimple(ctx, Request{Action: "limit", Username: username, Limit: limit})
}
func (m *Manager) SyncDeviceUser(ctx context.Context, username string) ([]ServerResult, error) {
	return m.syncSimple(ctx, Request{Action: "deviceid-user", Username: username})
}
func (m *Manager) SyncDeviceUsers(ctx context.Context, usernames []string) ([]ServerResult, error) {
	return m.syncSimple(ctx, Request{Action: "deviceid-users", Usernames: usernames})
}
func (m *Manager) SyncDeviceScope(ctx context.Context) ([]ServerResult, error) {
	return m.syncSimple(ctx, Request{Action: "deviceid-scope"})
}

func (m *Manager) syncSimple(ctx context.Context, base Request) ([]ServerResult, error) {
	servers, err := m.st.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	var results []ServerResult
	for _, srv := range servers {
		srv = m.ensureServerAgentToken(ctx, srv)
		req := base
		req.Token = first(srv.AgentToken, m.cfg.RemoteAgentToken)
		resp, err := m.post(ctx, srv, req)
		if err != nil && shouldBootstrapAfterPost(err) {
			bootErr := m.bootstrapAgent(ctx, srv)
			if bootErr == nil {
				resp, err = m.post(ctx, srv, req)
			}
			if err != nil || bootErr != nil {
				fbResp, fbErr := m.syncViaSSH(ctx, srv, req)
				if fbErr == nil {
					resp, err = fbResp, nil
				} else if bootErr != nil {
					err = fmt.Errorf("agente remoto offline; fallback SSH falhou: %s", compactErr(fbErr, ""))
				} else {
					err = fmt.Errorf("agente remoto offline; fallback SSH falhou: %s", compactErr(fbErr, ""))
				}
			}
		}
		res := ServerResult{Server: srv, Resp: resp, OK: err == nil && resp.OK}
		if err != nil {
			res.Error = friendlySyncError(err)
		}
		if !resp.OK && resp.Error != "" {
			res.Error = friendlySyncError(errors.New(resp.Error))
		}
		_ = m.st.AddServerSyncLog(ctx, srv.Host, base.Action, res.OK, first(res.Error, resp.Output))
		results = append(results, res)
	}
	return results, nil
}

func (m *Manager) syncViaSSH(ctx context.Context, srv model.Server, req Request) (Response, error) {
	var resp Response
	srv = m.ensureServerAgentToken(ctx, srv)
	password := strings.TrimSpace(srv.SSHPassword)
	if password == "" {
		return resp, errors.New("senha SSH/root não cadastrada para fallback")
	}
	if _, lpErr := exec.LookPath("sshpass"); lpErr != nil {
		return resp, errors.New("sshpass não instalado no servidor principal")
	}
	user := strings.TrimSpace(srv.SSHUser)
	if user == "" {
		user = "root"
	}
	sshPort := srv.SSHPort
	if sshPort == 0 {
		sshPort = 22
	}
	b, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	tmp, err := os.CreateTemp("", "primecel-sync-*.json")
	if err != nil {
		return resp, err
	}
	localPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(localPath)
		return resp, err
	}
	_ = tmp.Close()
	defer os.Remove(localPath)
	remote := user + "@" + strings.TrimSpace(srv.Host)
	remotePath := fmt.Sprintf("/tmp/primecel-sync-%d.json", time.Now().UnixNano())
	sshCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	if out, err := m.runSCP(sshCtx, password, sshPort, localPath, remote+":"+remotePath); err != nil {
		return resp, fmt.Errorf("envio do snapshot por SSH falhou: %s", compactErr(err, out))
	}
	cmd := fmt.Sprintf("%s\nCONFIG_ENV=/etc/primecel-gestor/config.env /usr/local/bin/primecel-gestor sync apply-local --file %s >/tmp/primecel-sync-go.log 2>&1 || true\n%s\nrc=$?; rm -f %s; exit $rc", remoteSecondaryEnvPatch(), shellQuote(remotePath), remoteLegacyReconcileCommand(remotePath), shellQuote(remotePath))
	out, err := m.runSSH(sshCtx, password, sshPort, remote, cmd)
	if strings.TrimSpace(out) != "" {
		_ = json.Unmarshal([]byte(out), &resp)
	}
	if err != nil {
		if resp.Error != "" {
			return resp, errors.New(formatRemoteFailure(resp))
		}
		return resp, fmt.Errorf("aplicação via SSH falhou: %s", compactErr(err, out))
	}
	if resp.Agent == "" && strings.TrimSpace(out) != "" {
		if jerr := json.Unmarshal([]byte(out), &resp); jerr != nil {
			return resp, fmt.Errorf("resposta SSH inválida: %s", compactErr(jerr, out))
		}
	}
	if resp.Agent == "" {
		return Response{OK: true, Agent: "ssh-fallback", Version: "", Action: req.Action, Output: "sincronizado via SSH"}, nil
	}
	if !resp.OK {
		return resp, errors.New(formatRemoteFailure(resp))
	}
	return resp, nil
}

func remoteLegacyReconcileCommand(snapshotPath string) string {
	return fmt.Sprintf(`SNAPSHOT_FILE=%s python3 <<'PYEOF'
import json, os, re, subprocess, sys
from pathlib import Path

snap = os.environ.get('SNAPSHOT_FILE', '')
resp = {"ok": False, "agent": "ssh-legacy", "version": "", "action": "state-snapshot", "applied": 0, "failed": 0, "details": []}

def sh(cmd, inp=None, timeout=30, env=None):
    return subprocess.run(cmd, input=inp, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, env=env)

def detail(msg):
    resp["details"].append(str(msg)[:180])

try:
    with open(snap, 'r', encoding='utf-8') as f:
        req = json.load(f)
    accesses = req.get('accesses') or []
except Exception as e:
    resp["error"] = "snapshot inválido: " + str(e)
    print(json.dumps(resp, ensure_ascii=False))
    sys.exit(2)

valid = re.compile(r'^[A-Z][A-Za-z0-9_]{3,11}$')
entries = []  # (usuario, senha, limite, validade, expires_at)
failed = 0
applied = 0
xray_items = []
desired_users = set()
desired_xray = set()
ssh_pg_synced = 0
checkuser_written = 0

def parse_date10(raw):
    raw = str(raw or '').strip()[:10]
    if not raw:
        return None
    try:
        import datetime
        return datetime.date.fromisoformat(raw)
    except Exception:
        return None

def days_until(expiry):
    try:
        import datetime
        return max((parse_date10(expiry) - datetime.date.today()).days, 0)
    except Exception:
        return 0

def access_expiry_date(expiry, expires_at, is_trial):
    import datetime
    if not is_trial:
        return expiry
    raw = str(expires_at or '').strip()
    if raw:
        try:
            dt = datetime.datetime.fromisoformat(raw.replace('Z','+00:00')[:19])
            return (dt.date() + datetime.timedelta(days=1)).isoformat()
        except Exception:
            pass
    e = parse_date10(expiry)
    if e:
        return (e + datetime.timedelta(days=1)).isoformat()
    return expiry

def normalize_checkuser_expiry(expiry, expires_at):
    import datetime
    e = parse_date10(expiry)
    raw = str(expires_at or '').strip()
    if raw:
        try:
            dt = datetime.datetime.fromisoformat(raw.replace('Z','+00:00')[:19])
            if e and dt.date() == (e + datetime.timedelta(days=1)) and dt.time() == datetime.time.min:
                return datetime.datetime.combine(e, datetime.time(23,59,59)).isoformat(timespec='seconds')
            return dt.isoformat(timespec='seconds')
        except Exception:
            pass
    if e:
        return datetime.datetime.combine(e, datetime.time(23,59,59)).isoformat(timespec='seconds')
    return raw

def write_checkuser_expiration(username, expiry, expires_at):
    text = normalize_checkuser_expiry(expiry, expires_at)
    if not text:
        return False
    try:
        targets = [('/etc/DragonTeste/expirations.db','/etc/DragonTeste/expirations'),('/root/checkuser_expirations.db','/root/usuarios_expiracao')]
        for db_path, user_dir in targets:
            Path(user_dir).mkdir(parents=True, exist_ok=True)
            Path(user_dir, username + '.txt').write_text(text + '\n', encoding='utf-8')
            lines = []
            p = Path(db_path)
            if p.exists():
                for line in p.read_text(encoding='utf-8', errors='ignore').splitlines():
                    parts = line.split(maxsplit=1)
                    if parts and parts[0] == username:
                        continue
                    if line.strip():
                        lines.append(line.rstrip())
            lines.append(f'{username} {text}')
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text('\n'.join(lines) + '\n', encoding='utf-8')
            try:
                os.chmod(db_path, 0o644)
                os.chmod(Path(user_dir, username + '.txt'), 0o644)
            except Exception:
                pass
        return True
    except Exception as e:
        detail('checkuser expiração: ' + str(e))
        return False

def try_dragoncore_cli_create(username, password, limit, expiry):
    menu = '/opt/DragonCore/menu.php'
    if not os.path.exists(menu) or subprocess.run(['sh','-lc','command -v php >/dev/null 2>&1']).returncode != 0:
        return False
    if subprocess.run(['id','-u',username], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
        return False
    try:
        r = sh(['php', menu, 'criaruser', str(days_until(expiry)), username, password, str(limit)], timeout=45)
        txt = (r.stdout or '') + (r.stderr or '')
        if r.returncode == 0 and subprocess.run(['id','-u',username], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
            return True
        if txt.strip():
            detail(f'{username}: DragonCore criaruser não confirmou Linux: ' + txt.strip()[:120])
    except Exception as e:
        detail(f'{username}: DragonCore criaruser falhou: {e}')
    return False

def sync_dragoncore_ssh_pg(username, password, limit):
    # Igual ao bot 024: o menu DragonCore SSH conta/lista pela tabela users.
    # A diferença aqui é que usamos 3 caminhos: PHP config.php, sudo postgres/psql
    # e psql direto. Assim a restauração remota não depende só do PHP pg_connect.
    lim = str(int(limit))
    php_paths = ['/opt/DragonCore/config.php','/opt/DragonCore/html/config.php','/var/www/html/config.php']
    for cfg in php_paths:
        if os.path.exists(cfg) and subprocess.run(['sh','-lc','command -v php >/dev/null 2>&1']).returncode == 0:
            php_code = f'require_once "{cfg}";\n$conn = pg_connect("host=localhost dbname=dragoncore user={{$db_user}} password={{$db_pass}}");\nif (!$conn) {{ fwrite(STDERR, "pg_connect failed\\n"); exit(2); }}\npg_query($conn, "CREATE TABLE IF NOT EXISTS users (ID SERIAL PRIMARY KEY, usr TEXT, pass TEXT, limi TEXT)");\n$usr = $argv[1];\n$pass = $argv[2];\n$limi = $argv[3];\n$res = pg_query_params($conn, "UPDATE users SET pass=$2, limi=$3 WHERE usr=$1", array($usr, $pass, $limi));\nif (!$res) {{ fwrite(STDERR, pg_last_error($conn)); exit(3); }}\nif (pg_affected_rows($res) < 1) {{\n  $res = pg_query_params($conn, "INSERT INTO users (usr, pass, limi) VALUES ($1,$2,$3)", array($usr, $pass, $limi));\n  if (!$res) {{ fwrite(STDERR, pg_last_error($conn)); exit(4); }}\n}}\npg_close($conn);'
            try:
                r = sh(['php','-r',php_code,username,password,lim], timeout=25)
                if r.returncode == 0:
                    return True
                detail(f'{username}: DragonCore users PG/PHP: ' + ((r.stderr or r.stdout or '').strip()[:140]))
            except Exception as e:
                detail(f'{username}: DragonCore users PG/PHP: {e}')
    # Fallback por psql, útil quando config.php mudou ou o módulo php-pgsql está ausente.
    if subprocess.run(['sh','-lc','command -v psql >/dev/null 2>&1']).returncode == 0:
        su = username.replace("'", "''")
        sp = password.replace("'", "''")
        sl = lim.replace("'", "''")
        sql = (
            "CREATE TABLE IF NOT EXISTS users (ID SERIAL PRIMARY KEY, usr TEXT, pass TEXT, limi TEXT); "
            f"UPDATE users SET pass='{sp}', limi='{sl}' WHERE usr='{su}'; "
            f"INSERT INTO users (usr, pass, limi) SELECT '{su}', '{sp}', '{sl}' WHERE NOT EXISTS (SELECT 1 FROM users WHERE usr='{su}');"
        )
        attempts = [
            ['sudo','-u','postgres','psql','-d','dragoncore','-v','ON_ERROR_STOP=1','-c',sql],
            ['psql','-d','dragoncore','-v','ON_ERROR_STOP=1','-c',sql],
        ]
        for cmd in attempts:
            try:
                r = sh(cmd, timeout=30)
                if r.returncode == 0:
                    return True
                msg = (r.stderr or r.stdout or '').strip()
                if msg:
                    detail(f'{username}: DragonCore users PG/psql: ' + msg[:140])
            except Exception as e:
                detail(f'{username}: DragonCore users PG/psql: {e}')
    return False

def dragoncore_present():
    return any(os.path.exists(p) for p in ['/opt/DragonCore/menu.php','/opt/DragonCore/config.php','/opt/DragonCore/html/config.php','/var/www/html/config.php'])

def dragoncore_users_count():
    if subprocess.run(['sh','-lc','command -v psql >/dev/null 2>&1']).returncode != 0:
        return None
    sql = 'SELECT COUNT(*) FROM users;'
    for cmd in [
        ['sudo','-u','postgres','psql','-d','dragoncore','-t','-A','-c',sql],
        ['psql','-d','dragoncore','-t','-A','-c',sql],
    ]:
        try:
            r = sh(cmd, timeout=15)
            if r.returncode == 0:
                txt = (r.stdout or '').strip().splitlines()
                if txt:
                    return int(txt[-1].strip())
        except Exception:
            pass
    return None

for a in accesses:
    u = str(a.get('username') or '').strip()
    p = str(a.get('password') or '').strip()
    try:
        limit = int(a.get('limit') or 1)
    except Exception:
        limit = 1
    expiry = str(a.get('expiry') or '').strip()[:10]
    expires_at = str(a.get('expires_at') or '').strip()
    is_trial = bool(a.get('is_trial'))
    access_expiry = access_expiry_date(expiry, expires_at, is_trial)
    uuid = str(a.get('uuid') or '').strip()
    xray_enabled = bool(a.get('xray_enabled'))
    if not valid.match(u) or not p or ':' in p or ' ' in p or not expiry:
        failed += 1
        detail(f"{u or '<vazio>'}: dados inválidos")
        continue
    try:
        # Primeiro tenta o criaruser do DragonCore, como a linha Python/024.
        created_by_dragoncore = try_dragoncore_cli_create(u, p, limit, access_expiry)
        if subprocess.run(['id','-u',u], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode != 0:
            created = False
            last_txt = ''
            attempts = [
                ['useradd','-M','-s','/bin/false','-e',access_expiry,u],
                ['useradd','--badname','-M','-s','/bin/false','-e',access_expiry,u],
                ['useradd','-M','-s','/usr/sbin/nologin','-e',access_expiry,u],
                ['useradd','--badname','-M','-s','/usr/sbin/nologin','-e',access_expiry,u],
                ['useradd','-M','-e',access_expiry,u],
                ['useradd','--badname','-M','-e',access_expiry,u],
            ]
            for cmd in attempts:
                r = sh(cmd, timeout=25)
                last_txt = (r.stderr or r.stdout or '').strip()
                if r.returncode == 0 or 'already exists' in last_txt.lower():
                    created = True
                    break
            if not created:
                raise RuntimeError('useradd: ' + last_txt)
        # Garante shell bloqueado, senha e validade mesmo quando DragonCore criou.
        subprocess.run(['usermod','-s','/bin/false',u], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
        r = sh(['chpasswd'], inp=f'{u}:{p}\n', timeout=25)
        if r.returncode != 0:
            raise RuntimeError('chpasswd: ' + (r.stderr or r.stdout).strip())
        subprocess.run(['passwd','-u',u], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
        subprocess.run(['usermod','-e',access_expiry,u], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
        subprocess.run(['chage','-E',access_expiry,u], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
        entries.append((u,p,limit,access_expiry,expires_at))
        desired_users.add(u.lower())
        if uuid and xray_enabled:
            xray_items.append((u,uuid,access_expiry))
            desired_xray.add(u.lower())
        pg_ok = sync_dragoncore_ssh_pg(u, p, limit)
        if pg_ok is True:
            ssh_pg_synced += 1
        elif pg_ok is False and dragoncore_present():
            # Não marca OK falso quando o menu SSH/DragonCore existe mas a tabela users não foi atualizada.
            failed += 1
        if write_checkuser_expiration(u, expiry, expires_at):
            checkuser_written += 1
        applied += 1
    except Exception as e:
        failed += 1
        detail(f"{u}: {e}")

# Remove sobras legadas derivadas de usuarios.db antigos.
# A secundária deve bater com o snapshot ativo do principal.
try:
    legacy_paths = ['/root/usuarios.db','/etc/primecel-gestor/usuarios.db','/etc/tg-access-bot/usuarios.db','/etc/SSHPlus/usuarios.db','/etc/sshplus/usuarios.db','/etc/DragonCore/usuarios.db','/opt/DragonCore/usuarios.db','/etc/adm-lite/usuarios.db','/etc/adm-manager/usuarios.db']
    legacy_seen = set()
    for db_path in legacy_paths:
        pth = Path(db_path)
        if not pth.exists():
            continue
        for line in pth.read_text(encoding='utf-8', errors='ignore').splitlines():
            parts = line.split()
            if not parts:
                continue
            old_u = parts[0].strip()
            key = old_u.lower()
            if key in legacy_seen or key in desired_users or not valid.match(old_u):
                continue
            legacy_seen.add(key)
            subprocess.run(['userdel','-f',old_u], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
            detail('removido legado ausente: ' + old_u)
except Exception as e:
    detail('prune legado: ' + str(e))

# Remove sobras do PostgreSQL DragonCore SSH/users quando possível.
try:
    if desired_users and subprocess.run(['sh','-lc','command -v psql >/dev/null 2>&1']).returncode == 0:
        keep = ','.join("'" + x.replace("'", "''") + "'" for x in sorted(desired_users))
        sql = 'DELETE FROM users WHERE lower(usr) NOT IN (' + keep + ');'
        for cmd in [
            ['sudo','-u','postgres','psql','-d','dragoncore','-v','ON_ERROR_STOP=1','-c',sql],
            ['psql','-d','dragoncore','-v','ON_ERROR_STOP=1','-c',sql],
        ]:
            r = sh(cmd, timeout=20)
            if r.returncode == 0:
                break
except Exception as e:
    detail('prune DragonCore users: ' + str(e))

# Atualiza usuarios.db no mesmo formato da linha Python/024.
# Mantém /root, espelho de dados do bot antigo e caminhos comuns de scripts SSH.
db_written = []
try:
    db_targets = ['/root/usuarios.db','/etc/primecel-gestor/usuarios.db','/etc/tg-access-bot/usuarios.db']
    for extra in ['/etc/SSHPlus/usuarios.db','/etc/sshplus/usuarios.db','/etc/DragonCore/usuarios.db','/opt/DragonCore/usuarios.db','/etc/adm-lite/usuarios.db','/etc/adm-manager/usuarios.db']:
        if os.path.isdir(os.path.dirname(extra)) and extra not in db_targets:
            db_targets.append(extra)
    data = ''.join(f'{u} {p} {l} {e}\n' for u,p,l,e,_ in entries)
    for db_path in db_targets:
        Path(os.path.dirname(db_path)).mkdir(parents=True, exist_ok=True)
        tmp = db_path + '.tmp'
        with open(tmp, 'w', encoding='utf-8') as f:
            f.write(data)
        os.chmod(tmp, 0o600)
        os.replace(tmp, db_path)
        db_written.append(db_path)
except Exception as e:
    failed += 1
    detail('usuarios.db: ' + str(e))

# Aplica Xray direto no config quando existir.
xray_config_updated = False
config_paths = ['/usr/local/etc/xray/config.json','/etc/xray/config.json','/opt/xray/config.json']
for cfg_path in config_paths:
    if not os.path.exists(cfg_path):
        continue
    try:
        with open(cfg_path, 'r', encoding='utf-8') as f:
            cfg = json.load(f)
        inbounds = cfg.get('inbounds') or []
        inbound = None
        for cand in inbounds:
            if cand.get('tag') == 'inbound-dragoncore':
                inbound = cand; break
        if inbound is None:
            for cand in inbounds:
                if str(cand.get('protocol','')).lower() == 'vless' and isinstance(cand.get('settings',{}).get('clients'), list):
                    inbound = cand; break
        if inbound is None:
            detail('xray: inbound VLESS não encontrado')
            break
        settings = inbound.setdefault('settings', {})
        clients = settings.setdefault('clients', [])
        changed = False
        pruned_clients = []
        for c in clients:
            email = str(c.get('email','')).strip() if isinstance(c, dict) else ''
            if isinstance(c, dict) and valid.match(email) and email.lower() not in desired_xray:
                changed = True
                continue
            pruned_clients.append(c)
        clients = pruned_clients
        settings['clients'] = clients
        for u,uuid,expiry in xray_items:
            found = False
            for c in clients:
                if str(c.get('email','')) == u:
                    found = True
                    if c.get('id') != uuid:
                        c['id'] = uuid; changed = True
                    c['level'] = 0
                    break
            if not found:
                clients.append({'id': uuid, 'email': u, 'level': 0})
                changed = True
        if changed:
            bak = cfg_path + '.bak-primecel-sync'
            try:
                if os.path.exists(cfg_path):
                    Path(bak).write_bytes(Path(cfg_path).read_bytes())
            except Exception:
                pass
            with open(cfg_path, 'w', encoding='utf-8') as f:
                json.dump(cfg, f, ensure_ascii=False, indent=2)
            xray_config_updated = True
            subprocess.run('systemctl restart xray || systemctl restart xray.service || systemctl restart v2ray || true', shell=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=40)
        break
    except Exception as e:
        failed += 1
        detail('xray config: ' + str(e))
        break

# Registra UUID no PostgreSQL do DragonCore, no mesmo formato do bot 024.
pg_registered = False
if xray_items:
    php_ok = False
    if os.path.exists('/opt/DragonCore/config.php') and subprocess.run(['sh','-lc','command -v php >/dev/null 2>&1']).returncode == 0:
        php_code = 'require_once "/opt/DragonCore/config.php";\n$conn = pg_connect("host=localhost dbname=dragoncore user={$db_user} password={$db_pass}");\nif (!$conn) { fwrite(STDERR, "pg_connect failed\\n"); exit(2); }\npg_query($conn, "CREATE TABLE IF NOT EXISTS xray (id SERIAL PRIMARY KEY, uuid TEXT, nick TEXT, expiry DATE, protocol TEXT)");\n$uuid = $argv[1];\n$nick = $argv[2];\n$expiry = $argv[3];\n$protocol = $argv[4];\n$res = pg_query_params($conn, "UPDATE xray SET uuid=$1, nick=$2, expiry=$3, protocol=$4 WHERE nick=$2 OR uuid=$1", array($uuid, $nick, $expiry, $protocol));\nif (!$res) { fwrite(STDERR, pg_last_error($conn)); exit(3); }\nif (pg_affected_rows($res) < 1) {\n  $res = pg_query_params($conn, "INSERT INTO xray (uuid, nick, expiry, protocol) VALUES ($1,$2,$3,$4)", array($uuid, $nick, $expiry, $protocol));\n  if (!$res) { fwrite(STDERR, pg_last_error($conn)); exit(4); }\n}\npg_close($conn);'
        ok_count = 0
        for u,uuid,expiry in xray_items:
            try:
                r = sh(['php','-r',php_code,uuid,u,expiry,'xhttp'], timeout=25)
                if r.returncode == 0:
                    ok_count += 1
                else:
                    detail(f'{u}: DragonCore xray PG: ' + ((r.stderr or r.stdout or '').strip()[:140]))
            except Exception as e:
                detail(f'{u}: DragonCore xray PG: {e}')
        php_ok = ok_count == len(xray_items)
        pg_registered = php_ok
    if not pg_registered and subprocess.run(['sh','-lc','command -v psql >/dev/null 2>&1']).returncode == 0:
        stmts = ['CREATE TABLE IF NOT EXISTS xray (id SERIAL PRIMARY KEY, uuid TEXT, nick TEXT, expiry DATE, protocol TEXT);']
        if desired_xray:
            keep = ','.join("'" + x.replace("'", "''") + "'" for x in sorted(desired_xray))
            stmts.append('DELETE FROM xray WHERE nick IS NOT NULL AND nick <> '' AND nick NOT IN (' + keep + ');')
        for u,uuid,expiry in xray_items:
            su = u.replace("'", "''"); sid = uuid.replace("'", "''"); se = expiry.replace("'", "''")
            stmts.append(f"DELETE FROM xray WHERE nick='{su}' OR uuid='{sid}'; INSERT INTO xray (uuid,nick,expiry,protocol) VALUES ('{sid}','{su}','{se}','xhttp');")
        sql = '\n'.join(stmts)
        attempts = [(['sudo','-u','postgres','psql','-d','dragoncore','-v','ON_ERROR_STOP=1','-c',sql], None), (['psql','-d','dragoncore','-v','ON_ERROR_STOP=1','-c',sql], None)]
        for cmd, envv in attempts:
            try:
                r = sh(cmd, timeout=35, env=envv)
                if r.returncode == 0:
                    pg_registered = True
                    break
            except Exception:
                pass
    if os.path.exists('/opt/DragonCore/config.php') and not pg_registered:
        failed += 1
        detail('DragonCore xray PG não confirmou UUIDs')

# Verificação forte: não marca OK se Linux/usuarios.db não refletirem o snapshot.
try:
    db_lines = 0
    if os.path.exists('/root/usuarios.db'):
        with open('/root/usuarios.db', 'r', encoding='utf-8', errors='ignore') as f:
            db_lines = sum(1 for line in f if line.strip())
    if db_lines < len(entries):
        failed += 1
        detail(f'usuarios.db incompleto: {db_lines}/{len(entries)} em /root/usuarios.db')
    missing_users = []
    for u,_,_,_,_ in entries[:20]:
        if subprocess.run(['id','-u',u], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode != 0:
            missing_users.append(u)
    if missing_users:
        failed += len(missing_users)
        detail('usuários Linux ausentes: ' + ', '.join(missing_users[:8]))
    if dragoncore_present():
        dc_count = dragoncore_users_count()
        if dc_count is not None and dc_count < len(entries):
            failed += 1
            detail(f'DragonCore users incompleto: {dc_count}/{len(entries)}')
except Exception as e:
    failed += 1
    detail('verificação: ' + str(e))

resp['applied'] = applied
resp['failed'] = failed
resp['ok'] = failed == 0
flags = []
if xray_config_updated: flags.append('xray_config')
if ssh_pg_synced: flags.append('dragoncore_ssh_users=' + str(ssh_pg_synced))
if checkuser_written: flags.append('checkuser_exp=' + str(checkuser_written))
if pg_registered: flags.append('dragoncore_xray_pg')
if db_written:
    flags.append('usuarios_db=' + ','.join(db_written[:3]))
resp['output'] = 'snapshot aplicado no Linux/usuarios.db' + ((' + ' + ','.join(flags)) if flags else '')
if failed:
    extra = '; '.join(resp.get('details', [])[:4])
    resp['error'] = 'falhas ao aplicar/verificar snapshot SSH' + ((': ' + extra) if extra else '')
print(json.dumps(resp, ensure_ascii=False))
sys.exit(0 if failed == 0 else 2)
PYEOF`, shellQuote(snapshotPath))
}

func remoteSecondaryEnvPatch() string {
	return `mkdir -p /etc/primecel-gestor
if [ ! -f /etc/primecel-gestor/config.env ]; then
  touch /etc/primecel-gestor/config.env
fi
set_env_line() {
  k="$1"; v="$2"
  if grep -q "^${k}=" /etc/primecel-gestor/config.env 2>/dev/null; then
    sed -i "s|^${k}=.*|${k}=${v}|" /etc/primecel-gestor/config.env
  else
    printf '%s=%s
' "$k" "$v" >> /etc/primecel-gestor/config.env
  fi
}
set_env_line BOT_DATA_DIR /etc/primecel-gestor
set_env_line GESTOR_DB_PATH /etc/primecel-gestor/gestor.db
set_env_line USUARIOS_DB_PATH /root/usuarios.db
set_env_line PRINCIPAL_MANAGER_ONLY 0
set_env_line XRAY_CREATE_ENABLED 1
chmod 600 /etc/primecel-gestor/config.env`
}

func (m *Manager) post(ctx context.Context, srv model.Server, req Request) (Response, error) {
	var respData Response
	b, _ := json.Marshal(req)
	url := fmt.Sprintf("http://%s:%d/sync", srv.Host, nonZero(srv.AgentPort, m.cfg.RemoteAgentPort, 8787))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return respData, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Token != "" {
		httpReq.Header.Set("X-Primecel-Agent-Token", req.Token)
	}
	res, err := m.hc.Do(httpReq)
	if err != nil {
		return respData, err
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&respData); err != nil {
		return respData, err
	}
	if res.StatusCode >= 400 {
		return respData, fmt.Errorf("agent http %d: %s", res.StatusCode, first(respData.Error, respData.Output))
	}
	return respData, nil
}

func shouldBootstrapAfterPost(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "no route to host") || strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "timeout")
}

func syncSSHVerifyEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("PRIMECEL_SYNC_SSH_VERIFY")))
	return v == "1" || v == "true" || v == "yes" || v == "sim" || v == "on"
}

func friendlySyncError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "token do agente inválido"):
		return "token do agente inválido"
	case strings.Contains(low, "connection refused"):
		return "agente remoto não está rodando ou porta fechada"
	case strings.Contains(low, "no route to host"):
		return "VPS inacessível pela rede"
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline exceeded"):
		return "timeout ao comunicar com a VPS"
	case strings.Contains(low, "sshpass não instalado"):
		return "sshpass não instalado no servidor principal"
	case strings.Contains(low, "senha ssh/root"):
		return msg
	}
	if len([]rune(msg)) > 160 {
		r := []rune(msg)
		msg = string(r[:160]) + "..."
	}
	return msg
}

func (m *Manager) ensureServerAgentToken(ctx context.Context, srv model.Server) model.Server {
	if strings.TrimSpace(srv.AgentToken) != "" || strings.TrimSpace(m.cfg.RemoteAgentToken) != "" {
		return srv
	}
	token := generateAgentToken()
	srv.AgentToken = token
	srv.UpdatedAt = time.Now().UTC()
	_ = m.st.UpsertServer(ctx, srv)
	return srv
}

func generateAgentToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("primecel-%d", time.Now().UnixNano())
}

func snapshotFromAccount(a model.Account) SnapshotAccess {
	return SnapshotAccess{Username: a.Username, Password: a.Password, Expiry: a.ExpiryDate, ExpiresAt: a.ExpiresAt.Format(time.RFC3339), Limit: a.LimitConnections, UUID: a.UUID, OwnerTelegramID: a.OwnerTelegramID, OwnerName: a.OwnerName, OwnerType: a.OwnerType, IsTrial: a.IsTrial, ClientWhatsApp: a.ClientWhatsApp, MonthlyValue: a.MonthlyValue, XrayEnabled: a.XrayEnabled}
}

func formatRemoteFailure(resp Response) string {
	base := first(resp.Error, resp.Output, "fallback SSH retornou falha")
	if len(resp.Details) > 0 {
		var parts []string
		for i, d := range resp.Details {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			parts = append(parts, d)
			if i >= 4 {
				break
			}
		}
		if len(parts) > 0 {
			base += ": " + strings.Join(parts, "; ")
		}
	}
	return base
}

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
