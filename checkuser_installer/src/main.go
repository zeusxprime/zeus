package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const version = "0.1.16-primecel-standalone-dtunnel-device"

type Args struct {
	Host           string
	Port           int
	Start          bool
	SSLEnabled     bool
	ListAllDevices bool
	ListDevices    string
	DeleteDevices  string
	DeleteDB       bool
	ShowVersion    bool
}

type account struct {
	ID              int
	Username        string
	UUID            string
	ExpiresAt       time.Time
	ExpiryDate      string
	Limit           int
	Status          string
	OwnerTelegramID int64
	OwnerType       string
}

type deviceRow struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type onlineUser struct {
	Username    string   `json:"username"`
	Connections int      `json:"connections"`
	Limit       int      `json:"limit"`
	Sources     []string `json:"sources,omitempty"`
	Modes       []string `json:"modes,omitempty"`
}

type onlineResponse struct {
	OK     bool         `json:"ok"`
	Source string       `json:"source"`
	Count  int          `json:"count"`
	Users  []onlineUser `json:"users"`
}

func initializeArgs() *Args {
	args := &Args{}
	flag.StringVar(&args.Host, "host", "0.0.0.0", "Host to listen")
	flag.IntVar(&args.Port, "port", 2052, "Port")
	flag.BoolVar(&args.Start, "start", false, "Start the daemon")
	flag.BoolVar(&args.SSLEnabled, "ssl", false, "Use server SSL")
	flag.BoolVar(&args.ListAllDevices, "list-all-devices", false, "List all devices")
	flag.StringVar(&args.ListDevices, "list-devices", "", "List devices from a user")
	flag.StringVar(&args.DeleteDevices, "delete-devices", "", "Delete devices from a user")
	flag.BoolVar(&args.DeleteDB, "delete-db", false, "Delete database of devices")
	flag.BoolVar(&args.ShowVersion, "version", false, "Show version information")
	flag.Parse()
	return args
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	args := initializeArgs()
	if args.ShowVersion {
		fmt.Println("checkuser " + version)
		return
	}
	if args.Start {
		startServer(args.Host, args.Port, args.SSLEnabled)
		return
	}
	if args.DeleteDB {
		mustJSON(map[string]any{"ok": true, "dropped": dropDeviceTable(context.Background()) == nil})
		return
	}
	if args.ListAllDevices {
		rows, err := listDevices(context.Background(), "")
		if err != nil {
			fatal(err)
		}
		mustJSON(map[string]any{"ok": true, "count": len(rows), "devices": rows})
		return
	}
	if args.ListDevices != "" {
		rows, err := listDevices(context.Background(), args.ListDevices)
		if err != nil {
			fatal(err)
		}
		mustJSON(map[string]any{"ok": true, "username": args.ListDevices, "count": len(rows), "devices": rows})
		return
	}
	if args.DeleteDevices != "" {
		count, err := deleteDevices(context.Background(), args.DeleteDevices)
		if err != nil {
			fatal(err)
		}
		mustJSON(map[string]any{"ok": true, "username": args.DeleteDevices, "removed": count})
		return
	}
	flag.Usage()
	os.Exit(1)
}

func startServer(host string, port int, ssl bool) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler)
	mux.HandleFunc("/check", checkHandler)
	mux.HandleFunc("/check/", checkPathHandler)
	mux.HandleFunc("/details/", detailsHandler)
	mux.HandleFunc("/count", countHandler)
	mux.HandleFunc("/onlines", onlinesHandler)
	mux.HandleFunc("/online-summary", onlinesHandler)
	mux.HandleFunc("/online-count", onlinesHandler)
	mux.HandleFunc("/devices/list", devicesListHandler)
	mux.HandleFunc("/devices/list/", devicesListHandler)
	mux.HandleFunc("/devices/count", devicesCountHandler)
	mux.HandleFunc("/device", checkHandler)
	addr := fmt.Sprintf("%s:%d", host, port)
	fmt.Println("checkuser ouvindo em", addr)
	if ssl {
		cert := firstExisting("/etc/checkuser/cert.pem", "/etc/ssl/checkuser/cert.pem")
		key := firstExisting("/etc/checkuser/key.pem", "/etc/ssl/checkuser/key.pem")
		if cert != "" && key != "" {
			fatal(http.ListenAndServeTLS(addr, cert, key, mux))
		}
		fmt.Fprintln(os.Stderr, "ssl solicitado, mas certificado/key não encontrados; iniciando em HTTP")
	}
	fatal(http.ListenAndServe(addr, mux))
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if user := findUserParam(r); user != "" {
		renderCheck(w, r, user)
		return
	}
	setCORS(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><html><head><meta charset=\"utf-8\"><title>CheckUser</title></head><body><h3>CheckUser DTunnel</h3><p>Use <code>/?user=USUARIO&amp;deviceId=DEVICE</code></p></body></html>"))
}

func checkHandler(w http.ResponseWriter, r *http.Request) { renderCheck(w, r, findUserParam(r)) }

func checkPathHandler(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimPrefix(r.URL.Path, "/check/")
	renderCheck(w, r, user)
}

func detailsHandler(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimPrefix(r.URL.Path, "/details/")
	acc, err := findAccount(r.Context(), user)
	if err != nil || acc == nil {
		jsonOut(w, http.StatusNotFound, map[string]any{"status": "not_found", "allowed": false, "error": errString(err)})
		return
	}
	label, days, val, unit, hours, minutes, totalMinutes := remainingInfo(acc.ExpiresAt)
	connections := bestLiveConnections(r.Context(), acc.Username, acc.Limit)
	jsonOut(w, 200, map[string]any{
		"id": acc.ID, "username": acc.Username, "expires_at": acc.ExpiryDate, "expires_days": days,
		"expires_in": label, "expiration_remaining": label, "expiration_display": label,
		"expiration_value": val, "expiration_unit": unit, "expiration_hours": hours, "expiration_minutes": minutes,
		"expiration_total_minutes": totalMinutes, "limit": acc.Limit, "connections": connections,
	})
}

func countHandler(w http.ResponseWriter, r *http.Request) {
	user := findUserParam(r)
	if user != "" {
		acc, _ := findAccount(r.Context(), user)
		if acc == nil {
			jsonOut(w, 404, map[string]any{"count": 0, "status": "not_found"})
			return
		}
		jsonOut(w, 200, map[string]any{"username": acc.Username, "count": bestLiveConnections(r.Context(), acc.Username, acc.Limit)})
		return
	}
	accs := loadAccounts(r.Context())
	now := time.Now()
	count := 0
	for _, a := range accs {
		if isActive(a, now) {
			count++
		}
	}
	jsonOut(w, 200, map[string]any{"count": count})
}

func onlinesHandler(w http.ResponseWriter, r *http.Request) {
	resp := getOnlineSummary(r.Context())
	if resp == nil {
		resp = &onlineResponse{OK: true, Source: "checkuser", Users: []onlineUser{}}
	}
	resp = normalizeOnline(resp)
	jsonOut(w, 200, resp)
}

func devicesListHandler(w http.ResponseWriter, r *http.Request) {
	user := findUserParam(r)
	if user == "" {
		user = strings.TrimPrefix(r.URL.Path, "/devices/list/")
	}
	rows, err := listDevices(r.Context(), user)
	if err != nil {
		jsonOut(w, 500, map[string]any{"ok": false, "error": err.Error(), "devices": []any{}})
		return
	}
	jsonOut(w, 200, map[string]any{"ok": true, "count": len(rows), "devices": rows})
}

func devicesCountHandler(w http.ResponseWriter, r *http.Request) {
	user := findUserParam(r)
	n, _ := countDevicesByUsername(r.Context(), user)
	jsonOut(w, 200, map[string]any{"count": n})
}

func renderCheck(w http.ResponseWriter, r *http.Request, username string) {
	username = strings.TrimSpace(username)
	if username == "" {
		jsonOut(w, 400, map[string]any{"status": "bad_request", "allowed": false, "error": "Please provide a username"})
		return
	}
	acc, err := findAccount(r.Context(), username)
	if err != nil || acc == nil {
		jsonOut(w, 404, map[string]any{"status": "not_found", "allowed": false, "device_allowed": false, "device_status": "not_found", "error": errString(err)})
		return
	}

	now := time.Now()
	if !isActive(*acc, now) {
		label, days, val, unit, hours, minutes, totalMinutes := remainingInfo(acc.ExpiresAt)
		jsonOut(w, 200, map[string]any{
			"id": acc.ID, "username": acc.Username, "expiration_date": formatDateBR(acc.ExpiresAt), "expiration_days": days,
			"expires_in": label, "expiration_value": val, "expiration_unit": unit, "expiration_hours": hours,
			"expiration_minutes": minutes, "expiration_total_minutes": totalMinutes, "limit_connections": maxInt(acc.Limit, 1),
			"count_connections": maxInt(acc.Limit, 1) + 1, "device_count": 0, "device_limit": maxInt(acc.Limit, 1),
			"device_allowed": false, "device_status": "expired", "device_uuid": acc.UUID, "allowed": false,
		})
		return
	}

	deviceID := findDeviceParam(r)
    limit := maxInt(acc.Limit, 1)
    devices, _ := countDevicesByUsername(r.Context(), acc.Username)
    liveConnections := bestLiveConnections(r.Context(), acc.Username, limit)
    deviceAllowed := true
    deviceStatus := "ok"
    connections := liveConnections

	if deviceID != "" {
		exists, _ := deviceExists(r.Context(), deviceID)
		if !exists {
            if devices >= limit {
                deviceAllowed = false
                deviceStatus = "limit_reached"
            } else if err := saveDevice(r.Context(), deviceID, acc.Username); err == nil {
                devices++
            }
        }
	}

	label, days, val, unit, _, _, _ := remainingInfo(acc.ExpiresAt)
    allowed := deviceAllowed && connections <= limit

    jsonOut(w, 200, map[string]any{
	    "id": acc.ID,
	    "username": acc.Username,
	    "expiration_date": formatDateBR(acc.ExpiresAt),
	    "expiration_days": days,
	    "expires_in": label,
	    "expiration_value": val,
	    "expiration_unit": unit,
	    "limit_connections": limit,
	    "count_connections": bestLiveConnections(r.Context(), acc.Username, limit),
	    "device_count": devices,
	    "device_limit": limit,
	    "device_allowed": allowed,
	    "device_status": deviceStatus,
	    "allowed": allowed,
    })
}

func findUserParam(r *http.Request) string {
	q := r.URL.Query()
	for _, k := range []string{"user", "username", "usuario", "uuid", "xray_uuid", "id_uuid"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

func findDeviceParam(r *http.Request) string {
	q := r.URL.Query()
	for _, k := range []string{"deviceId", "deviceid", "device_id", "hwid", "android_id", "id"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

func findAccount(ctx context.Context, lookup string) (*account, error) {
	lookup = strings.TrimSpace(lookup)
	if lookup == "" {
		return nil, errors.New("usuario vazio")
	}
	if !safeLookup(lookup) {
		return nil, errors.New("usuario invalido")
	}
	if acc := findAccountInSQLite(ctx, lookup); acc != nil {
		if suspended, reason := resellerSuspended(acc.OwnerTelegramID, acc.OwnerType); suspended {
			return nil, fmt.Errorf("acesso suspenso: %s", reason)
		}
		return acc, nil
	}
	accs := loadAccounts(ctx)
	want := norm(lookup)
	var found *account
	for i := range accs {
		a := accs[i]
		if norm(a.Username) == want || (a.UUID != "" && norm(a.UUID) == want) {
			cp := a
			found = &cp
		}
	}
	if found == nil {
		return nil, fmt.Errorf("validade nao encontrada para o usuario %s", lookup)
	}
	if suspended, reason := resellerSuspended(found.OwnerTelegramID, found.OwnerType); suspended {
		return nil, fmt.Errorf("acesso suspenso: %s", reason)
	}
	return found, nil
}

func loadAccounts(ctx context.Context) []account {
	out := []account{}
	seen := map[string]int{}
	for _, a := range loadAccountsFromSQLite(ctx) {
		key := norm(a.Username)
		if key == "" {
			continue
		}
		if idx, ok := seen[key]; ok {
			out[idx] = a
		} else {
			seen[key] = len(out)
			out = append(out, a)
		}
	}
	for _, path := range []string{env("CHECKUSER_BOT_USERS_LOG", "/etc/primecel-gestor/users.jsonl"), "/etc/tg-access-bot/users.jsonl"} {
		for _, a := range loadAccountsJSONL(path) {
			key := norm(a.Username)
			if key == "" {
				continue
			}
			if idx, ok := seen[key]; ok {
				out[idx] = a
			} else {
				seen[key] = len(out)
				out = append(out, a)
			}
		}
	}
	for _, a := range loadAccountsUsuariosDB() {
		key := norm(a.Username)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; !ok {
			seen[key] = len(out)
			out = append(out, a)
		}
	}
	return out
}

func findAccountInSQLite(ctx context.Context, lookup string) *account {
	path := env("DB_FILE", "/etc/primecel-gestor/gestor.db")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	esc := sqlEscape(lookup)
	sql := "SELECT id,username,uuid,limit_connections,expires_at,expiry_date,owner_telegram_id,owner_type,status,COALESCE(deleted_at,'') FROM accounts WHERE deleted_at IS NULL AND (lower(username)=lower('" + esc + "') OR lower(uuid)=lower('" + esc + "')) ORDER BY updated_at DESC LIMIT 1;"
	out, err := sqliteOut(ctx, path, sql)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	line := strings.Split(strings.TrimSpace(out), "\n")[0]
	parts := strings.Split(line, "|")
	if len(parts) < 10 {
		return nil
	}
	a := accountFromParts(parts)
	return &a
}

func loadAccountsFromSQLite(ctx context.Context) []account {
	path := env("DB_FILE", "/etc/primecel-gestor/gestor.db")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	sql := "SELECT id,username,uuid,limit_connections,expires_at,expiry_date,owner_telegram_id,owner_type,status,COALESCE(deleted_at,'') FROM accounts ORDER BY updated_at ASC;"
	out, err := sqliteOut(ctx, path, sql)
	if err != nil {
		return nil
	}
	res := []account{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 10 {
			continue
		}
		if parts[9] != "" || strings.EqualFold(parts[8], "deleted") {
			continue
		}
		res = append(res, accountFromParts(parts))
	}
	return res
}

func accountFromParts(parts []string) account {
	exp := firstNonEmpty(parts[4], parts[5])
	t, _ := parseTime(exp)
	limit, _ := strconv.Atoi(strings.TrimSpace(parts[3]))
	id, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	owner, _ := strconv.ParseInt(strings.TrimSpace(parts[6]), 10, 64)
	return account{ID: id, Username: parts[1], UUID: parts[2], Limit: maxInt(limit, 1), ExpiresAt: t, ExpiryDate: dateDisplay(t, parts[5]), OwnerTelegramID: owner, OwnerType: parts[7], Status: firstNonEmpty(parts[8], "active")}
}

func loadAccountsJSONL(path string) []account {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	res := []account{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if boolFromAny(row["deleted"]) || strings.EqualFold(strFromAny(row["status"]), "deleted") {
			continue
		}
		username := firstNonEmpty(strFromAny(row["username"]), strFromAny(row["user"]), strFromAny(row["login"]))
		if username == "" {
			continue
		}
		expRaw := firstNonEmpty(strFromAny(row["expires_at"]), strFromAny(row["expiry"]), strFromAny(row["expires"]), strFromAny(row["expiration_date"]))
		exp, ok := parseTime(expRaw)
		if !ok {
			continue
		}
		limit := intFromAny(firstAny(row, "limit", "limit_connections", "limite"))
		owner := int64FromAny(firstAny(row, "owner_telegram_id", "reseller_telegram_id"))
		res = append(res, account{Username: username, UUID: strFromAny(row["uuid"]), ExpiresAt: exp, ExpiryDate: dateDisplay(exp, strFromAny(row["expiry"])), Limit: maxInt(limit, 1), Status: firstNonEmpty(strFromAny(row["status"]), "active"), OwnerTelegramID: owner, OwnerType: strFromAny(row["owner_type"])})
	}
	return res
}

func loadAccountsUsuariosDB() []account {
	path := env("CHECKUSER_USUARIOS_DB_PATH", "/root/usuarios.db")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	res := []account{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sepLine := strings.NewReplacer("|", " ", ";", " ", ",", " ").Replace(line)
		fields := strings.Fields(sepLine)
		if len(fields) < 2 {
			continue
		}
		username := fields[0]
		limit := 1
		expRaw := ""
		for _, f := range fields[1:] {
			lf := strings.ToLower(f)
			if strings.HasPrefix(lf, "limit=") || strings.HasPrefix(lf, "limite=") {
				limit, _ = strconv.Atoi(strings.TrimSpace(strings.SplitN(f, "=", 2)[1]))
				continue
			}
			if strings.HasPrefix(lf, "expiry=") || strings.HasPrefix(lf, "expires=") || strings.HasPrefix(lf, "validade=") {
				expRaw = strings.TrimSpace(strings.SplitN(f, "=", 2)[1])
				continue
			}
			if looksDate(f) {
				expRaw = f
			}
		}
		if len(fields) >= 5 {
			if n, err := strconv.Atoi(fields[len(fields)-1]); err == nil && n > 0 {
				limit = n
			}
		}
		exp, ok := parseTime(expRaw)
		if !ok {
			continue
		}
		res = append(res, account{Username: username, ExpiresAt: exp, ExpiryDate: dateDisplay(exp, expRaw), Limit: maxInt(limit, 1), Status: "active"})
	}
	return res
}

func resellerSuspended(ownerID int64, ownerType string) (bool, string) {
	if ownerID <= 0 || ownerType == "" || strings.EqualFold(ownerType, "admin") {
		return false, ""
	}
	rows := loadResellers()
	if len(rows) == 0 {
		return false, ""
	}
	current := ownerID
	seen := map[int64]bool{}
	first := true
	for current > 0 {
		if seen[current] {
			return true, "cadeia de revenda invalida"
		}
		seen[current] = true
		row, ok := rows[current]
		if !ok {
			return false, ""
		}
		label := "revenda"
		if !first {
			label = "revenda principal"
		}
		if active, ok := row["active"]; ok && !boolFromAny(active) {
			return true, label + " bloqueada"
		}
		if boolFromAny(row["blocked"]) {
			return true, label + " bloqueada"
		}
		expRaw := firstNonEmpty(strFromAny(row["expires_at"]), strFromAny(row["expiry"]), strFromAny(row["expires"]))
		if exp, ok := parseTime(expRaw); ok && time.Now().After(exp) {
			return true, label + " expirada"
		}
		parent := int64FromAny(firstAny(row, "parent_telegram_id", "parent_id"))
		if parent <= 0 {
			return false, ""
		}
		current = parent
		first = false
	}
	return false, ""
}

func loadResellers() map[int64]map[string]any {
	paths := []string{env("CHECKUSER_BOT_RESELLERS_JSON", "/etc/primecel-gestor/resellers.json"), "/etc/tg-access-bot/resellers.json"}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil || len(b) == 0 {
			continue
		}
		var byKey map[string]map[string]any
		if err := json.Unmarshal(b, &byKey); err != nil || len(byKey) == 0 {
			continue
		}
		out := map[int64]map[string]any{}
		for key, row := range byKey {
			id := int64FromAny(firstAny(row, "telegram_id", "id"))
			if id <= 0 {
				id, _ = strconv.ParseInt(strings.TrimSpace(key), 10, 64)
			}
			if id > 0 {
				out[id] = row
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func isActive(a account, now time.Time) bool {
	if strings.EqualFold(a.Status, "deleted") || strings.EqualFold(a.Status, "removed") || strings.EqualFold(a.Status, "blocked") {
		return false
	}
	return !a.ExpiresAt.IsZero() && a.ExpiresAt.After(now)
}

func deviceDBPath() string { return env("CHECKUSER_DB_PATH", "/root/db.sqlite3") }

func ensureDevicesTable(ctx context.Context) error {
	path := deviceDBPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	_, err := sqliteOut(ctx, path, `CREATE TABLE IF NOT EXISTS devices (id TEXT PRIMARY KEY, username TEXT);`)
	return err
}

func saveDevice(ctx context.Context, id, username string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(username) == "" {
		return nil
	}
	if err := ensureDevicesTable(ctx); err != nil {
		return err
	}
	_, err := sqliteOut(ctx, deviceDBPath(), "INSERT OR IGNORE INTO devices (id, username) VALUES ('"+sqlEscape(id)+"', '"+sqlEscape(username)+"');")
	return err
}

func deviceExists(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, nil
	}
	if err := ensureDevicesTable(ctx); err != nil {
		return false, err
	}
	out, err := sqliteOut(ctx, deviceDBPath(), "SELECT COUNT(*) FROM devices WHERE id='"+sqlEscape(id)+"';")
	if err != nil {
		return false, err
	}
	return atoi(strings.TrimSpace(out)) > 0, nil
}

func countDevicesByUsername(ctx context.Context, username string) (int, error) {
	if strings.TrimSpace(username) == "" {
		return 0, nil
	}
	if err := ensureDevicesTable(ctx); err != nil {
		return 0, err
	}
	out, err := sqliteOut(ctx, deviceDBPath(), "SELECT COUNT(*) FROM devices WHERE username='"+sqlEscape(username)+"';")
	if err != nil {
		return 0, err
	}
	return atoi(strings.TrimSpace(out)), nil
}

func listDevices(ctx context.Context, username string) ([]deviceRow, error) {
	if err := ensureDevicesTable(ctx); err != nil {
		return nil, err
	}
	sql := "SELECT id,username FROM devices ORDER BY username COLLATE NOCASE,id;"
	if strings.TrimSpace(username) != "" {
		sql = "SELECT id,username FROM devices WHERE username='" + sqlEscape(username) + "' ORDER BY id;"
	}
	out, err := sqliteOut(ctx, deviceDBPath(), sql)
	if err != nil {
		return nil, err
	}
	rows := []deviceRow{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			rows = append(rows, deviceRow{ID: parts[0], Username: parts[1]})
		}
	}
	return rows, nil
}

func deleteDevices(ctx context.Context, username string) (int, error) {
	if strings.TrimSpace(username) == "" {
		return 0, nil
	}
	count, _ := countDevicesByUsername(ctx, username)
	_, err := sqliteOut(ctx, deviceDBPath(), "DELETE FROM devices WHERE username='"+sqlEscape(username)+"';")
	return count, err
}

func dropDeviceTable(ctx context.Context) error {
	if err := ensureDevicesTable(ctx); err != nil {
		return err
	}
	_, err := sqliteOut(ctx, deviceDBPath(), `DROP TABLE IF EXISTS devices;`)
	return err
}

func sqliteOut(ctx context.Context, db, sql string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sqlite3", "-noheader", "-separator", "|", db, sql)
	b, err := cmd.CombinedOutput()
	if cctx.Err() != nil {
		return "", cctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("sqlite3: %v: %s", err, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

func getOnlineSummary(ctx context.Context) *onlineResponse {
	for _, bin := range []string{"primecel-gestor", "/usr/local/bin/primecel-gestor"} {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		cmd := exec.CommandContext(cctx, bin, "online", "summary")
		b, err := cmd.Output()
		cancel()
		if err != nil || len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		var raw struct {
			OK    bool             `json:"ok"`
			Count int              `json:"count"`
			Users []map[string]any `json:"users"`
		}
		if json.Unmarshal(b, &raw) != nil {
			continue
		}
		users := []onlineUser{}
		for _, row := range raw.Users {
			username := firstNonEmpty(strFromAny(firstAny(row, "username", "user", "login", "usuario", "name")))
			conn := intFromAny(firstAny(row, "connections", "count_connections", "count", "online", "onlines"))
			limit := intFromAny(firstAny(row, "limit", "limit_connections", "device_limit", "limite"))
			if username == "" || conn <= 0 {
				continue
			}
			users = append(users, onlineUser{Username: username, Connections: conn, Limit: maxInt(limit, 1), Sources: strSlice(row["sources"]), Modes: strSlice(row["modes"])})
		}
		return &onlineResponse{OK: true, Source: "checkuser", Users: users}
	}
	return nil
}

func bestLiveConnections(ctx context.Context, username string, limit int) int {
	resp := normalizeOnline(getOnlineSummary(ctx))
	if resp == nil {
		return 0
	}
	best := 0
	for _, u := range resp.Users {
		if strings.EqualFold(u.Username, username) && u.Connections > best {
			best = u.Connections
		}
	}
	return best
}

func normalizeOnline(resp *onlineResponse) *onlineResponse {
	if resp == nil {
		return nil
	}
	merged := map[string]*onlineUser{}
	for _, u := range resp.Users {
		if strings.TrimSpace(u.Username) == "" || u.Connections <= 0 {
			continue
		}
		key := norm(u.Username)
		if u.Limit <= 0 {
			u.Limit = 1
		}
		//if u.Connections > u.Limit {
		//	u.Connections = u.Limit
		//}
		if ex := merged[key]; ex != nil {
			ex.Connections += u.Connections
			if ex.Limit <= 0 && u.Limit > 0 {
				ex.Limit = u.Limit
			}
			ex.Sources = unique(append(ex.Sources, u.Sources...))
			ex.Modes = unique(append(ex.Modes, u.Modes...))
		} else {
			cp := u
			merged[key] = &cp
		}
	}
	out := &onlineResponse{OK: true, Source: "checkuser", Users: []onlineUser{}}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
       u := merged[key]

       out.Count += u.Connections
       out.Users = append(out.Users, *u)
    }
	return out
}

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(strings.Trim(s, `"'`))
	if s == "" || strings.EqualFold(s, "null") || strings.EqualFold(s, "nil") {
		return time.Time{}, false
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02", "02/01/2006 15:04:05", "02/01/2006", "02-01-2006", "2006/01/02"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			if layout == "2006-01-02" || layout == "02/01/2006" || layout == "02-01-2006" || layout == "2006/01/02" {
				return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.Local), true
			}
			return t, true
		}
	}
	return time.Time{}, false
}

func remainingInfo(t time.Time) (label string, days, value int, unit string, hours, minutes, totalMinutes int) {
	d := time.Until(t)
	if d <= 0 {
		return "expirado", 0, 0, "expired", 0, 0, 0
	}
	totalMinutes = int(d.Minutes())
	if totalMinutes < 1440 {
		hours = totalMinutes / 60
		minutes = totalMinutes % 60
		return two(hours) + "h:" + two(minutes), 0, 0, "days", hours, minutes, totalMinutes
	}
	days = totalMinutes / 1440
	if days == 1 {
		return "1 dia", 1, 1, "days", 24, 0, totalMinutes
	}
	return strconv.Itoa(days) + " dias", days, days, "days", days * 24, 0, totalMinutes
}

func jsonOut(w http.ResponseWriter, status int, v any) {
	setCORS(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}

	_, _ = w.Write(b)
}

func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
}

func mustJSON(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }
func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func atoi(s string) int { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }
func two(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
func formatDateBR(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("02/01/2006")
}
func dateDisplay(t time.Time, fallback string) string {
	if !t.IsZero() {
		return formatDateBR(t)
	}
	return fallback
}
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func sqlEscape(s string) string { return strings.ReplaceAll(s, "'", "''") }
func htmlEsc(s string) string   { return html.EscapeString(s) }
func _unused()                  { _ = htmlEsc }

func safeLookup(s string) bool {
	if len(s) > 128 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == '@' {
			continue
		}
		return false
	}
	return true
}

func looksDate(s string) bool {
	_, ok := parseTime(s)
	return ok
}

func firstAny(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

func strFromAny(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return fmt.Sprint(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

func intFromAny(v any) int { return int(int64FromAny(v)) }
func int64FromAny(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	default:
		n, _ := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(x)), 10, 64)
		return n
	}
}

func boolFromAny(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s == "1" || s == "true" || s == "yes" || s == "sim" || s == "s"
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	default:
		return false
	}
}

func strSlice(v any) []string {
	out := []string{}
	switch x := v.(type) {
	case []any:
		for _, it := range x {
			if s := strFromAny(it); s != "" {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, x...)
	case string:
		for _, p := range strings.Split(x, ",") {
			if strings.TrimSpace(p) != "" {
				out = append(out, strings.TrimSpace(p))
			}
		}
	}
	return unique(out)
}

func unique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	return out
}
