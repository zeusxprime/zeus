package store

/*
#cgo LDFLAGS: -lsqlite3
#include <sqlite3.h>
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"primecel-gestor/gestor_bot/model"
)

type DB struct {
	ptr  *C.sqlite3
	path string
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var db *C.sqlite3
	if rc := C.sqlite3_open(cpath, &db); rc != C.SQLITE_OK {
		return nil, fmt.Errorf("sqlite open: %s", sqliteErr(db))
	}
	d := &DB{ptr: db, path: path}
	if err := d.Exec(context.Background(), `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}
func (d *DB) Close() error {
	if d.ptr != nil {
		rc := C.sqlite3_close(d.ptr)
		d.ptr = nil
		if rc != C.SQLITE_OK {
			return errors.New("sqlite close failed")
		}
	}
	return nil
}
func sqliteErr(db *C.sqlite3) string {
	if db == nil {
		return "unknown"
	}
	return C.GoString(C.sqlite3_errmsg(db))
}

func (d *DB) Exec(ctx context.Context, sql string, args ...any) error {
	stmt, err := d.prepare(sql)
	if err != nil {
		return err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bind(stmt, args...); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rc := C.sqlite3_step(stmt)
		if rc == C.SQLITE_DONE {
			return nil
		}
		if rc == C.SQLITE_ROW {
			continue
		}
		return fmt.Errorf("sqlite exec: %s | %s", sqliteErr(d.ptr), compactSQL(sql))
	}
}

func (d *DB) Query(ctx context.Context, sql string, args ...any) ([]map[string]string, error) {
	stmt, err := d.prepare(sql)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bind(stmt, args...); err != nil {
		return nil, err
	}
	cols := int(C.sqlite3_column_count(stmt))
	rows := []map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		rc := C.sqlite3_step(stmt)
		if rc == C.SQLITE_DONE {
			return rows, nil
		}
		if rc != C.SQLITE_ROW {
			return nil, fmt.Errorf("sqlite query: %s | %s", sqliteErr(d.ptr), compactSQL(sql))
		}
		m := map[string]string{}
		for i := 0; i < cols; i++ {
			name := C.GoString(C.sqlite3_column_name(stmt, C.int(i)))
			txt := C.sqlite3_column_text(stmt, C.int(i))
			if txt != nil {
				m[name] = C.GoString((*C.char)(unsafe.Pointer(txt)))
			} else {
				m[name] = ""
			}
		}
		rows = append(rows, m)
	}
}

func (d *DB) prepare(sql string) (*C.sqlite3_stmt, error) {
	csql := C.CString(sql)
	defer C.free(unsafe.Pointer(csql))
	var stmt *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(d.ptr, csql, -1, &stmt, nil); rc != C.SQLITE_OK {
		return nil, fmt.Errorf("sqlite prepare: %s | %s", sqliteErr(d.ptr), compactSQL(sql))
	}
	return stmt, nil
}
func bind(stmt *C.sqlite3_stmt, args ...any) error {
	for i, a := range args {
		idx := C.int(i + 1)
		switch v := a.(type) {
		case nil:
			C.sqlite3_bind_null(stmt, idx)
		case string:
			cs := C.CString(v)
			C.sqlite3_bind_text(stmt, idx, cs, -1, (*[0]byte)(C.free))
		case int:
			C.sqlite3_bind_int64(stmt, idx, C.sqlite3_int64(v))
		case int64:
			C.sqlite3_bind_int64(stmt, idx, C.sqlite3_int64(v))
		case bool:
			if v {
				C.sqlite3_bind_int64(stmt, idx, 1)
			} else {
				C.sqlite3_bind_int64(stmt, idx, 0)
			}
		case float64:
			C.sqlite3_bind_double(stmt, idx, C.double(v))
		case time.Time:
			cs := C.CString(v.UTC().Format(time.RFC3339))
			C.sqlite3_bind_text(stmt, idx, cs, -1, (*[0]byte)(C.free))
		case *time.Time:
			if v == nil {
				C.sqlite3_bind_null(stmt, idx)
			} else {
				cs := C.CString(v.UTC().Format(time.RFC3339))
				C.sqlite3_bind_text(stmt, idx, cs, -1, (*[0]byte)(C.free))
			}
		default:
			cs := C.CString(fmt.Sprint(v))
			C.sqlite3_bind_text(stmt, idx, cs, -1, (*[0]byte)(C.free))
		}
	}
	return nil
}
func compactSQL(s string) string { return strings.Join(strings.Fields(s), " ") }

func (d *DB) Migrate(ctx context.Context) error {
	for _, q := range migrations {
		if err := d.Exec(ctx, q); err != nil {
			return err
		}
	}
	if err := d.ensureColumn(ctx, "accounts", "monthly_value", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return nil
}

func (d *DB) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := d.Query(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	for _, r := range rows {
		if strings.EqualFold(r["name"], column) {
			return nil
		}
	}
	return d.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
}
func (d *DB) WithTx(ctx context.Context, fn func(*DB) error) error {
	if err := d.Exec(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := fn(d); err != nil {
		_ = d.Exec(context.Background(), "ROLLBACK")
		return err
	}
	return d.Exec(ctx, "COMMIT")
}

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS accounts (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, password TEXT NOT NULL, uuid TEXT DEFAULT '', limit_connections INTEGER NOT NULL DEFAULT 1, expires_at TEXT NOT NULL, expiry_date TEXT NOT NULL, owner_telegram_id INTEGER DEFAULT 0, owner_name TEXT DEFAULT '', owner_type TEXT DEFAULT 'admin', status TEXT NOT NULL DEFAULT 'active', is_trial INTEGER NOT NULL DEFAULT 0, xray_enabled INTEGER NOT NULL DEFAULT 0, credit_counted INTEGER NOT NULL DEFAULT 0, client_whatsapp TEXT DEFAULT '', monthly_value REAL NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, deleted_at TEXT);`,
	`CREATE TABLE IF NOT EXISTS account_events (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL, event_type TEXT NOT NULL, data_json TEXT NOT NULL DEFAULT '{}', actor_telegram_id INTEGER DEFAULT 0, created_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS resellers (id INTEGER PRIMARY KEY AUTOINCREMENT, telegram_id INTEGER NOT NULL UNIQUE, name TEXT NOT NULL, whatsapp_phone TEXT, password TEXT DEFAULT '', credits INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1, max_days INTEGER NOT NULL DEFAULT 30, max_limit INTEGER NOT NULL DEFAULT 1, allow_xray INTEGER NOT NULL DEFAULT 0, allow_subreseller INTEGER NOT NULL DEFAULT 0, expires_at TEXT, parent_telegram_id INTEGER DEFAULT 0, level INTEGER NOT NULL DEFAULT 0, monthly_price REAL NOT NULL DEFAULT 0, pending_monthly_price REAL DEFAULT 0, pending_monthly_difference REAL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, deleted_at TEXT);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_resellers_whatsapp_unique ON resellers(whatsapp_phone) WHERE whatsapp_phone IS NOT NULL AND whatsapp_phone != '';`,
	`CREATE TABLE IF NOT EXISTS reseller_credit_movements (id INTEGER PRIMARY KEY AUTOINCREMENT, reseller_telegram_id INTEGER NOT NULL, amount INTEGER NOT NULL, reason TEXT NOT NULL, ref_username TEXT DEFAULT '', ref_reseller_telegram_id INTEGER DEFAULT 0, actor_telegram_id INTEGER DEFAULT 0, created_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS servers (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, host TEXT NOT NULL UNIQUE, ssh_port INTEGER NOT NULL DEFAULT 22, ssh_user TEXT NOT NULL DEFAULT 'root', ssh_password TEXT DEFAULT '', agent_port INTEGER NOT NULL DEFAULT 8787, agent_token TEXT DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS devices (id TEXT NOT NULL, username TEXT NOT NULL, limit_connections INTEGER NOT NULL DEFAULT 1, user_uuid TEXT DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL, PRIMARY KEY(id, username));`,
	`CREATE TABLE IF NOT EXISTS device_users (username TEXT PRIMARY KEY, user_uuid TEXT DEFAULT '', limit_connections INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS telegram_user_state (user_id INTEGER PRIMARY KEY, chat_id INTEGER NOT NULL, state TEXT NOT NULL, data_json TEXT NOT NULL DEFAULT '{}', flow_message_id INTEGER, content_message_id INTEGER, menu_message_id INTEGER, live_kind TEXT DEFAULT '', last_activity_at TEXT NOT NULL, updated_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS telegram_chat_messages (id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL, kind TEXT NOT NULL, protected INTEGER NOT NULL DEFAULT 0, last_text TEXT DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(chat_id,message_id));`,
	`CREATE TABLE IF NOT EXISTS telegram_chat_activity (chat_id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, last_activity_at TEXT NOT NULL, auto_home_at TEXT DEFAULT '', updated_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS cloudflare_events (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, domain TEXT DEFAULT '', ok INTEGER NOT NULL DEFAULT 0, data_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS notice_events (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, message TEXT NOT NULL, targets INTEGER NOT NULL DEFAULT 0, delivered INTEGER NOT NULL DEFAULT 0, failed INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS apps (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, version TEXT DEFAULT '', file_id TEXT DEFAULT '', file_unique_id TEXT DEFAULT '', file_name TEXT DEFAULT '', mime_type TEXT DEFAULT '', path TEXT DEFAULT '', updated_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS payment_owner_configs (owner_id INTEGER PRIMARY KEY, bank TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 0, token TEXT DEFAULT '', data_json TEXT NOT NULL DEFAULT '{}', updated_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS payment_packages (id INTEGER PRIMARY KEY AUTOINCREMENT, owner_id INTEGER NOT NULL DEFAULT 0, kind TEXT NOT NULL, name TEXT NOT NULL, months INTEGER NOT NULL DEFAULT 0, days INTEGER NOT NULL DEFAULT 0, credits INTEGER NOT NULL DEFAULT 0, amount REAL NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);`,
	`CREATE INDEX IF NOT EXISTS idx_payment_packages_owner ON payment_packages(owner_id,kind,active);`,
	`CREATE TABLE IF NOT EXISTS payment_orders (order_id TEXT PRIMARY KEY, owner_id INTEGER NOT NULL DEFAULT 0, target_reseller_id INTEGER NOT NULL DEFAULT 0, kind TEXT NOT NULL, months INTEGER NOT NULL DEFAULT 0, days INTEGER NOT NULL DEFAULT 0, credits INTEGER NOT NULL DEFAULT 0, amount REAL NOT NULL DEFAULT 0, bank TEXT NOT NULL DEFAULT '', external_id TEXT DEFAULT '', status TEXT NOT NULL DEFAULT 'pending', pix_copy_paste TEXT DEFAULT '', payment_url TEXT DEFAULT '', payload_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL, paid_at TEXT, applied_at TEXT);`,
	`CREATE TABLE IF NOT EXISTS renewal_sessions (token TEXT PRIMARY KEY, username TEXT NOT NULL, expires_at TEXT NOT NULL, created_at TEXT NOT NULL);`,
	`CREATE INDEX IF NOT EXISTS idx_renewal_sessions_expires ON renewal_sessions(expires_at);`,
	`CREATE INDEX IF NOT EXISTS idx_payment_orders_owner_status ON payment_orders(owner_id,status);`,
	`CREATE TABLE IF NOT EXISTS payment_events (id INTEGER PRIMARY KEY AUTOINCREMENT, order_id TEXT DEFAULT '', owner_id INTEGER NOT NULL DEFAULT 0, event_type TEXT NOT NULL, data_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS payment_webhook_events (id INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT DEFAULT '', order_id TEXT DEFAULT '', owner_id INTEGER NOT NULL DEFAULT 0, bank TEXT DEFAULT '', external_id TEXT DEFAULT '', status TEXT DEFAULT '', remote_ip TEXT DEFAULT '', result TEXT NOT NULL DEFAULT '', error_text TEXT DEFAULT '', headers_json TEXT NOT NULL DEFAULT '{}', body_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_webhook_events_event_id ON payment_webhook_events(event_id) WHERE event_id IS NOT NULL AND event_id != '';`,
	`CREATE INDEX IF NOT EXISTS idx_payment_webhook_events_order ON payment_webhook_events(order_id, created_at);`,
	`CREATE TABLE IF NOT EXISTS whatsapp_renewal_requests (id TEXT PRIMARY KEY, username TEXT NOT NULL, client_phone TEXT NOT NULL, seller_phone TEXT DEFAULT '', owner_id INTEGER NOT NULL DEFAULT 0, months INTEGER NOT NULL DEFAULT 1, days INTEGER NOT NULL DEFAULT 30, amount REAL NOT NULL DEFAULT 0, mode TEXT NOT NULL DEFAULT 'manual', status TEXT NOT NULL DEFAULT 'pending', proof_path TEXT DEFAULT '', order_id TEXT DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL, applied_at TEXT);`,
	`CREATE INDEX IF NOT EXISTS idx_whatsapp_renewal_seller_status ON whatsapp_renewal_requests(seller_phone,status,created_at);`,
	`CREATE TABLE IF NOT EXISTS whatsapp_sessions (phone TEXT PRIMARY KEY, state TEXT NOT NULL DEFAULT '', data_json TEXT NOT NULL DEFAULT '{}', updated_at TEXT NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS expiration_notice_state (subject_type TEXT NOT NULL, subject_key TEXT NOT NULL, expires_at TEXT NOT NULL, sent_at TEXT NOT NULL, PRIMARY KEY(subject_type,subject_key,expires_at));`,
	`CREATE TABLE IF NOT EXISTS expiration_notice_messages (id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL, delete_after TEXT NOT NULL, deleted_at TEXT DEFAULT '', created_at TEXT NOT NULL, UNIQUE(chat_id,message_id));`,
	`CREATE INDEX IF NOT EXISTS idx_expiration_notice_messages_delete ON expiration_notice_messages(delete_after, deleted_at);`,
}

func (d *DB) UpsertAccount(ctx context.Context, a model.Account) error {
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	return d.Exec(ctx, `INSERT INTO accounts(username,password,uuid,limit_connections,expires_at,expiry_date,owner_telegram_id,owner_name,owner_type,status,is_trial,xray_enabled,credit_counted,client_whatsapp,monthly_value,created_at,updated_at,deleted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(username) DO UPDATE SET password=excluded.password, uuid=excluded.uuid, limit_connections=excluded.limit_connections, expires_at=excluded.expires_at, expiry_date=excluded.expiry_date, owner_telegram_id=excluded.owner_telegram_id, owner_name=excluded.owner_name, owner_type=excluded.owner_type, status=excluded.status, is_trial=excluded.is_trial, xray_enabled=excluded.xray_enabled, credit_counted=excluded.credit_counted, client_whatsapp=excluded.client_whatsapp, monthly_value=excluded.monthly_value, updated_at=excluded.updated_at, deleted_at=excluded.deleted_at`, a.Username, a.Password, a.UUID, a.LimitConnections, a.ExpiresAt, a.ExpiryDate, a.OwnerTelegramID, a.OwnerName, a.OwnerType, a.Status, a.IsTrial, a.XrayEnabled, a.CreditCounted, a.ClientWhatsApp, a.MonthlyValue, a.CreatedAt, a.UpdatedAt, a.DeletedAt)
}
func (d *DB) FindAccount(ctx context.Context, username string) (*model.Account, error) {
	rows, err := d.Query(ctx, `SELECT * FROM accounts WHERE lower(username)=lower(?) AND deleted_at IS NULL LIMIT 1`, username)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rowAccount(rows[0]), nil
}
func (d *DB) FindAccountByUUID(ctx context.Context, uuid string) (*model.Account, error) {
	rows, err := d.Query(ctx, `SELECT * FROM accounts WHERE uuid=? AND deleted_at IS NULL LIMIT 1`, uuid)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rowAccount(rows[0]), nil
}
func (d *DB) ListAccounts(ctx context.Context, includeDeleted bool) ([]model.Account, error) {
	q := `SELECT * FROM accounts`
	if !includeDeleted {
		q += ` WHERE deleted_at IS NULL`
	}
	q += ` ORDER BY username COLLATE NOCASE`
	rows, err := d.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]model.Account, 0, len(rows))
	for _, r := range rows {
		out = append(out, *rowAccount(r))
	}
	return out, nil
}
func (d *DB) MarkAccountDeleted(ctx context.Context, username string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return d.Exec(ctx, `UPDATE accounts SET status='deleted', deleted_at=?, updated_at=? WHERE lower(username)=lower(?)`, now, now, username)
}
func (d *DB) AddAccountEvent(ctx context.Context, username, typ, data string, actor int64) error {
	return d.Exec(ctx, `INSERT INTO account_events(username,event_type,data_json,actor_telegram_id,created_at) VALUES(?,?,?,?,?)`, username, typ, data, actor, time.Now().UTC().Format(time.RFC3339))
}

func (d *DB) UpsertReseller(ctx context.Context, r model.Reseller) error {
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	return d.Exec(ctx, `INSERT INTO resellers(telegram_id,name,whatsapp_phone,password,credits,active,max_days,max_limit,allow_xray,allow_subreseller,expires_at,parent_telegram_id,level,monthly_price,pending_monthly_price,pending_monthly_difference,created_at,updated_at,deleted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(telegram_id) DO UPDATE SET name=excluded.name, whatsapp_phone=excluded.whatsapp_phone, password=excluded.password, credits=excluded.credits, active=excluded.active, max_days=excluded.max_days, max_limit=excluded.max_limit, allow_xray=excluded.allow_xray, allow_subreseller=excluded.allow_subreseller, expires_at=excluded.expires_at, parent_telegram_id=excluded.parent_telegram_id, level=excluded.level, monthly_price=excluded.monthly_price, pending_monthly_price=excluded.pending_monthly_price, pending_monthly_difference=excluded.pending_monthly_difference, updated_at=excluded.updated_at, deleted_at=excluded.deleted_at`, r.TelegramID, r.Name, r.WhatsAppPhone, r.Password, r.Credits, r.Active, r.MaxDays, r.MaxLimit, r.AllowXray, r.AllowSubReseller, r.ExpiresAt, r.ParentTelegramID, r.Level, r.MonthlyPrice, r.PendingMonthlyPrice, r.PendingMonthlyDifference, r.CreatedAt, r.UpdatedAt, r.DeletedAt)
}
func (d *DB) FindReseller(ctx context.Context, id int64) (*model.Reseller, error) {
	rows, err := d.Query(ctx, `SELECT * FROM resellers WHERE telegram_id=? AND deleted_at IS NULL LIMIT 1`, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rowReseller(rows[0]), nil
}
func (d *DB) ListResellers(ctx context.Context) ([]model.Reseller, error) {
	rows, err := d.Query(ctx, `SELECT * FROM resellers WHERE deleted_at IS NULL ORDER BY level, name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	out := make([]model.Reseller, 0, len(rows))
	for _, r := range rows {
		out = append(out, *rowReseller(r))
	}
	return out, nil
}

func (d *DB) MarkResellerDeleted(ctx context.Context, telegramID int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return d.Exec(ctx, `UPDATE resellers SET active=0, deleted_at=?, updated_at=? WHERE telegram_id=?`, now, now, telegramID)
}

func (d *DB) AddCreditMovement(ctx context.Context, resellerID int64, amount int, reason, refUser string, refResellerID, actorID int64) error {
	return d.Exec(ctx, `INSERT INTO reseller_credit_movements(reseller_telegram_id,amount,reason,ref_username,ref_reseller_telegram_id,actor_telegram_id,created_at) VALUES(?,?,?,?,?,?,?)`, resellerID, amount, reason, refUser, refResellerID, actorID, time.Now().UTC().Format(time.RFC3339))
}

func (d *DB) UpsertServer(ctx context.Context, srv model.Server) error {
	now := time.Now().UTC()
	if srv.CreatedAt.IsZero() {
		srv.CreatedAt = now
	}
	srv.UpdatedAt = now
	return d.Exec(ctx, `INSERT INTO servers(name,host,ssh_port,ssh_user,ssh_password,agent_port,agent_token,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(host) DO UPDATE SET name=excluded.name, ssh_port=excluded.ssh_port, ssh_user=excluded.ssh_user, ssh_password=excluded.ssh_password, agent_port=excluded.agent_port, agent_token=excluded.agent_token, enabled=excluded.enabled, updated_at=excluded.updated_at`, srv.Name, srv.Host, srv.SSHPort, srv.SSHUser, srv.SSHPassword, srv.AgentPort, srv.AgentToken, srv.Enabled, srv.CreatedAt, srv.UpdatedAt)
}
func (d *DB) ListServers(ctx context.Context) ([]model.Server, error) {
	rows, err := d.Query(ctx, `SELECT * FROM servers WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	out := make([]model.Server, 0, len(rows))
	for _, r := range rows {
		out = append(out, *rowServer(r))
	}
	return out, nil
}
func (d *DB) FindServer(ctx context.Context, id int64) (*model.Server, error) {
	rows, err := d.Query(ctx, `SELECT * FROM servers WHERE id=? AND enabled=1 LIMIT 1`, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rowServer(rows[0]), nil
}
func (d *DB) DeleteServer(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return d.Exec(ctx, `UPDATE servers SET enabled=0, updated_at=? WHERE id=?`, now, id)
}
func (d *DB) AddServerSyncLog(ctx context.Context, serverHost, action string, ok bool, output string) error {
	// Mantido em tabela de eventos genérica nesta base; tabela dedicada entra com sync/logs completos.
	status := "0"
	if ok {
		status = "1"
	}
	return d.Exec(ctx, `INSERT INTO account_events(username,event_type,data_json,actor_telegram_id,created_at) VALUES(?,?,?,?,?)`, serverHost, "remote_sync_"+action, `{"ok":`+status+`,"output":`+quoteJSON(output)+`}`, 0, time.Now().UTC().Format(time.RFC3339))
}

func (d *DB) UpsertDeviceUser(ctx context.Context, username, uuid string, limit int) error {
	return d.Exec(ctx, `INSERT INTO device_users(username,user_uuid,limit_connections,updated_at) VALUES(?,?,?,?) ON CONFLICT(username) DO UPDATE SET user_uuid=excluded.user_uuid, limit_connections=excluded.limit_connections, updated_at=excluded.updated_at`, username, uuid, limit, time.Now().UTC().Format(time.RFC3339))
}
func (d *DB) ClearDevicesForUser(ctx context.Context, username string, removeMeta bool) error {
	if err := d.Exec(ctx, `DELETE FROM devices WHERE lower(username)=lower(?)`, username); err != nil {
		return err
	}
	if removeMeta {
		return d.Exec(ctx, `DELETE FROM device_users WHERE lower(username)=lower(?)`, username)
	}
	return nil
}
func (d *DB) CountDevices(ctx context.Context, username string) (int, error) {
	rows, err := d.Query(ctx, `SELECT COUNT(DISTINCT id) AS n FROM devices WHERE lower(username)=lower(?)`, username)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return atoi(rows[0]["n"]), nil
}
func (d *DB) DeviceExists(ctx context.Context, username, id string) (bool, error) {
	rows, err := d.Query(ctx, `SELECT 1 AS x FROM devices WHERE lower(username)=lower(?) AND id=? LIMIT 1`, username, id)
	return len(rows) > 0, err
}
func (d *DB) AddDevice(ctx context.Context, username, id, uuid string, limit int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return d.Exec(ctx, `INSERT OR IGNORE INTO devices(id,username,limit_connections,user_uuid,created_at,updated_at) VALUES(?,?,?,?,?,?)`, id, username, limit, uuid, now, now)
}

func (d *DB) ListDevices(ctx context.Context, username string) ([]model.Device, error) {
	query := `SELECT id,username,limit_connections,user_uuid,created_at,updated_at FROM devices`
	args := []any{}
	if strings.TrimSpace(username) != "" {
		query += ` WHERE lower(username)=lower(?)`
		args = append(args, username)
	}
	query += ` ORDER BY username,id`
	rows, err := d.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]model.Device, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.Device{
			ID:               r["id"],
			Username:         r["username"],
			LimitConnections: atoi(r["limit_connections"]),
			UserUUID:         r["user_uuid"],
			CreatedAt:        parseTime(r["created_at"]),
			UpdatedAt:        parseTime(r["updated_at"]),
		})
	}
	return out, nil
}

func rowServer(r map[string]string) *model.Server {
	return &model.Server{ID: int64(atoi(r["id"])), Name: r["name"], Host: r["host"], SSHPort: atoi(r["ssh_port"]), SSHUser: r["ssh_user"], SSHPassword: r["ssh_password"], AgentPort: atoi(r["agent_port"]), AgentToken: r["agent_token"], Enabled: isTrue(r["enabled"]), CreatedAt: parseTime(r["created_at"]), UpdatedAt: parseTime(r["updated_at"])}
}
func quoteJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

func rowAccount(r map[string]string) *model.Account {
	var deleted *time.Time
	if t := parseTime(r["deleted_at"]); !t.IsZero() {
		deleted = &t
	}
	return &model.Account{ID: int64(atoi(r["id"])), Username: r["username"], Password: r["password"], UUID: r["uuid"], LimitConnections: atoi(r["limit_connections"]), ExpiresAt: parseTime(r["expires_at"]), ExpiryDate: r["expiry_date"], OwnerTelegramID: int64(atoi(r["owner_telegram_id"])), OwnerName: r["owner_name"], OwnerType: r["owner_type"], Status: r["status"], IsTrial: isTrue(r["is_trial"]), XrayEnabled: isTrue(r["xray_enabled"]), CreditCounted: isTrue(r["credit_counted"]), ClientWhatsApp: r["client_whatsapp"], MonthlyValue: atof(r["monthly_value"]), CreatedAt: parseTime(r["created_at"]), UpdatedAt: parseTime(r["updated_at"]), DeletedAt: deleted}
}
func rowReseller(r map[string]string) *model.Reseller {
	return &model.Reseller{ID: int64(atoi(r["id"])), TelegramID: int64(atoi(r["telegram_id"])), Name: r["name"], WhatsAppPhone: r["whatsapp_phone"], Password: r["password"], Credits: atoi(r["credits"]), Active: isTrue(r["active"]), MaxDays: atoi(r["max_days"]), MaxLimit: atoi(r["max_limit"]), AllowXray: isTrue(r["allow_xray"]), AllowSubReseller: isTrue(r["allow_subreseller"]), ExpiresAt: parseTime(r["expires_at"]), ParentTelegramID: int64(atoi(r["parent_telegram_id"])), Level: atoi(r["level"]), MonthlyPrice: atof(r["monthly_price"]), PendingMonthlyPrice: atof(r["pending_monthly_price"]), PendingMonthlyDifference: atof(r["pending_monthly_difference"]), CreatedAt: parseTime(r["created_at"]), UpdatedAt: parseTime(r["updated_at"])}
}
func atoi(s string) int     { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }
func atof(s string) float64 { f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return f }
func isTrue(s string) bool  { return s == "1" || strings.EqualFold(s, "true") }
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02", "02/01/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return d.Exec(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, key, value, now)
}
func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	rows, err := d.Query(ctx, `SELECT value FROM settings WHERE key=? LIMIT 1`, key)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0]["value"], nil
}
func (d *DB) UpsertApp(ctx context.Context, app model.App) error {
	now := time.Now().UTC()
	if app.UpdatedAt.IsZero() {
		app.UpdatedAt = now
	}
	return d.Exec(ctx, `INSERT INTO apps(name,version,file_id,file_unique_id,file_name,mime_type,path,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET version=excluded.version, file_id=excluded.file_id, file_unique_id=excluded.file_unique_id, file_name=excluded.file_name, mime_type=excluded.mime_type, path=excluded.path, updated_at=excluded.updated_at`, app.Name, app.Version, app.FileID, app.FileUniqueID, app.FileName, app.MimeType, app.Path, app.UpdatedAt)
}
func (d *DB) ListApps(ctx context.Context) ([]model.App, error) {
	rows, err := d.Query(ctx, `SELECT * FROM apps ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	out := make([]model.App, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.App{ID: int64(atoi(r["id"])), Name: r["name"], Version: r["version"], FileID: r["file_id"], FileUniqueID: r["file_unique_id"], FileName: r["file_name"], MimeType: r["mime_type"], Path: r["path"], UpdatedAt: parseTime(r["updated_at"])})
	}
	return out, nil
}
func (d *DB) FindApp(ctx context.Context, name string) (*model.App, error) {
	rows, err := d.Query(ctx, `SELECT * FROM apps WHERE lower(name)=lower(?) LIMIT 1`, name)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &model.App{ID: int64(atoi(r["id"])), Name: r["name"], Version: r["version"], FileID: r["file_id"], FileUniqueID: r["file_unique_id"], FileName: r["file_name"], MimeType: r["mime_type"], Path: r["path"], UpdatedAt: parseTime(r["updated_at"])}, nil
}
func (d *DB) FindAppByID(ctx context.Context, id int64) (*model.App, error) {
	rows, err := d.Query(ctx, `SELECT * FROM apps WHERE id=? LIMIT 1`, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &model.App{ID: int64(atoi(r["id"])), Name: r["name"], Version: r["version"], FileID: r["file_id"], FileUniqueID: r["file_unique_id"], FileName: r["file_name"], MimeType: r["mime_type"], Path: r["path"], UpdatedAt: parseTime(r["updated_at"])}, nil
}
func (d *DB) DeleteApp(ctx context.Context, name string) error {
	return d.Exec(ctx, `DELETE FROM apps WHERE lower(name)=lower(?)`, name)
}
func (d *DB) AddNoticeEvent(ctx context.Context, kind, message string, targets, delivered, failed int) error {
	return d.Exec(ctx, `INSERT INTO notice_events(kind,message,targets,delivered,failed,created_at) VALUES(?,?,?,?,?,?)`, kind, message, targets, delivered, failed, time.Now().UTC().Format(time.RFC3339))
}
func (d *DB) AddCloudflareEvent(ctx context.Context, kind, domain string, ok bool, data string) error {
	return d.Exec(ctx, `INSERT INTO cloudflare_events(kind,domain,ok,data_json,created_at) VALUES(?,?,?,?,?)`, kind, domain, ok, data, time.Now().UTC().Format(time.RFC3339))
}

func (d *DB) UpsertPaymentOwnerConfig(ctx context.Context, c model.PaymentOwnerConfig) error {
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = time.Now().UTC()
	}
	return d.Exec(ctx, `INSERT INTO payment_owner_configs(owner_id,bank,enabled,token,data_json,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(owner_id) DO UPDATE SET bank=excluded.bank, enabled=excluded.enabled, token=excluded.token, data_json=excluded.data_json, updated_at=excluded.updated_at`, c.OwnerID, c.Bank, c.Enabled, c.Token, c.DataJSON, c.UpdatedAt)
}
func (d *DB) FindPaymentOwnerConfig(ctx context.Context, ownerID int64) (*model.PaymentOwnerConfig, error) {
	rows, err := d.Query(ctx, `SELECT * FROM payment_owner_configs WHERE owner_id=? LIMIT 1`, ownerID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &model.PaymentOwnerConfig{OwnerID: int64(atoi(r["owner_id"])), Bank: r["bank"], Enabled: isTrue(r["enabled"]), Token: r["token"], DataJSON: r["data_json"], UpdatedAt: parseTime(r["updated_at"])}, nil
}
func (d *DB) UpsertPaymentPackage(ctx context.Context, p model.PaymentPackage) (*model.PaymentPackage, error) {
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if p.ID > 0 {
		if err := d.Exec(ctx, `UPDATE payment_packages SET owner_id=?, kind=?, name=?, months=?, days=?, credits=?, amount=?, active=?, updated_at=? WHERE id=?`, p.OwnerID, p.Kind, p.Name, p.Months, p.Days, p.Credits, p.Amount, p.Active, p.UpdatedAt, p.ID); err != nil {
			return nil, err
		}
		return &p, nil
	}
	if err := d.Exec(ctx, `INSERT INTO payment_packages(owner_id,kind,name,months,days,credits,amount,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, p.OwnerID, p.Kind, p.Name, p.Months, p.Days, p.Credits, p.Amount, p.Active, p.CreatedAt, p.UpdatedAt); err != nil {
		return nil, err
	}
	rows, err := d.Query(ctx, `SELECT * FROM payment_packages WHERE rowid=last_insert_rowid()`)
	if err != nil || len(rows) == 0 {
		return &p, err
	}
	return rowPaymentPackage(rows[0]), nil
}
func (d *DB) ListPaymentPackages(ctx context.Context, ownerID int64, onlyActive bool) ([]model.PaymentPackage, error) {
	q := `SELECT * FROM payment_packages WHERE owner_id=?`
	if onlyActive {
		q += ` AND active=1`
	}
	q += ` ORDER BY kind, amount, id`
	rows, err := d.Query(ctx, q, ownerID)
	if err != nil {
		return nil, err
	}
	out := make([]model.PaymentPackage, 0, len(rows))
	for _, r := range rows {
		out = append(out, *rowPaymentPackage(r))
	}
	return out, nil
}
func (d *DB) InsertPaymentOrder(ctx context.Context, o model.PaymentOrder) error {
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	if o.Status == "" {
		o.Status = "pending"
	}
	return d.Exec(ctx, `INSERT INTO payment_orders(order_id,owner_id,target_reseller_id,kind,months,days,credits,amount,bank,external_id,status,pix_copy_paste,payment_url,payload_json,created_at,paid_at,applied_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, o.OrderID, o.OwnerID, o.TargetResellerID, o.Kind, o.Months, o.Days, o.Credits, o.Amount, o.Bank, o.ExternalID, o.Status, o.PixCopyPaste, o.PaymentURL, o.PayloadJSON, o.CreatedAt, o.PaidAt, o.AppliedAt)
}
func (d *DB) FindPaymentOrder(ctx context.Context, orderID string) (*model.PaymentOrder, error) {
	rows, err := d.Query(ctx, `SELECT * FROM payment_orders WHERE order_id=? LIMIT 1`, orderID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rowPaymentOrder(rows[0]), nil
}
func (d *DB) UpdatePaymentOrder(ctx context.Context, o model.PaymentOrder) error {
	return d.Exec(ctx, `UPDATE payment_orders SET owner_id=?, target_reseller_id=?, kind=?, months=?, days=?, credits=?, amount=?, bank=?, external_id=?, status=?, pix_copy_paste=?, payment_url=?, payload_json=?, paid_at=?, applied_at=? WHERE order_id=?`, o.OwnerID, o.TargetResellerID, o.Kind, o.Months, o.Days, o.Credits, o.Amount, o.Bank, o.ExternalID, o.Status, o.PixCopyPaste, o.PaymentURL, o.PayloadJSON, o.PaidAt, o.AppliedAt, o.OrderID)
}
func (d *DB) ListPaymentOrders(ctx context.Context, ownerID int64, status string) ([]model.PaymentOrder, error) {
	q := `SELECT * FROM payment_orders WHERE 1=1`
	args := []any{}
	if ownerID >= 0 {
		q += ` AND owner_id=?`
		args = append(args, ownerID)
	}
	if strings.TrimSpace(status) != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 100`
	rows, err := d.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := make([]model.PaymentOrder, 0, len(rows))
	for _, r := range rows {
		out = append(out, *rowPaymentOrder(r))
	}
	return out, nil
}

func (d *DB) ListPaymentOrdersByTarget(ctx context.Context, targetResellerID int64, status string) ([]model.PaymentOrder, error) {
	q := `SELECT * FROM payment_orders WHERE target_reseller_id=?`
	args := []any{targetResellerID}
	if strings.TrimSpace(status) != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 20`
	rows, err := d.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := make([]model.PaymentOrder, 0, len(rows))
	for _, r := range rows {
		out = append(out, *rowPaymentOrder(r))
	}
	return out, nil
}

func (d *DB) AddPaymentEvent(ctx context.Context, orderID string, ownerID int64, typ, data string) error {
	return d.Exec(ctx, `INSERT INTO payment_events(order_id,owner_id,event_type,data_json,created_at) VALUES(?,?,?,?,?)`, orderID, ownerID, typ, data, time.Now().UTC().Format(time.RFC3339))
}

func (d *DB) AddPaymentWebhookEvent(ctx context.Context, ev model.PaymentWebhookEvent) (bool, error) {
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	if ev.HeadersJSON == "" {
		ev.HeadersJSON = "{}"
	}
	if ev.BodyJSON == "" {
		ev.BodyJSON = "{}"
	}
	beforeRows, _ := d.Query(ctx, `SELECT COUNT(*) AS n FROM payment_webhook_events WHERE event_id=? AND event_id!=''`, ev.EventID)
	if ev.EventID != "" && len(beforeRows) > 0 && atoi(beforeRows[0]["n"]) > 0 {
		return false, nil
	}
	err := d.Exec(ctx, `INSERT OR IGNORE INTO payment_webhook_events(event_id,order_id,owner_id,bank,external_id,status,remote_ip,result,error_text,headers_json,body_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, ev.EventID, ev.OrderID, ev.OwnerID, ev.Bank, ev.ExternalID, ev.Status, ev.RemoteIP, ev.Result, ev.ErrorText, ev.HeadersJSON, ev.BodyJSON, ev.CreatedAt)
	if err != nil {
		return false, err
	}
	if ev.EventID == "" {
		return true, nil
	}
	afterRows, _ := d.Query(ctx, `SELECT COUNT(*) AS n FROM payment_webhook_events WHERE event_id=?`, ev.EventID)
	return len(afterRows) > 0 && atoi(afterRows[0]["n"]) > 0, nil
}

func (d *DB) ListPaymentWebhookEvents(ctx context.Context, ownerID int64, limit int) ([]model.PaymentWebhookEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	q := `SELECT * FROM payment_webhook_events WHERE 1=1`
	args := []any{}
	if ownerID >= 0 {
		q += ` AND owner_id=?`
		args = append(args, ownerID)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := make([]model.PaymentWebhookEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.PaymentWebhookEvent{
			ID: int64(atoi(r["id"])), EventID: r["event_id"], OrderID: r["order_id"], OwnerID: int64(atoi(r["owner_id"])), Bank: r["bank"], ExternalID: r["external_id"], Status: r["status"], RemoteIP: r["remote_ip"], Result: r["result"], ErrorText: r["error_text"], HeadersJSON: r["headers_json"], BodyJSON: r["body_json"], CreatedAt: parseTime(r["created_at"]),
		})
	}
	return out, nil
}

func rowPaymentPackage(r map[string]string) *model.PaymentPackage {
	return &model.PaymentPackage{ID: int64(atoi(r["id"])), OwnerID: int64(atoi(r["owner_id"])), Kind: r["kind"], Name: r["name"], Months: atoi(r["months"]), Days: atoi(r["days"]), Credits: atoi(r["credits"]), Amount: atof(r["amount"]), Active: isTrue(r["active"]), CreatedAt: parseTime(r["created_at"]), UpdatedAt: parseTime(r["updated_at"])}
}
func rowPaymentOrder(r map[string]string) *model.PaymentOrder {
	var paid, applied *time.Time
	if t := parseTime(r["paid_at"]); !t.IsZero() {
		paid = &t
	}
	if t := parseTime(r["applied_at"]); !t.IsZero() {
		applied = &t
	}
	return &model.PaymentOrder{OrderID: r["order_id"], OwnerID: int64(atoi(r["owner_id"])), TargetResellerID: int64(atoi(r["target_reseller_id"])), Kind: r["kind"], Months: atoi(r["months"]), Days: atoi(r["days"]), Credits: atoi(r["credits"]), Amount: atof(r["amount"]), Bank: r["bank"], ExternalID: r["external_id"], Status: r["status"], PixCopyPaste: r["pix_copy_paste"], PaymentURL: r["payment_url"], PayloadJSON: r["payload_json"], CreatedAt: parseTime(r["created_at"]), PaidAt: paid, AppliedAt: applied}
}
