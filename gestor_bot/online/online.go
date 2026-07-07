package online

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
)

type Manager struct {
	cfg config.Config
	st  *store.DB
}

func NewManager(cfg config.Config, st *store.DB) *Manager { return &Manager{cfg: cfg, st: st} }

type Item struct {
	Username        string   `json:"username"`
	Connections     int      `json:"connections"`
	Limit           int      `json:"limit"`
	OwnerID         int64    `json:"owner_telegram_id"`
	OwnerName       string   `json:"owner_name"`
	OwnerType       string   `json:"owner_type"`
	Sources         []string `json:"sources"`
	Modes           []string `json:"modes,omitempty"`
	IP              string   `json:"ip,omitempty"`
	ConnectedTime   string   `json:"connected_time,omitempty"`
	DeviceAuthority bool     `json:"device_authority,omitempty"`
}

type Summary struct {
	OK    bool   `json:"ok"`
	Count int    `json:"count"`
	Users []Item `json:"users"`
}

// Summary retorna onlines locais + VPS secundárias. O agente remoto usa LocalSummary
// para evitar recursão; os menus do bot devem usar Summary.
func (m *Manager) Summary(ctx context.Context) (Summary, error) {
	base, err := m.LocalSummary(ctx)
	if err != nil {
		return Summary{}, err
	}
	accs, _ := m.st.ListAccounts(ctx, false)
	byName := accountsByName(accs)
	merged := map[string]*Item{}
	for _, it := range base.Users {
		mergeItem(merged, enrich(it, byName, "local"))
	}
	servers, _ := m.st.ListServers(ctx)
	type remoteResult struct {
		items []Item
	}
	results := make(chan remoteResult, len(servers))
	sem := make(chan struct{}, 6)
	launched := 0
	for _, srv := range servers {
		if !srv.Enabled || strings.TrimSpace(srv.Host) == "" {
			continue
		}
		srv := srv
		launched++
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			// Cada VPS secundária tem timeout próprio. Se uma VPS ficar lenta/offline,
			// ela não prende as demais nem sobrepõe o loop de atualização do Telegram.
			remoteCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
			defer cancel()
			remoteMerged := map[string]*Item{}
			if sum, err := m.remotePublicAgentSummary(remoteCtx, srv); err == nil && len(sum.Users) > 0 {
				for _, it := range sum.Users {
					mergeItemMax(remoteMerged, enrich(it, byName, ""))
				}
			} else {
				if sum, err := m.remoteCheckUserSummary(remoteCtx, srv, byName); err == nil && len(sum.Users) > 0 {
					for _, it := range sum.Users {
						mergeItemMax(remoteMerged, enrich(it, byName, sourceName(srv, "checkuser")))
					}
				}
				if sum, err := m.remoteAgentSummary(remoteCtx, srv); err == nil && len(sum.Users) > 0 {
					for _, it := range sum.Users {
						mergeItemMax(remoteMerged, enrich(it, byName, sourceName(srv, "agente")))
					}
				}
				if sum, err := m.remoteSSHSummary(remoteCtx, srv); err == nil && len(sum.Users) > 0 {
					for _, it := range sum.Users {
						mergeItemMax(remoteMerged, enrich(it, byName, sourceName(srv, "ssh")))
					}
				}
			}
			items := make([]Item, 0, len(remoteMerged))
			for _, it := range remoteMerged {
				if it != nil {
					items = append(items, *it)
				}
			}
			results <- remoteResult{items: items}
		}()
	}
	for i := 0; i < launched; i++ {
		select {
		case res := <-results:
			for _, it := range res.items {
				mergeItem(merged, it)
			}
		case <-ctx.Done():
			return finalize(merged), nil
		}
	}
	return finalize(merged), nil
}

func (m *Manager) AgentPublicSummary(ctx context.Context) (Summary, error) {
	// Leitura pública do agente para /onlines.
	// Mantém o fluxo do bot intacto; esta função só lê fontes locais e nunca sincroniza,
	// cria, remove ou reinicia serviços.
	accs, err := m.st.ListAccounts(ctx, false)
	if err != nil {
		return Summary{}, err
	}
	byName := map[string]model.Account{}
	uuidToName := map[string]string{}
	hasXrayIdentity := false
	for _, a := range accs {
		if a.DeletedAt != nil || a.Status == "deleted" || strings.TrimSpace(a.Username) == "" {
			continue
		}
		key := strings.ToLower(a.Username)
		byName[key] = a
		if strings.TrimSpace(a.UUID) != "" {
			uuidToName[strings.ToLower(a.UUID)] = a.Username
			hasXrayIdentity = true
		}
	}
	m.extendSSHAccountCandidates(ctx, byName)

	// Fonte autoritativa do agente público:
	// a porta 81/Proto, quando disponível, já é o detector vivo da própria VPS.
	// Não mistura com heurística SSH local, porque isso foi a origem dos falsos 2/1
	// e também de casos 2 conexões reais aparecendo como 1.
	protoOut := seedAccountItems(accs)
	m.detectPort81Public(ctx, byName, protoOut)
	if summaryHasUsers(protoOut) {
		return finalize(protoOut), nil
	}

	// Fallback somente quando a porta 81 não existe, está vazia ou não responde.
	out := seedAccountItems(accs)
	m.detectSSH(ctx, byName, out)
	if hasXrayIdentity && m.xrayServiceActive(ctx) {
		m.detectXrayLogs(ctx, byName, uuidToName, out)
	}
	return finalize(out), nil
}

func summaryHasUsers(m map[string]*Item) bool {
	for _, it := range m {
		if it != nil && strings.TrimSpace(it.Username) != "" && it.Connections > 0 && !isSystemOnlineUsername(it.Username) {
			return true
		}
	}
	return false
}

func (m *Manager) detectPort81Public(ctx context.Context, byName map[string]model.Account, out map[string]*Item) {
	for _, path := range checkUserOnlinePaths(81) {
		sum, err := m.getSummaryURL(ctx, "http://127.0.0.1:81"+withLocalOnlineScope(path), "")
		if err != nil || len(sum.Users) == 0 {
			continue
		}
		for _, it := range sum.Users {
			it.Sources = mergeStringLists(nil, []string{"proto"})
			it.Modes = mergeStringLists(it.Modes, []string{"PROTO"})
			if e := enrich(it, byName, "proto"); e.Username != "" {
				mergeItemMax(out, e)
				continue
			}
			// Mantém compatibilidade com a porta 81: se a VPS ainda não tiver SQLite local
			// completo, retorna o usuário bruto para o principal filtrar/enriquecer depois.
			if it.Limit <= 0 {
				it.Limit = maxInt(1, it.Connections)
			}
			mergeItemMax(out, it)
		}
		return
	}
}

func (m *Manager) extendSSHAccountCandidates(ctx context.Context, byName map[string]model.Account) {
	b, err := runOutput(ctx, 1200*time.Millisecond, "getent", "passwd")
	if err != nil || len(b) == 0 {
		b, err = os.ReadFile("/etc/passwd")
		if err != nil {
			return
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) < 7 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		key := strings.ToLower(name)
		if key == "" || isSystemOnlineUsername(key) {
			continue
		}
		uid, _ := strconv.Atoi(parts[2])
		shell := strings.ToLower(strings.TrimSpace(parts[6]))
		if uid < 1000 && !strings.Contains(shell, "bash") && !strings.Contains(shell, "sh") && !strings.Contains(shell, "false") {
			continue
		}
		if strings.Contains(shell, "nologin") || strings.Contains(shell, "false") {
			// Dragon/SSH VPN às vezes usa shell restrito, mas usuário conectado ainda aparece em ps.
			// Mantém a conta como candidata; a filtragem final só inclui quem tiver sessão viva.
		}
		if _, ok := byName[key]; !ok {
			byName[key] = model.Account{Username: name, LimitConnections: 1, Status: "active"}
		}
	}
}

func (m *Manager) xrayServiceActive(ctx context.Context) bool {
	for _, unit := range []string{"xray", "xray.service", "v2ray", "v2ray.service", "dragoncore", "dragoncore.service"} {
		if _, err := runOutput(ctx, 1200*time.Millisecond, "systemctl", "is-active", "--quiet", unit); err == nil {
			return true
		}
	}
	if b, err := runOutput(ctx, 1200*time.Millisecond, "pgrep", "-fa", "xray|v2ray|dragoncore"); err == nil && strings.TrimSpace(string(b)) != "" {
		return true
	}
	return false
}

func (m *Manager) LocalSummary(ctx context.Context) (Summary, error) {
	accs, err := m.st.ListAccounts(ctx, false)
	if err != nil {
		return Summary{}, err
	}
	byName := map[string]model.Account{}
	uuidToName := map[string]string{}
	for _, a := range accs {
		if a.DeletedAt != nil || a.Status == "deleted" {
			continue
		}
		key := strings.ToLower(a.Username)
		byName[key] = a
		if a.UUID != "" {
			uuidToName[strings.ToLower(a.UUID)] = a.Username
		}
	}
	out := seedAccountItems(accs)
	runDetector := func(fn func(map[string]*Item)) {
		tmp := seedAccountItems(accs)
		fn(tmp)
		mergeAllMax(out, tmp)
	}
	// Onlines locais seguem a referência v024: somente sessões reais SSH/Dropbear
	// e Xray/V2Ray recentes/ativas. Tabela devices é registro de aparelho, não
	// prova conexão atual, então não entra na contagem online.
	runDetector(func(tmp map[string]*Item) { m.detectSSH(ctx, byName, tmp) })
	runDetector(func(tmp map[string]*Item) { m.detectXrayLogs(ctx, byName, uuidToName, tmp) })
	return finalize(out), nil
}

func (m *Manager) detectCheckUserDevices(ctx context.Context, byName map[string]model.Account, uuidToName map[string]string, out map[string]*Item) {
	devices, err := m.st.ListDevices(ctx, "")
	if err != nil {
		return
	}
	counts := map[string]map[string]bool{}
	for idx, d := range devices {
		name := resolveAccountName(d.Username, d.UserUUID, byName, uuidToName)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if counts[key] == nil {
			counts[key] = map[string]bool{}
		}
		devID := strings.TrimSpace(d.ID)
		if devID == "" {
			devID = fmt.Sprintf("row:%d:%s:%s", idx, d.Username, d.UserUUID)
		}
		counts[key][strings.ToLower(devID)] = true
	}
	for key, ids := range counts {
		if len(ids) <= 0 {
			continue
		}
		if out[key] == nil {
			a := byName[key]
			out[key] = &Item{Username: a.Username, Limit: nonZero(a.LimitConnections, 1), OwnerID: a.OwnerTelegramID, OwnerName: a.OwnerName, OwnerType: a.OwnerType}
		}
		addN(out[key], len(ids), "checkuser-devices")
		out[key].DeviceAuthority = true
	}
}

func (m *Manager) detectLegacyCheckUserDB(ctx context.Context, byName map[string]model.Account, uuidToName map[string]string, out map[string]*Item) {
	path := strings.TrimSpace(m.cfg.CheckUserDBPath)
	if path == "" || path == m.cfg.DBPath {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	queries := []string{
		`SELECT username,COALESCE(user_uuid,'') AS uuid,COUNT(DISTINCT id) AS n FROM devices GROUP BY lower(username),lower(COALESCE(user_uuid,''));`,
		`SELECT username,COALESCE(user_uuid,'') AS uuid,COUNT(DISTINCT device_id) AS n FROM devices GROUP BY lower(username),lower(COALESCE(user_uuid,''));`,
		`SELECT username,COALESCE(user_uuid,'') AS uuid,COUNT(DISTINCT deviceid) AS n FROM devices GROUP BY lower(username),lower(COALESCE(user_uuid,''));`,
		`SELECT username,COALESCE(user_uuid,'') AS uuid,COUNT(DISTINCT hwid) AS n FROM devices GROUP BY lower(username),lower(COALESCE(user_uuid,''));`,
		`SELECT username,COALESCE(user_uuid,'') AS uuid,COUNT(*) AS n FROM devices GROUP BY lower(username),lower(COALESCE(user_uuid,''));`,
		`SELECT user AS username,'' AS uuid,COUNT(DISTINCT id) AS n FROM devices GROUP BY lower(user);`,
		`SELECT user AS username,'' AS uuid,COUNT(DISTINCT device_id) AS n FROM devices GROUP BY lower(user);`,
		`SELECT login AS username,'' AS uuid,COUNT(DISTINCT id) AS n FROM devices GROUP BY lower(login);`,
		`SELECT username,'' AS uuid,COUNT(DISTINCT id) AS n FROM device GROUP BY lower(username);`,
		`SELECT user AS username,'' AS uuid,COUNT(DISTINCT id) AS n FROM device GROUP BY lower(user);`,
	}
	for _, q := range queries {
		lines, ok := runSQLiteRows(ctx, path, q)
		if !ok {
			continue
		}
		for _, line := range lines {
			parts := strings.Split(line, "|")
			if len(parts) < 3 {
				continue
			}
			name := resolveAccountName(parts[0], parts[1], byName, uuidToName)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if out[key] == nil {
				a := byName[key]
				out[key] = &Item{Username: a.Username, Limit: nonZero(a.LimitConnections, 1), OwnerID: a.OwnerTelegramID, OwnerName: a.OwnerName, OwnerType: a.OwnerType}
			}
			addN(out[key], atoi(parts[2]), "checkuser-db")
			out[key].DeviceAuthority = true
		}
		return
	}
}

func runSQLiteRows(ctx context.Context, path, query string) ([]string, bool) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sqlite3", "-noheader", "-separator", "|", path, query)
	b, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out, len(out) > 0
}

func (m *Manager) detectLocalCheckUserHTTP(ctx context.Context, byName map[string]model.Account, out map[string]*Item) {
	for _, port := range uniqueInts(81, m.cfg.CheckUserPort, 2052) {
		for _, path := range checkUserOnlinePaths(port) {
			sum, err := m.getSummaryURL(ctx, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), "")
			if err != nil || len(sum.Users) == 0 {
				continue
			}
			for _, it := range sum.Users {
				it = enrich(it, byName, fmt.Sprintf("checkuser:%d", port))
				mergeItem(out, it)
			}
			return
		}
	}
}

func (m *Manager) detectSSH(ctx context.Context, byName map[string]model.Account, out map[string]*Item) {
	counts := m.detectSSHCounts(ctx, byName)
	for key, n := range counts {
		if n <= 0 {
			continue
		}
		if out[key] == nil {
			a := byName[key]
			name := strings.TrimSpace(a.Username)
			if name == "" {
				name = key
			}
			out[key] = &Item{Username: name, Limit: nonZero(a.LimitConnections, 1), OwnerID: a.OwnerTelegramID, OwnerName: a.OwnerName, OwnerType: a.OwnerType}
		}
		addN(out[key], n, "ssh")
	}
}

func (m *Manager) detectSSHCounts(ctx context.Context, byName map[string]model.Account) map[string]int {
	counts := map[string]int{}
	if len(byName) == 0 {
		return counts
	}

	procs := m.readSSHProcessTable(ctx)
	privSessions := map[string]map[string]bool{}
	childSessions := map[string]map[string]bool{}
	procUserSessions := map[string]map[string]bool{}
	whoSessions := map[string]map[string]bool{}
	socketSessions := map[string]map[string]bool{}

	for _, proc := range procs {
		lowArgs := strings.ToLower(proc.args)
		if !strings.Contains(lowArgs, "sshd") && !strings.Contains(lowArgs, "dropbear") {
			continue
		}
		if key := sshPrivUsername(proc.args, byName); key != "" {
			addSession(privSessions, key, fmt.Sprintf("priv:%d", proc.pid))
		}
		if key := sshChildUsername(proc.args, byName); key != "" {
			addSession(childSessions, key, sshProcessSessionID(proc, procs, "child"))
		}
		if key := knownAccountKey(proc.user, byName); key != "" {
			addSession(procUserSessions, key, sshProcessSessionID(proc, procs, "proc"))
		}
	}

	if b, err := runOutput(ctx, 1500*time.Millisecond, "who"); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) == 0 {
				continue
			}
			key := knownAccountKey(fields[0], byName)
			if key == "" {
				continue
			}
			sid := "who:" + fields[0]
			if len(fields) > 1 {
				sid += ":" + fields[1]
			}
			addSession(whoSessions, key, sid)
		}
	}

	m.detectSSHSockets(ctx, byName, procs, socketSessions)

	for key := range byName {
		if n := sshPreferredSessionCount(socketSessions[key], privSessions[key], childSessions[key], procUserSessions[key], whoSessions[key]); n > 0 {
			counts[key] = n
		}
	}
	return counts
}

func sshPreferredSessionCount(socket, priv, child, procUser, who map[string]bool) int {
	// Prioridade conservadora para evitar conexão fantasma:
	// 1) socket TCP estabelecido é a melhor evidência de sessão real;
	// 2) se [priv] e filho normalizado concordarem em um número maior que o socket,
	//    aceita esse número para não prender 2 aparelhos em 1/1;
	// 3) filho/proc/who só entram como fallback quando as fontes mais confiáveis
	//    não aparecerem.
	s, p, c, pu, w := len(socket), len(priv), len(child), len(procUser), len(who)
	if s > 0 {
		if p > s && p == c {
			return p
		}
		return s
	}
	if p > 0 {
		return p
	}
	if c > 0 {
		if w > 0 && w < c {
			return w
		}
		return c
	}
	if pu > 0 {
		return pu
	}
	return w
}

func maxLen(maps ...map[string]bool) int {
	max := 0
	for _, m := range maps {
		if len(m) > max {
			max = len(m)
		}
	}
	return max
}

func sshProcessSessionID(proc sshProcInfo, procs []sshProcInfo, prefix string) string {
	byPID := map[int]sshProcInfo{}
	for _, p := range procs {
		byPID[p.pid] = p
	}
	pid := proc.pid
	seen := map[int]bool{}
	for i := 0; i < 8 && pid > 1 && !seen[pid]; i++ {
		seen[pid] = true
		p, ok := byPID[pid]
		if !ok {
			break
		}
		if strings.Contains(strings.ToLower(p.args), "[priv]") {
			return prefix + ":priv:" + strconv.Itoa(p.pid)
		}
		pid = p.ppid
	}
	if proc.ppid > 1 {
		return prefix + ":ppid:" + strconv.Itoa(proc.ppid)
	}
	return prefix + ":pid:" + strconv.Itoa(proc.pid)
}

type sshProcInfo struct {
	pid  int
	ppid int
	user string
	args string
}

func (m *Manager) readSSHProcessTable(ctx context.Context) []sshProcInfo {
	outputs := []string{}
	if b, err := runOutput(ctx, 2*time.Second, "ps", "-eo", "pid=,ppid=,user=,args="); err == nil {
		outputs = append(outputs, string(b))
	}
	if len(outputs) == 0 {
		if b, err := runOutput(ctx, 2*time.Second, "ps", "-ef"); err == nil {
			outputs = append(outputs, psEFToTable(string(b)))
		}
	}
	seen := map[int]bool{}
	var procs []sshProcInfo
	for _, out := range outputs {
		sc := bufio.NewScanner(strings.NewReader(out))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 4 {
				continue
			}
			pid, err1 := strconv.Atoi(fields[0])
			ppid, err2 := strconv.Atoi(fields[1])
			if err1 != nil || err2 != nil || pid <= 0 || seen[pid] {
				continue
			}
			seen[pid] = true
			procs = append(procs, sshProcInfo{pid: pid, ppid: ppid, user: strings.ToLower(fields[2]), args: strings.Join(fields[3:], " ")})
		}
	}
	return procs
}

func psEFToTable(text string) string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(strings.ToUpper(line), "UID ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s %s %s", fields[1], fields[2], fields[0], strings.Join(fields[7:], " ")))
	}
	return strings.Join(out, "\n")
}

func (m *Manager) detectSSHSockets(ctx context.Context, byName map[string]model.Account, procs []sshProcInfo, sessions map[string]map[string]bool) {
	procByPID := map[int]sshProcInfo{}
	for _, p := range procs {
		procByPID[p.pid] = p
	}
	consume := func(text string, netstat bool) {
		sc := bufio.NewScanner(strings.NewReader(text))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			low := strings.ToLower(line)
			if line == "" || (!strings.Contains(low, "sshd") && !strings.Contains(low, "dropbear")) {
				continue
			}
			peer := socketPeerEndpoint(line, netstat)
			for _, pid := range socketPIDs(line) {
				key := usernameForSSHPID(pid, procByPID, byName)
				if key == "" {
					continue
				}
				sid := sshSessionIDForPID(pid, procByPID, peer)
				addSession(sessions, key, "sock:"+sid)
			}
		}
	}
	if b, err := runOutput(ctx, 2*time.Second, "ss", "-Htanp", "state", "established"); err == nil {
		consume(string(b), false)
	}
	if b, err := runOutput(ctx, 2500*time.Millisecond, "netstat", "-tnpa"); err == nil {
		consume(string(b), true)
	}
}

func socketPIDs(line string) []int {
	seen := map[int]bool{}
	var out []int
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`pid=(\d+)`), regexp.MustCompile(`\b(\d+)/(?:sshd|dropbear)\b`)} {
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			if len(m) < 2 {
				continue
			}
			pid, _ := strconv.Atoi(m[1])
			if pid > 0 && !seen[pid] {
				seen[pid] = true
				out = append(out, pid)
			}
		}
	}
	return out
}

func socketPeerEndpoint(line string, netstat bool) string {
	fields := strings.Fields(line)
	if netstat {
		if len(fields) >= 5 {
			return fields[4]
		}
		return ""
	}
	if len(fields) >= 5 && strings.HasPrefix(strings.ToUpper(fields[0]), "ESTAB") {
		return fields[4]
	}
	if len(fields) >= 4 && isNumericField(fields[0]) && isNumericField(fields[1]) {
		return fields[3]
	}
	return ""
}

func isNumericField(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func sshSessionIDForPID(pid int, procs map[int]sshProcInfo, fallback string) string {
	seen := map[int]bool{}
	cur := pid
	for i := 0; i < 8 && cur > 1 && !seen[cur]; i++ {
		seen[cur] = true
		p, ok := procs[cur]
		if !ok {
			break
		}
		if strings.Contains(strings.ToLower(p.args), "[priv]") {
			return "priv:" + strconv.Itoa(p.pid)
		}
		cur = p.ppid
	}
	fallback = strings.TrimSpace(fallback)
	if fallback != "" && fallback != "*" && !strings.HasPrefix(fallback, "0.0.0.0") {
		return "peer:" + fallback
	}
	return fmt.Sprintf("pid:%d", pid)
}

func usernameForSSHPID(pid int, procs map[int]sshProcInfo, byName map[string]model.Account) string {
	seen := map[int]bool{}
	for i := 0; i < 8 && pid > 1 && !seen[pid]; i++ {
		seen[pid] = true
		p, ok := procs[pid]
		if !ok {
			break
		}
		if key := sshPrivUsername(p.args, byName); key != "" {
			return key
		}
		if key := sshChildUsername(p.args, byName); key != "" {
			return key
		}
		if key := knownAccountKey(p.user, byName); key != "" {
			return key
		}
		pid = p.ppid
	}
	return ""
}

func sshPrivUsername(args string, byName map[string]model.Account) string {
	re := regexp.MustCompile(`(?i)\b(?:sshd|dropbear):\s*([A-Za-z0-9_.-]{2,64})\s+\[priv\]`)
	if m := re.FindStringSubmatch(args); len(m) > 1 {
		return knownAccountKey(m[1], byName)
	}
	return ""
}

func sshChildUsername(args string, byName map[string]model.Account) string {
	re := regexp.MustCompile(`(?i)\b(?:sshd|dropbear):\s*([A-Za-z0-9_.-]{2,64})@(?:notty|pts/\d+|tty\d+)`)
	if m := re.FindStringSubmatch(args); len(m) > 1 {
		return knownAccountKey(m[1], byName)
	}
	return ""
}

func knownAccountKey(username string, byName map[string]model.Account) string {
	key := strings.ToLower(strings.TrimSpace(username))
	if key == "" || isSystemOnlineUsername(key) {
		return ""
	}
	if _, ok := byName[key]; ok {
		return key
	}
	return ""
}

func addSession(dst map[string]map[string]bool, key, session string) {
	key = strings.ToLower(strings.TrimSpace(key))
	session = strings.TrimSpace(session)
	if key == "" || session == "" {
		return
	}
	if dst[key] == nil {
		dst[key] = map[string]bool{}
	}
	dst[key][session] = true
}

var identityPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bemail\s*[:=]\s*["']?([A-Za-z0-9_.@-]{3,100})`),
	regexp.MustCompile(`(?i)\buser(?:name)?\s*[:=]\s*["']?([A-Za-z0-9_.@-]{3,100})`),
	regexp.MustCompile(`(?i)\bnick\s*[:=]\s*["']?([A-Za-z0-9_.@-]{3,100})`),
	regexp.MustCompile(`(?i)\b(?:uuid|id)\s*[:=]\s*["']?([0-9a-fA-F-]{32,36})`),
	regexp.MustCompile(`\[([A-Za-z0-9_.@-]{3,100})\]`),
	regexp.MustCompile(`(?i)"email"\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`(?i)"user(?:name)?"\s*:\s*"([^"]+)"`),
}
var uuidPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
var strictUUIDPattern = regexp.MustCompile(`(?i)\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\b`)

type datePattern struct {
	re     *regexp.Regexp
	layout string
}

var logDatePatterns = []datePattern{
	{regexp.MustCompile(`^(\d{4}/\d{2}/\d{2}\s+\d{2}:\d{2}:\d{2})`), "2006/01/02 15:04:05"},
	{regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2})`), "2006-01-02 15:04:05"},
	{regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})`), "2006-01-02T15:04:05"},
}

var sourcePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bfrom\s+([^\s]+)`),
	regexp.MustCompile(`(?i)\baccepted\s+[^\s]+\s+([^\s]+)`),
}

func (m *Manager) detectXrayLogs(ctx context.Context, byName map[string]model.Account, uuidToName map[string]string, out map[string]*Item) {
	windowSeconds := nonZero(m.cfg.OnlineXrayWindowSeconds, 30)
	if windowSeconds < 10 {
		windowSeconds = 10
	}
	if windowSeconds > 90 {
		windowSeconds = 90
	}
	window := time.Duration(windowSeconds) * time.Second
	since := time.Now().Add(-window)
	activePeerIPs := m.activeXrayPeerIPs(ctx)
	perUserSources := map[string]map[string]bool{}

	consumeLine := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		dt, ok := parseLogTime(line)
		if !ok {
			dt = time.Now()
		}
		name := extractName(line, byName, uuidToName)
		if name == "" {
			return
		}
		sourceIP := extractSourceIP(line)
		isRecent := !dt.Before(since)
		isSocketActive := sourceIP != "" && activePeerIPs[sourceIP]
		if !isRecent && !isSocketActive {
			return
		}
		if len(activePeerIPs) > 0 && sourceIP != "" && !activePeerIPs[sourceIP] && !isRecent {
			return
		}
		key := strings.ToLower(name)
		if perUserSources[key] == nil {
			perUserSources[key] = map[string]bool{}
		}
		// Conta aparelhos distintos pelo IP ativo quando disponível. Linhas de log
		// sem IP não devem criar múltiplas conexões artificiais para o mesmo usuário.
		source := sourceIP
		if source == "" {
			source = "xray"
		}
		perUserSources[key][source] = true
	}

	for _, path := range m.discoverXrayAccessLogs() {
		b, err := tailFile(path, 512*1024)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		limit := 2500
		if len(lines) < limit {
			limit = len(lines)
		}
		for i := len(lines) - 1; i >= 0 && len(lines)-i <= limit; i-- {
			consumeLine(lines[i])
		}
	}
	for _, line := range m.readXrayJournalLines(ctx, windowSeconds) {
		consumeLine(line)
	}
	for key, sources := range perUserSources {
		count := len(sources)
		if count <= 0 {
			count = 1
		}
		addN(out[key], count, "xray")
	}
}

func parseLogTime(line string) (time.Time, bool) {
	for _, p := range logDatePatterns {
		m := p.re.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		if dt, err := time.ParseInLocation(p.layout, m[1], time.Local); err == nil {
			return dt, true
		}
	}
	return time.Time{}, false
}

func extractName(line string, byName map[string]model.Account, uuidToName map[string]string) string {
	resolve := func(raw string) string {
		key := strings.ToLower(strings.Trim(raw, `[](){}<>,;:'" `))
		if key == "" {
			return ""
		}
		if name := uuidToName[key]; name != "" {
			return name
		}
		if a, ok := byName[key]; ok {
			return a.Username
		}
		return ""
	}
	for _, re := range identityPatterns {
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			if len(m) > 1 {
				if name := resolve(m[1]); name != "" {
					return name
				}
			}
		}
	}
	for _, re := range []*regexp.Regexp{strictUUIDPattern, uuidPattern} {
		for _, u := range re.FindAllString(line, -1) {
			if name := resolve(u); name != "" {
				return name
			}
		}
	}
	low := strings.ToLower(line)
	keys := make([]string, 0, len(byName))
	for key := range byName {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, key := range keys {
		if containsIdentity(low, key) {
			return byName[key].Username
		}
	}
	return ""
}

func containsIdentity(line, key string) bool {
	if key == "" {
		return false
	}
	pattern := `(?i)(^|[^A-Za-z0-9_.@-])` + regexp.QuoteMeta(key) + `([^A-Za-z0-9_.@-]|$)`
	return regexp.MustCompile(pattern).FindStringIndex(line) != nil
}

func extractSource(line string) string {
	for _, re := range sourcePatterns {
		if m := re.FindStringSubmatch(line); len(m) > 1 {
			return strings.TrimSpace(m[1])
		}
	}
	return "xray"
}

func extractSourceIP(line string) string {
	if ip := endpointIP(extractSource(line)); ip != "" && ip != "xray" {
		return ip
	}
	ipv4 := regexp.MustCompile(`\b((?:\d{1,3}\.){3}\d{1,3})\b`)
	if m := ipv4.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	return ""
}

func splitEndpoint(endpoint string) (string, int) {
	raw := strings.Trim(strings.TrimSpace(endpoint), `"`)
	if raw == "" {
		return "", 0
	}
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "tcp:"), "tcp6:")
	if i := strings.Index(raw, "%"); i >= 0 {
		if j := strings.Index(raw[i:], ":"); j >= 0 {
			raw = raw[:i] + raw[i+j:]
		}
	}
	host := raw
	port := 0
	portRaw := ""
	if strings.HasPrefix(raw, "[") && strings.Contains(raw, "]:") {
		parts := strings.SplitN(strings.TrimPrefix(raw, "["), "]:", 2)
		host, portRaw = parts[0], parts[1]
	} else if strings.Count(raw, ":") == 1 {
		parts := strings.SplitN(raw, ":", 2)
		host, portRaw = parts[0], parts[1]
	} else if strings.Count(raw, ":") > 1 {
		idx := strings.LastIndex(raw, ":")
		if idx > -1 && idx < len(raw)-1 {
			host, portRaw = raw[:idx], raw[idx+1:]
		}
	}
	if n, err := strconv.Atoi(portRaw); err == nil {
		port = n
	}
	host = strings.Trim(host, "[]")
	if host == "*" || host == "::" || host == "0.0.0.0" {
		host = ""
	}
	host = strings.TrimPrefix(host, "::ffff:")
	return host, port
}

func endpointIP(endpoint string) string {
	host, _ := splitEndpoint(endpoint)
	return host
}

func (m *Manager) discoverXrayPorts() map[int]bool {
	ports := map[int]bool{}
	if m.cfg.Xray.LinkPort > 0 && m.cfg.Xray.LinkPort <= 65535 {
		ports[m.cfg.Xray.LinkPort] = true
	}
	for _, path := range m.cfg.Xray.ConfigPaths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var data map[string]any
		if json.Unmarshal(b, &data) != nil {
			continue
		}
		arr, _ := data["inbounds"].([]any)
		for _, v := range arr {
			mp, _ := v.(map[string]any)
			if mp == nil {
				continue
			}
			switch p := mp["port"].(type) {
			case float64:
				if p > 0 && p <= 65535 {
					ports[int(p)] = true
				}
			case string:
				if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 && n <= 65535 {
					ports[n] = true
				}
			}
		}
	}
	return ports
}

func (m *Manager) activeXrayPeerIPs(ctx context.Context) map[string]bool {
	ports := m.discoverXrayPorts()
	if len(ports) == 0 {
		return map[string]bool{}
	}
	peers := map[string]bool{}
	consume := func(output string) {
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) == 0 {
				continue
			}
			localEp, peerEp := "", ""
			if len(fields) >= 5 && strings.HasPrefix(strings.ToUpper(fields[0]), "ESTAB") {
				localEp, peerEp = fields[3], fields[4]
			} else if len(fields) >= 6 && strings.HasPrefix(fields[0], "tcp") && strings.ToUpper(fields[len(fields)-1]) == "ESTABLISHED" {
				localEp, peerEp = fields[3], fields[4]
			}
			if localEp == "" || peerEp == "" {
				continue
			}
			_, localPort := splitEndpoint(localEp)
			if !ports[localPort] {
				continue
			}
			if ip := endpointIP(peerEp); ip != "" {
				peers[ip] = true
			}
		}
	}
	if b, err := runOutput(ctx, 4*time.Second, "ss", "-Htan", "state", "established"); err == nil {
		consume(string(b))
	} else if b, err := runOutput(ctx, 5*time.Second, "netstat", "-tan"); err == nil {
		consume(string(b))
	}
	return peers
}

func (m *Manager) discoverXrayAccessLogs() []string {
	seen := map[string]bool{}
	var paths []string
	add := func(v any) {
		raw := strings.Trim(strings.TrimSpace(fmt.Sprint(v)), `"'`)
		low := strings.ToLower(raw)
		if raw == "" || low == "none" || low == "off" || low == "false" || low == "null" || low == "stdout" || low == "stderr" || seen[raw] {
			return
		}
		seen[raw] = true
		paths = append(paths, raw)
	}
	for _, p := range m.cfg.Xray.AccessLogPaths {
		add(p)
	}
	for _, p := range []string{"/var/log/xray/access.log", "/usr/local/var/log/xray/access.log", "/usr/local/etc/xray/access.log", "/var/log/v2ray/access.log", "/var/log/dragoncore/access.log", "/opt/DragonCore/access.log", "/opt/DragonCore/logs/access.log"} {
		add(p)
	}
	for _, path := range m.cfg.Xray.ConfigPaths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var data map[string]any
		if json.Unmarshal(b, &data) != nil {
			continue
		}
		if logCfg, _ := data["log"].(map[string]any); logCfg != nil {
			add(logCfg["access"])
		}
	}
	return paths
}

func (m *Manager) readXrayJournalLines(ctx context.Context, windowSeconds int) []string {
	minutes := windowSeconds/60 + 1
	if minutes < 1 {
		minutes = 1
	}
	args := []string{}
	for _, unit := range []string{"xray", "xray.service", "xray@*", "xray*.service", "v2ray", "v2ray.service", "v2ray@*", "v2ray*.service", "dragoncore", "dragoncore.service", "dragoncore*.service"} {
		args = append(args, "-u", unit)
	}
	args = append(args, "--since", fmt.Sprintf("-%d min", minutes), "-n", "8000", "--no-pager", "-o", "short-iso")
	b, err := runOutput(ctx, 12*time.Second, "journalctl", args...)
	if err != nil {
		return nil
	}
	return strings.Split(string(b), "\n")
}

func runOutput(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return exec.CommandContext(cctx, name, args...).Output()
}

func tailFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := st.Size() - max
	if start < 0 {
		start = 0
	}
	_, _ = f.Seek(start, 0)
	if start > 0 {
		_, _ = f.Read(make([]byte, 1))
	}
	return io.ReadAll(f)
}

func (m *Manager) remoteAgentSummary(ctx context.Context, srv model.Server) (Summary, error) {
	port := nonZero(srv.AgentPort, m.cfg.RemoteAgentPort, 8787)
	url := fmt.Sprintf("http://%s:%d/online-summary", srv.Host, port)
	return m.getSummaryURL(ctx, url, first(srv.AgentToken, m.cfg.RemoteAgentToken))
}

func (m *Manager) remotePublicAgentSummary(ctx context.Context, srv model.Server) (Summary, error) {
	port := nonZero(srv.AgentPort, m.cfg.RemoteAgentPort, 8787)
	url := fmt.Sprintf("http://%s:%d/onlines", srv.Host, port)
	return m.getSummaryURL(ctx, url, "")
}

func (m *Manager) remoteSSHSummary(ctx context.Context, srv model.Server) (Summary, error) {
	if strings.TrimSpace(srv.SSHPassword) == "" || strings.TrimSpace(srv.Host) == "" {
		return Summary{}, fmt.Errorf("ssh sem senha")
	}
	sshPort := nonZero(srv.SSHPort, 22)
	sshUser := first(srv.SSHUser, "root")
	remote := sshUser + "@" + strings.TrimSpace(srv.Host)
	req := map[string]any{"action": "online-summary"}
	b, _ := json.Marshal(req)
	remoteReq := strings.ReplaceAll(string(b), "'", "'\\''")
	cmd := "tmp=$(mktemp /tmp/primecel-online-XXXXXX.json); printf '%s' '" + remoteReq + "' > $tmp; " +
		"if command -v primecel-gestor >/dev/null 2>&1; then primecel-gestor sync apply-local --file $tmp; else /usr/local/bin/primecel-gestor sync apply-local --file $tmp; fi; rc=$?; rm -f $tmp; exit $rc"
	cctx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	args := []string{"-p", srv.SSHPassword, "ssh", "-q", "-o", "LogLevel=ERROR", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5", "-p", strconv.Itoa(sshPort), remote, cmd}
	out, err := exec.CommandContext(cctx, "sshpass", args...).CombinedOutput()
	if err != nil {
		return Summary{}, err
	}
	return summaryFromSyncResponseOutput(out)
}

func summaryFromSyncResponseOutput(out []byte) (Summary, error) {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return Summary{}, fmt.Errorf("ssh sem saída")
	}
	var direct Summary
	if err := json.Unmarshal([]byte(text), &direct); err == nil && (direct.OK || len(direct.Users) > 0) {
		return direct, nil
	}
	var resp struct {
		OK     bool   `json:"ok"`
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		return Summary{}, err
	}
	if !resp.OK && strings.TrimSpace(resp.Error) != "" {
		return Summary{}, fmt.Errorf("%s", resp.Error)
	}
	var sum Summary
	if err := json.Unmarshal([]byte(resp.Output), &sum); err != nil {
		return Summary{}, err
	}
	return sum, nil
}

func (m *Manager) remoteCheckUserSummary(ctx context.Context, srv model.Server, byName map[string]model.Account) (Summary, error) {
	ports := uniqueInts(81, m.cfg.CheckUserPort, 2052)
	var lastErr error
	for _, port := range ports {
		for _, path := range checkUserOnlinePaths(port) {
			url := fmt.Sprintf("http://%s:%d%s", srv.Host, port, withLocalOnlineScope(path))
			sum, err := m.getSummaryURL(ctx, url, "")
			if err == nil && len(sum.Users) > 0 {
				for i := range sum.Users {
					if len(sum.Users[i].Sources) == 0 {
						sum.Users[i].Sources = []string{sourceName(srv, fmt.Sprintf("porta-%d", port))}
					}
				}
				return sum, nil
			}
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("sem onlines via checkuser")
	}
	return Summary{}, lastErr
}

func (m *Manager) getSummaryURL(ctx context.Context, rawURL, token string) (Summary, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Summary{}, err
	}
	req.Header.Set("User-Agent", "GestorPrimecel/online-check")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	q := req.URL.Query()
	q.Set("_", strconv.FormatInt(time.Now().UnixNano(), 10))
	req.URL.RawQuery = q.Encode()
	if token != "" {
		req.Header.Set("X-Primecel-Agent-Token", token)
		q := req.URL.Query()
		q.Set("token", token)
		req.URL.RawQuery = q.Encode()
	}
	res, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return Summary{}, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return Summary{}, fmt.Errorf("http %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 256*1024))
	if err != nil {
		return Summary{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err == nil {
		return summaryFromJSON(raw), nil
	}
	var arr []any
	if err := json.Unmarshal(b, &arr); err == nil {
		return summaryFromAnyList(arr), nil
	}
	return summaryFromText(string(b)), nil
}

func (m *Manager) getCountURL(ctx context.Context, rawURL string) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return 0, fmt.Errorf("http %d", res.StatusCode)
	}
	var raw map[string]any
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return 0, err
	}
	return intFromAny(firstAny(raw, "count", "device_count", "count_connections", "connections")), nil
}

func withLocalOnlineScope(path string) string {
	if strings.Contains(path, "?") {
		return path + "&scope=local"
	}
	return path + "?scope=local"
}

func checkUserOnlinePaths(port int) []string {
	// Onlines precisam vir de endpoint vivo. /devices/list e /devices/count são
	// aparelhos cadastrados/liberados, não sessões conectadas, e causavam usuário
	// removido ou desconectado aparecer preso como online.
	paths := []string{"/onlines", "/online-summary"}
	if strings.TrimSpace(os.Getenv("MULTIVPS_PORT81_ONLINES")) != "" {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("MULTIVPS_PORT81_ONLINES")))
		if v == "0" || v == "false" || v == "no" || v == "nao" || v == "não" || v == "off" {
			return []string{"/online-summary"}
		}
	}
	return paths
}

func summaryFromJSON(raw map[string]any) Summary {
	merged := map[string]*Item{}
	// Não usa chave "devices" como online: isso representa aparelho cadastrado,
	// não conexão ativa. A contagem viva vem de users/onlines/online.
	for _, key := range []string{"users", "onlines", "online", "items", "accounts", "data", "result", "clientes", "usuarios"} {
		if v, ok := raw[key]; ok && v != nil {
			mergeAnyOnline(merged, v)
		}
	}
	if len(merged) == 0 {
		it := itemFromMap(raw)
		if it.Username != "" && it.Connections > 0 {
			mergeItem(merged, it)
		}
	}
	return finalize(merged)
}

func summaryFromAnyList(arr []any) Summary {
	merged := map[string]*Item{}
	mergeAnyOnline(merged, arr)
	return finalize(merged)
}

func mergeDevicesOnline(merged map[string]*Item, v any) {
	type acc struct {
		username string
		limit    int
		ids      map[string]bool
	}
	devices := map[string]*acc{}
	var walk func(any)
	walk = func(x any) {
		switch val := x.(type) {
		case []any:
			for _, item := range val {
				walk(item)
			}
		case map[string]any:
			if isDeviceRow(val) {
				username := stringFromAny(firstAny(val, "username", "user", "login", "usuario", "nick", "name", "nome", "email", "conta"))
				if username == "" {
					username = stringFromAny(firstAny(val, "uuid", "user_uuid", "xray_uuid"))
				}
				if username == "" {
					return
				}
				key := strings.ToLower(username)
				if devices[key] == nil {
					devices[key] = &acc{username: username, ids: map[string]bool{}}
				}
				devID := stringFromAny(firstAny(val, "id", "device_id", "deviceid", "deviceId", "hwid", "android_id", "androidId", "device"))
				if devID == "" {
					devID = fmt.Sprintf("row:%d", len(devices[key].ids)+1)
				}
				devices[key].ids[strings.ToLower(devID)] = true
				if limit := intFromAny(firstAny(val, "limit", "limite", "limit_connections", "device_limit", "max", "max_limit")); limit > devices[key].limit {
					devices[key].limit = limit
				}
				return
			}
			for username, item := range val {
				uname := strings.TrimSpace(username)
				if uname == "" {
					continue
				}
				if arr, ok := item.([]any); ok {
					for _, one := range arr {
						if mp, ok := one.(map[string]any); ok {
							row := map[string]any{}
							for k, v := range mp {
								row[k] = v
							}
							if stringFromAny(firstAny(row, "username", "user", "login", "usuario", "nick", "name", "nome", "email", "conta")) == "" {
								row["username"] = uname
							}
							walk(row)
						}
					}
					continue
				}
				walk(item)
			}
		}
	}
	walk(v)
	for _, d := range devices {
		if d == nil || d.username == "" || len(d.ids) <= 0 {
			continue
		}
		limit := d.limit
		if limit <= 0 {
			limit = len(d.ids)
		}
		mergeItemMax(merged, Item{Username: d.username, Connections: len(d.ids), Limit: maxInt(1, limit), Sources: []string{"devices"}, DeviceAuthority: true})
	}
}

func isDeviceRow(mp map[string]any) bool {
	if stringFromAny(firstAny(mp, "id", "device_id", "deviceid", "deviceId", "hwid", "android_id", "androidId", "device")) == "" {
		return false
	}
	return stringFromAny(firstAny(mp, "username", "user", "login", "usuario", "nick", "name", "nome", "email", "conta", "uuid", "user_uuid", "xray_uuid")) != ""
}

func mergeAnyOnline(merged map[string]*Item, v any) {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			mergeAnyOnline(merged, item)
		}
	case map[string]any:
		if direct := itemFromMap(x); direct.Username != "" && direct.Connections > 0 {
			mergeItem(merged, direct)
			return
		}
		for username, item := range x {
			uname := strings.TrimSpace(username)
			if uname == "" {
				continue
			}
			if mp, ok := item.(map[string]any); ok {
				row := map[string]any{}
				for k, v := range mp {
					row[k] = v
				}
				if stringFromAny(firstAny(row, "username", "user", "login", "usuario", "nick", "name", "nome", "email", "conta", "uuid", "user_uuid", "xray_uuid")) == "" {
					row["username"] = uname
				}
				it := itemFromMap(row)
				if it.Username != "" && it.Connections > 0 {
					mergeItem(merged, it)
				}
				continue
			}
			connections := onlineCountFromAnyValue(item)
			if connections <= 0 {
				connections = 1
			}
			mergeItem(merged, Item{Username: uname, Connections: connections, Limit: maxInt(1, connections), Sources: []string{"checkuser"}})
		}
	case string:
		if it, ok := itemFromTextLine(x); ok {
			mergeItem(merged, it)
		}
	}
}

func onlineCountFromAnyValue(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case []any:
		seen := map[string]bool{}
		count := 0
		for idx, one := range x {
			if mp, ok := one.(map[string]any); ok {
				id := first(
					stringFromAny(firstAny(mp, "id", "device_id", "deviceid", "session", "session_id", "peer", "ip", "client_ip", "remote_ip", "addr")),
					stringFromAny(firstAny(mp, "username", "user", "login", "usuario")),
				)
				if id == "" {
					id = fmt.Sprintf("row:%d", idx)
				}
				id = strings.ToLower(strings.TrimSpace(id))
				if !seen[id] {
					seen[id] = true
					count++
				}
				continue
			}
			id := strings.ToLower(strings.TrimSpace(stringFromAny(one)))
			if id == "" {
				id = fmt.Sprintf("row:%d", idx)
			}
			if !seen[id] {
				seen[id] = true
				count++
			}
		}
		return count
	case map[string]any:
		if n := intFromAny(firstAny(x, "connections", "connection", "count_connections", "online", "onlines", "qtd", "total")); n > 0 {
			return n
		}
		for _, key := range []string{"users", "onlines", "online", "items", "data", "result", "clientes", "usuarios"} {
			if inner, ok := x[key]; ok {
				if n := onlineCountFromAnyValue(inner); n > 0 {
					return n
				}
			}
		}
		return 0
	default:
		return intFromAny(v)
	}
}

func summaryFromText(text string) Summary {
	merged := map[string]*Item{}
	text = strings.TrimSpace(text)
	if text == "" {
		return finalize(merged)
	}
	noTags := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(text, "\n")
	noTags = strings.ReplaceAll(noTags, "\r", "\n")
	for _, raw := range strings.Split(noTags, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		if strings.Contains(low, "nenhuma conta") || strings.Contains(low, "contas online") || strings.Contains(low, "usuarios online") || strings.Contains(low, "usuários online") || strings.TrimSpace(low) == "online:" {
			continue
		}
		if it, ok := itemFromTextLine(line); ok {
			mergeItem(merged, it)
		}
	}
	return finalize(merged)
}

func itemFromTextLine(line string) (Item, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Item{}, false
	}
	re := regexp.MustCompile("`?([A-Za-z0-9_.@-]{2,64})`?(?:\\s*(?:\\||:|-|=|\\s)\\s*(\\d+)(?:\\s*/\\s*(\\d+))?)?")
	m := re.FindStringSubmatch(line)
	if len(m) < 2 || strings.TrimSpace(m[1]) == "" {
		return Item{}, false
	}
	username := strings.TrimSpace(m[1])
	connections := 1
	if len(m) > 2 && strings.TrimSpace(m[2]) != "" {
		connections = maxInt(1, atoi(m[2]))
	}
	limit := connections
	if len(m) > 3 && strings.TrimSpace(m[3]) != "" {
		limit = maxInt(1, atoi(m[3]))
	}
	return Item{Username: username, Connections: connections, Limit: maxInt(1, limit), Sources: []string{"proto"}, Modes: []string{"PROTO"}}, true
}

func itemFromMap(mp map[string]any) Item {
	it := Item{
		Username:      stringFromAny(firstAny(mp, "username", "user", "login", "usuario", "nick", "name", "nome", "email", "conta")),
		Connections:   intFromAny(firstAny(mp, "connections", "connection", "count", "online", "onlines", "count_connections", "conexoes", "conexões", "qtd")),
		Limit:         intFromAny(firstAny(mp, "limit", "limite", "limit_connections", "device_limit", "max", "max_limit")),
		OwnerID:       int64(intFromAny(firstAny(mp, "owner_telegram_id", "owner_id"))),
		OwnerName:     stringFromAny(firstAny(mp, "owner_name")),
		OwnerType:     stringFromAny(firstAny(mp, "owner_type")),
		IP:            stringFromAny(firstAny(mp, "ip", "client_ip", "remote_ip", "address", "addr")),
		ConnectedTime: stringFromAny(firstAny(mp, "connected_time", "uptime", "duration", "tempo", "time_online", "online_time")),
	}
	if it.Username == "" {
		it.Username = stringFromAny(firstAny(mp, "uuid", "user_uuid", "xray_uuid"))
	}
	if it.Connections <= 0 && it.Username != "" {
		it.Connections = 1
	}
	if it.Limit <= 0 {
		it.Limit = maxInt(1, it.Connections)
	}
	if arr, ok := mp["sources"].([]any); ok {
		for _, v := range arr {
			if s := stringFromAny(v); s != "" {
				it.Sources = mergeStringLists(it.Sources, []string{normalizeOnlineSource(s)})
			}
		}
	} else if s := stringFromAny(firstAny(mp, "source", "mode", "type", "protocol", "protocolo")); s != "" {
		it.Sources = mergeStringLists(it.Sources, []string{normalizeOnlineSource(s)})
	}
	if arr, ok := mp["modes"].([]any); ok {
		for _, v := range arr {
			if s := stringFromAny(v); s != "" {
				it.Modes = mergeStringLists(it.Modes, []string{strings.ToUpper(strings.TrimSpace(s))})
			}
		}
	} else if s := stringFromAny(mp["mode"]); s != "" {
		it.Modes = mergeStringLists(it.Modes, []string{strings.ToUpper(strings.TrimSpace(s))})
	}
	if len(it.Sources) == 0 && it.Username != "" {
		it.Sources = []string{"proto"}
	}
	for _, s := range it.Sources {
		it.Modes = mergeStringLists(it.Modes, modesForSource(s))
	}
	return it
}

func maxInt(vals ...int) int {
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func add(it *Item, source string) { addN(it, 1, source) }
func addN(it *Item, n int, source string) {
	if it == nil || n <= 0 {
		return
	}
	it.Connections += n
	source = normalizeOnlineSource(source)
	for _, s := range it.Sources {
		if s == source {
			it.Modes = mergeStringLists(it.Modes, modesForSource(source))
			return
		}
	}
	if source != "" {
		it.Sources = append(it.Sources, source)
	}
	it.Modes = mergeStringLists(it.Modes, modesForSource(source))
}

func normalizeOnlineSource(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	s = strings.ReplaceAll(s, "_", "-")
	if s == "" {
		return ""
	}
	if strings.Contains(s, "porta-81") || strings.Contains(s, "port-81") || strings.Contains(s, ":81") || strings.Contains(s, "proto") || strings.Contains(s, "dragon") || strings.Contains(s, "proxy") {
		return "proto"
	}
	if strings.Contains(s, "xray") || strings.Contains(s, "v2ray") || strings.Contains(s, "vless") || strings.Contains(s, "vmess") || strings.Contains(s, "dragoncore") {
		return "xray"
	}
	if strings.Contains(s, "ssh") || strings.Contains(s, "dropbear") {
		return "ssh"
	}
	if strings.Contains(s, "checkuser") || strings.Contains(s, "online-summary") {
		return "proto"
	}
	return s
}

func modesForSource(source string) []string {
	switch normalizeOnlineSource(source) {
	case "proto":
		return []string{"PROTO"}
	case "ssh":
		return []string{"SSH"}
	case "xray":
		return []string{"XRAY"}
	default:
		return nil
	}
}

func mergeStringLists(dst []string, src []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(dst)+len(src))
	for _, s := range dst {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range src {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func nonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func cappedOnlineConnections(connections, limit int) int {
	if connections <= 0 {
		return 0
	}
	if limit <= 0 {
		limit = 1
	}
	maxVisible := limit + 1
	if connections > maxVisible {
		return maxVisible
	}
	return connections
}

func seedAccountItems(accs []model.Account) map[string]*Item {
	out := map[string]*Item{}
	for _, a := range accs {
		if a.DeletedAt != nil || a.Status == "deleted" {
			continue
		}
		key := strings.ToLower(a.Username)
		out[key] = &Item{Username: a.Username, Limit: nonZero(a.LimitConnections, 1), OwnerID: a.OwnerTelegramID, OwnerName: a.OwnerName, OwnerType: a.OwnerType}
	}
	return out
}

func mergeAllMax(dst map[string]*Item, src map[string]*Item) {
	for _, it := range src {
		if it == nil || it.Connections <= 0 {
			continue
		}
		mergeItemMax(dst, *it)
	}
}

func isSystemOnlineUsername(username string) bool {
	u := strings.ToLower(strings.TrimSpace(username))
	if u == "" {
		return true
	}
	if u == "root" || strings.HasPrefix(u, "root@") || strings.HasSuffix(u, "@notty") || strings.Contains(u, "@pts/") {
		return true
	}
	switch u {
	case "daemon", "bin", "sys", "sync", "games", "man", "lp", "mail", "news", "uucp", "proxy", "www-data", "backup", "list", "irc", "gnats", "nobody", "systemd-network", "systemd-resolve", "messagebus", "sshd", "ubuntu", "debian", "admin":
		return true
	}
	return false
}

func finalize(m map[string]*Item) Summary {
	users := make([]Item, 0)
	total := 0
	for _, it := range m {
		if it == nil || it.Connections <= 0 || it.Username == "" || isSystemOnlineUsername(it.Username) {
			continue
		}
		if it.Limit <= 0 {
			it.Limit = 1
		}
		it.Connections = cappedOnlineConnections(it.Connections, it.Limit)
		for i := range it.Sources {
			it.Sources[i] = normalizeOnlineSource(it.Sources[i])
			it.Modes = mergeStringLists(it.Modes, modesForSource(it.Sources[i]))
		}
		it.Sources = mergeStringLists(nil, it.Sources)
		sort.Strings(it.Sources)
		sort.Strings(it.Modes)
		users = append(users, *it)
		total += it.Connections
	}
	sort.Slice(users, func(i, j int) bool { return strings.ToLower(users[i].Username) < strings.ToLower(users[j].Username) })
	return Summary{OK: true, Count: total, Users: users}
}

func mergeItemMax(dst map[string]*Item, it Item) {
	if it.Username == "" || it.Connections <= 0 {
		return
	}
	key := strings.ToLower(it.Username)
	ex := dst[key]
	if ex == nil {
		cp := it
		if cp.Limit <= 0 {
			cp.Limit = 1
		}
		dst[key] = &cp
		return
	}
	if it.DeviceAuthority {
		ex.Connections = it.Connections
		ex.DeviceAuthority = true
	} else if !ex.DeviceAuthority && it.Connections > ex.Connections {
		ex.Connections = it.Connections
	}
	mergeItemMeta(ex, it)
}

func mergeItem(dst map[string]*Item, it Item) {
	if it.Username == "" || it.Connections <= 0 {
		return
	}
	key := strings.ToLower(it.Username)
	ex := dst[key]
	if ex == nil {
		cp := it
		if cp.Limit <= 0 {
			cp.Limit = 1
		}
		dst[key] = &cp
		return
	}
	ex.Connections += it.Connections
	mergeItemMeta(ex, it)
}

func mergeItemMeta(ex *Item, it Item) {
	if it.DeviceAuthority {
		ex.DeviceAuthority = true
		if it.Connections > 0 && (ex.Connections <= 0 || ex.Connections > it.Connections) {
			ex.Connections = it.Connections
		}
	}
	if ex.Limit <= 0 && it.Limit > 0 {
		ex.Limit = it.Limit
	}
	if ex.OwnerID == 0 && it.OwnerID != 0 {
		ex.OwnerID = it.OwnerID
		ex.OwnerName = it.OwnerName
		ex.OwnerType = it.OwnerType
	}
	for _, s := range it.Sources {
		s = normalizeOnlineSource(s)
		found := false
		for _, cur := range ex.Sources {
			if cur == s {
				found = true
				break
			}
		}
		if !found && s != "" {
			ex.Sources = append(ex.Sources, s)
		}
		ex.Modes = mergeStringLists(ex.Modes, modesForSource(s))
	}
	ex.Modes = mergeStringLists(ex.Modes, it.Modes)
	if ex.IP == "" && strings.TrimSpace(it.IP) != "" {
		ex.IP = strings.TrimSpace(it.IP)
	}
	if ex.ConnectedTime == "" && strings.TrimSpace(it.ConnectedTime) != "" {
		ex.ConnectedTime = strings.TrimSpace(it.ConnectedTime)
	}
}

func accountsByName(accs []model.Account) map[string]model.Account {
	out := map[string]model.Account{}
	for _, a := range accs {
		if a.DeletedAt != nil || a.Status == "deleted" || strings.TrimSpace(a.Username) == "" {
			continue
		}
		out[strings.ToLower(a.Username)] = a
		if strings.TrimSpace(a.UUID) != "" {
			out[strings.ToLower(a.UUID)] = a
		}
	}
	return out
}

func enrich(it Item, byName map[string]model.Account, source string) Item {
	key := strings.ToLower(strings.TrimSpace(it.Username))
	a, ok := byName[key]
	if !ok {
		// Só mostra online de conta ativa existente no bot. Isso elimina registros
		// presos em CheckUser/porta 81 depois que a conta foi removida.
		return Item{}
	}
	it.Username = a.Username
	if it.Limit <= 0 {
		it.Limit = nonZero(a.LimitConnections, 1)
	}
	if it.OwnerID == 0 {
		it.OwnerID = a.OwnerTelegramID
		it.OwnerName = a.OwnerName
		it.OwnerType = a.OwnerType
	}
	if len(it.Sources) == 0 && source != "" {
		it.Sources = []string{source}
	} else if source != "" {
		found := false
		for _, s := range it.Sources {
			if s == source {
				found = true
			}
		}
		if !found {
			it.Sources = append(it.Sources, source)
		}
	}
	return it
}

func resolveAccountName(username, uuid string, byName map[string]model.Account, uuidToName map[string]string) string {
	for _, v := range []string{username, uuid} {
		key := strings.ToLower(strings.TrimSpace(v))
		if key == "" {
			continue
		}
		if a, ok := byName[key]; ok {
			return a.Username
		}
		if name := uuidToName[key]; name != "" {
			return name
		}
	}
	return ""
}

func sourceName(srv model.Server, suffix string) string {
	name := strings.TrimSpace(srv.Name)
	if name == "" {
		name = strings.TrimSpace(srv.Host)
	}
	if suffix != "" {
		return name + "/" + suffix
	}
	return name
}

func uniqueInts(vals ...int) []int {
	seen := map[int]bool{}
	var out []int
	for _, v := range vals {
		if v <= 0 || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
func queryEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "%", "%25"), " ", "%20")
}
func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		return atoi(x)
	default:
		return 0
	}
}
func stringFromAny(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strconv.FormatInt(int64(x), 10)
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}
func firstAny(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}
func first(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func looksUUID(s string) bool { return uuidPattern.MatchString(s) }
