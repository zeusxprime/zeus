package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/accounts"
	"primecel-gestor/gestor_bot/apps"
	"primecel-gestor/gestor_bot/backup"
	"primecel-gestor/gestor_bot/checkuser"
	"primecel-gestor/gestor_bot/checkuserdb"
	"primecel-gestor/gestor_bot/cloudflare"
	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/legacy"
	"primecel-gestor/gestor_bot/mirrors"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/online"
	"primecel-gestor/gestor_bot/payments"
	"primecel-gestor/gestor_bot/remoteagent"
	"primecel-gestor/gestor_bot/resellers"
	"primecel-gestor/gestor_bot/settings"
	"primecel-gestor/gestor_bot/store"
	remotesync "primecel-gestor/gestor_bot/sync"
	"primecel-gestor/gestor_bot/system"
	"primecel-gestor/gestor_bot/telegram"
	"primecel-gestor/gestor_bot/whatsapp"
	"primecel-gestor/gestor_bot/xray"
)

const version = "prime_go_v108_checkuser_installer_offline_fix"

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	if cmd == "version" || cmd == "--version" || cmd == "-v" {
		fmt.Println("primecel-gestor", version)
		return
	}

	cfg, err := config.Load("")
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("sqlite: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		log.Fatalf("migrate sqlite: %v", err)
	}
	sanitizeLegacyDNSSettings(context.Background(), st)

	deps := buildDeps(cfg, st)
	switch cmd {
	case "migrate":
		runMigrate(context.Background(), cfg, st, args)
	case "users":
		runUsers(context.Background(), deps, args)
	case "resellers":
		runResellers(context.Background(), deps, args)
	case "servers":
		runServers(context.Background(), deps, args)
	case "sync":
		runSync(context.Background(), cfg, st, deps, args)
	case "online":
		runOnline(context.Background(), cfg, st, args)
	case "checkuser":
		runCheckUser(context.Background(), cfg, st, deps, args)
	case "agent":
		runAgent(context.Background(), cfg, st, deps, args)
	case "xray":
		runXray(context.Background(), cfg, st, args)
	case "backup":
		runBackup(context.Background(), deps, args)

	case "apps":
		runApps(context.Background(), cfg, st, args)
	case "cloudflare":
		runCloudflare(context.Background(), cfg, st, args)
	case "payments":
		runPayments(context.Background(), cfg, st, deps, args)
	case "settings":
		runSettings(context.Background(), cfg, st, args)
	case "bot":
		runBot(context.Background(), cfg, st, deps)
	case "whatsapp-handle":
		runWhatsAppHandle(context.Background(), cfg, st, deps, args)
	default:
		fmt.Fprintf(os.Stderr, "comando desconhecido: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

func sanitizeLegacyDNSSettings(ctx context.Context, st *store.DB) {
	// DNS VPS foi removido; limpa valores salvos antigos para evitar que
	// scripts/configurações legadas tentem reaproveitar sv.primecel.shop ou server.primecel.shop.
	_ = st.SetSetting(ctx, "vpn_dns_domain", "")
	_ = st.SetSetting(ctx, "vpn_dns_enabled", "0")
	for _, key := range []string{"cloudflare_domain", "server_dns_domain", "vpn_domain"} {
		if v, _ := st.GetSetting(ctx, key); cloudflare.IsLegacyServerDomain(v) || cloudflare.IsAllowedDNSVPSDomain(v) {
			_ = st.SetSetting(ctx, key, "")
		}
	}
}

type deps struct {
	cfg config.Config
	st  *store.DB
	acc *accounts.Service
	res *resellers.Service
}

func buildDeps(cfg config.Config, st *store.DB) deps {
	sys := system.NewLocalManager(cfg)
	mw := mirrors.NewWriter(cfg, st)
	rs := resellers.NewService(st, mw)
	xm := xray.NewManager(cfg)
	as := accounts.NewService(accounts.ServiceDeps{Store: st, System: sys, Mirrors: mw, Resellers: rs, Xray: xm, XrayOpts: xray.ApplyOptions{SafeRestart: true, NoRestart: false}, Config: cfg})
	return deps{cfg: cfg, st: st, acc: as, res: rs}
}

func usage() {
	fmt.Println(`primecel-gestor ` + version + `

Uso:
  primecel-gestor migrate legacy [--from /etc/tg-access-bot]
  primecel-gestor migrate check [--from /etc/tg-access-bot]
  primecel-gestor users create --username Cliente1 --password 12345 --days 30 --limit 1 [--uuid UUID] [--owner-id ID] [--owner-name Nome] [--owner-type admin|reseller|subreseller] [--trial]
  primecel-gestor users renew --username Cliente1 --days 30
  primecel-gestor users password --username Cliente1 --password 54321
  primecel-gestor users limit --username Cliente1 --limit 2
  primecel-gestor users remove --username Cliente1
  primecel-gestor resellers list
  primecel-gestor servers add --host 1.2.3.4 [--name Sv1] [--token TOKEN] [--agent-port 8787]
  primecel-gestor servers list
  primecel-gestor backup create [--output /etc/primecel-gestor/backups/backup-painel.tar.gz]
  primecel-gestor backup import --file backup-painel.tar.gz [--clean --confirm IMPORTAR] [--sync-remotes]
  primecel-gestor apps import --name PrimeCel --file app.apk --version 1.0
  primecel-gestor apps list
  primecel-gestor cloudflare server-sync --auto [--dry-run]
  primecel-gestor payments config --owner-id 0 --bank mercado_pago --token TOKEN --enabled
  primecel-gestor payments package upsert --owner-id 0 --kind limit --name 10-limites --limits 10 --amount 20
  primecel-gestor payments order create --owner-id 0 --target-reseller-id 123 --kind renew_limit --months 1 --limits 10 --amount 50 --bank mercado_pago
  primecel-gestor payments order paid --order-id PC...
  primecel-gestor payments webhook --start --port 8099   # expõe Webhook Pix, GET /api/plans e API de renovação
  primecel-gestor payments webhook-events [--owner-id -1]
  primecel-gestor whatsapp-handle --from 5585999999999 --text "menu"
  primecel-gestor settings set-profile --name Admin --whatsapp 5585999999999
  primecel-gestor sync state
  primecel-gestor online summary # onlines gerais: principal + secundárias
  primecel-gestor online local
  primecel-gestor online diag   # diagnóstico das fontes de online dos servidores
  primecel-gestor checkuser --start
  primecel-gestor agent --start [--host 0.0.0.0] [--port 8787]
  primecel-gestor xray apply --username Cliente1 --uuid UUID --expiry 2026-07-02 --limit 1 [--no-restart]
  primecel-gestor xray remove --username Cliente1 [--uuid UUID] [--no-restart]
  primecel-gestor version`)
}

func expiredAccessSuspenderLoop(ctx context.Context, cfg config.Config, st *store.DB, d deps) {
	runExpiredAccessSuspension(ctx, cfg, st, d)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runExpiredAccessSuspension(ctx, cfg, st, d)
		}
	}
}

func runExpiredAccessSuspension(ctx context.Context, cfg config.Config, st *store.DB, d deps) {
	if d.acc == nil {
		return
	}
	suspended, err := d.acc.SuspendExpiredAccess(ctx)
	if err != nil {
		payload, _ := json.Marshal(map[string]any{"error": err.Error()})
		_ = st.AddAccountEvent(ctx, "", "auto_suspend_expired_error", string(payload), 0)
		return
	}
	if len(suspended) == 0 {
		return
	}
	payload, _ := json.Marshal(map[string]any{"users": suspended})
	_ = st.AddAccountEvent(ctx, "", "auto_suspend_expired_batch", string(payload), 0)
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = remotesync.NewManager(cfg, st).SyncStateSnapshot(cctx)
	}()
}

func runBot(ctx context.Context, cfg config.Config, st *store.DB, d deps) {
	go expiredAccessSuspenderLoop(ctx, cfg, st, d)
	repairActiveTrialAccess(ctx, cfg, st)
	_ = mirrors.NewWriter(cfg, st).RefreshAll(ctx)
	b := telegram.NewBot(telegram.Services{Config: cfg, Store: st, Accounts: d.acc, Resellers: d.res, Online: online.NewManager(cfg, st), Version: version})
	fmt.Println("primecel gestor bot Telegram iniciado")
	fatalIf(b.Start(ctx))
}

func repairActiveTrialAccess(ctx context.Context, cfg config.Config, st *store.DB) {
	accs, err := st.ListAccounts(ctx, false)
	if err != nil {
		return
	}
	sys := system.NewLocalManager(cfg)
	now := time.Now().UTC()
	for _, acc := range accs {
		if !acc.IsTrial || acc.Status != "active" || acc.ExpiresAt.IsZero() || !acc.ExpiresAt.After(now) {
			continue
		}
		_ = sys.ApplyAccount(ctx, acc)
	}
}

func runMigrate(ctx context.Context, cfg config.Config, st *store.DB, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	sub := args[0]
	fs := flag.NewFlagSet("migrate "+sub, flag.ExitOnError)
	from := fs.String("from", legacy.DefaultLegacyDir, "pasta legada")
	_ = fs.Parse(args[1:])
	switch sub {
	case "legacy":
		report, err := legacy.ImportLegacy(ctx, legacy.ImportOptions{FromDir: *from, Config: cfg, Store: st, DryRun: false})
		if err != nil {
			log.Fatalf("migrate legacy: %v", err)
		}
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(b))
	case "check":
		report, err := legacy.ImportLegacy(ctx, legacy.ImportOptions{FromDir: *from, Config: cfg, Store: st, DryRun: true})
		if err != nil {
			log.Fatalf("migrate check: %v", err)
		}
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(b))
	default:
		usage()
		os.Exit(1)
	}
}

func runUsers(ctx context.Context, d deps, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("users create", flag.ExitOnError)
		username := fs.String("username", "", "usuário")
		password := fs.String("password", "", "senha")
		days := fs.Int("days", 30, "dias")
		limit := fs.Int("limit", 1, "limite")
		uuid := fs.String("uuid", "", "uuid xray")
		ownerID := fs.Int64("owner-id", 0, "telegram id do dono")
		ownerName := fs.String("owner-name", "", "nome do dono")
		ownerType := fs.String("owner-type", "admin", "admin|reseller|subreseller")
		trial := fs.Bool("trial", false, "conta teste")
		clientWA := fs.String("client-whatsapp", "", "whatsapp cliente")
		_ = fs.Parse(args[1:])
		actor := actorFromOwner(*ownerID, *ownerName, *ownerType)
		acc, err := d.acc.Create(ctx, actor, accounts.CreateDraft{Username: *username, Password: *password, Days: *days, Limit: *limit, UUID: *uuid, IsTrial: *trial, ClientWhatsApp: *clientWA})
		fatalIf(err)
		printJSON(acc)
	case "renew":
		fs := flag.NewFlagSet("users renew", flag.ExitOnError)
		username := fs.String("username", "", "usuário")
		days := fs.Int("days", 30, "dias")
		_ = fs.Parse(args[1:])
		acc, err := d.acc.Renew(ctx, model.Actor{Role: model.RoleAdmin, IsAdmin: true}, *username, *days)
		fatalIf(err)
		printJSON(acc)
	case "password":
		fs := flag.NewFlagSet("users password", flag.ExitOnError)
		username := fs.String("username", "", "usuário")
		password := fs.String("password", "", "senha")
		_ = fs.Parse(args[1:])
		acc, err := d.acc.ChangePassword(ctx, model.Actor{Role: model.RoleAdmin, IsAdmin: true}, *username, *password)
		fatalIf(err)
		printJSON(acc)
	case "limit":
		fs := flag.NewFlagSet("users limit", flag.ExitOnError)
		username := fs.String("username", "", "usuário")
		limit := fs.Int("limit", 1, "limite")
		_ = fs.Parse(args[1:])
		acc, err := d.acc.ChangeLimit(ctx, model.Actor{Role: model.RoleAdmin, IsAdmin: true}, *username, *limit)
		fatalIf(err)
		printJSON(acc)
	case "remove":
		fs := flag.NewFlagSet("users remove", flag.ExitOnError)
		username := fs.String("username", "", "usuário")
		_ = fs.Parse(args[1:])
		err := d.acc.Remove(ctx, model.Actor{Role: model.RoleAdmin, IsAdmin: true}, *username)
		fatalIf(err)
		fmt.Println("ok")
	default:
		usage()
		os.Exit(1)
	}
}

func runResellers(ctx context.Context, d deps, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	switch args[0] {
	case "list":
		rs, err := d.st.ListResellers(ctx)
		fatalIf(err)
		printJSON(rs)
	default:
		usage()
		os.Exit(1)
	}
}

func runServers(ctx context.Context, d deps, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("servers add", flag.ExitOnError)
		name := fs.String("name", "", "nome")
		host := fs.String("host", "", "ip/host")
		sshPort := fs.Int("ssh-port", 22, "porta ssh")
		sshUser := fs.String("ssh-user", "root", "usuário ssh")
		sshPass := fs.String("ssh-password", "", "senha ssh")
		agentPort := fs.Int("agent-port", d.cfg.RemoteAgentPort, "porta agente")
		token := fs.String("token", d.cfg.RemoteAgentToken, "token agente")
		_ = fs.Parse(args[1:])
		if strings.TrimSpace(*host) == "" {
			log.Fatal("--host obrigatório")
		}
		if strings.TrimSpace(*name) == "" {
			*name = "Sv"
		}
		srv := model.Server{Name: *name, Host: *host, SSHPort: *sshPort, SSHUser: *sshUser, SSHPassword: *sshPass, AgentPort: *agentPort, AgentToken: *token, Enabled: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		fatalIf(d.st.UpsertServer(ctx, srv))
		printJSON(srv)
	case "list":
		servers, err := d.st.ListServers(ctx)
		fatalIf(err)
		printJSON(servers)
	default:
		usage()
		os.Exit(1)
	}
}

func runBackup(ctx context.Context, d deps, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	mgr := backup.NewManager(d.cfg, d.st, mirrors.NewWriter(d.cfg, d.st), system.NewLocalManager(d.cfg), xray.NewManager(d.cfg))
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("backup create", flag.ExitOnError)
		out := fs.String("output", "", "arquivo de saída")
		_ = fs.Parse(args[1:])
		rep, err := mgr.Create(ctx, backup.CreateOptions{Output: *out})
		fatalIf(err)
		printJSON(rep)
	case "import":
		fs := flag.NewFlagSet("backup import", flag.ExitOnError)
		file := fs.String("file", "", "backup-painel.tar.gz")
		clean := fs.Bool("clean", false, "limpar estado atual antes de importar")
		confirm := fs.String("confirm", "", "digite IMPORTAR para limpeza destrutiva")
		syncRemotes := fs.Bool("sync-remotes", false, "enviar remoção às VPS secundárias antes da limpeza")
		_ = fs.Parse(args[1:])
		if *file == "" && fs.NArg() > 0 {
			*file = fs.Arg(0)
		}
		rep, err := mgr.Import(ctx, backup.ImportOptions{File: *file, Clean: *clean, ConfirmText: *confirm, SyncRemotes: *syncRemotes})
		fatalIf(err)
		printJSON(rep)
	default:
		usage()
		os.Exit(1)
	}
}

func runSync(ctx context.Context, cfg config.Config, st *store.DB, d deps, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	mgr := remotesync.NewManager(cfg, st)
	switch args[0] {
	case "state":
		res, err := mgr.SyncStateSnapshot(ctx)
		fatalIf(err)
		printJSON(res)
	case "remove":
		fs := flag.NewFlagSet("sync remove", flag.ExitOnError)
		username := fs.String("username", "", "usuário")
		_ = fs.Parse(args[1:])
		res, err := mgr.SyncRemove(ctx, *username)
		fatalIf(err)
		printJSON(res)
	case "apply-local":
		fs := flag.NewFlagSet("sync apply-local", flag.ExitOnError)
		file := fs.String("file", "", "arquivo JSON de sincronização")
		_ = fs.Parse(args[1:])
		resp := applyLocalSyncRequest(ctx, cfg, st, *file)
		printJSON(resp)
		if !resp.OK {
			os.Exit(2)
		}
	default:
		usage()
		os.Exit(1)
	}
}

func applyLocalSyncRequest(ctx context.Context, cfg config.Config, st *store.DB, file string) remotesync.Response {
	// Fallback SSH sempre roda em VPS secundária: nunca pode ficar em Gestor Only.
	secondaryCfg := cfg
	secondaryCfg.PrincipalManagerOnly = false
	d := buildDeps(secondaryCfg, st)
	if strings.TrimSpace(file) == "" {
		return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Error: "arquivo obrigatório"}
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Error: "falha ao ler arquivo: " + err.Error()}
	}
	var req remotesync.Request
	if err := json.Unmarshal(b, &req); err != nil {
		return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Error: "json inválido: " + err.Error()}
	}
	action := strings.TrimSpace(req.Action)
	if action == "" {
		action = "state-snapshot"
	}
	admin := model.Actor{Role: model.RoleAdmin, IsAdmin: true, Name: "Sync"}
	switch action {
	case "state-snapshot":
		applied, failed := 0, 0
		var details []string
		desired := make(map[string]bool, len(req.Accesses))
		desiredXray := make(map[string]bool, len(req.Accesses))
		for _, item := range req.Accesses {
			acc := accountFromSyncSnapshot(item)
			key := strings.ToLower(strings.TrimSpace(acc.Username))
			if key != "" {
				desired[key] = true
				if item.XrayEnabled && strings.TrimSpace(item.UUID) != "" {
					desiredXray[key] = true
				}
			}
			actor := model.Actor{TelegramID: item.OwnerTelegramID, Name: item.OwnerName, Role: model.ActorRole(item.OwnerType), IsAdmin: true}
			if actor.Role == "" {
				actor.Role = model.RoleAdmin
			}
			if _, err := d.acc.ApplyRemote(ctx, acc, actor); err != nil {
				failed++
				details = append(details, acc.Username+": "+err.Error())
				continue
			}
			applied++
		}
		removed, removeFailed, pruneDetails := d.acc.PruneRemoteToSnapshot(ctx, desired, desiredXray)
		details = append(details, pruneDetails...)
		failed += removeFailed
		if removed > 0 {
			applied += removed
		}
		if failed > 0 {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Applied: applied, Failed: failed, Details: details, Error: "falhas ao aplicar snapshot local"}
		}
		return remotesync.Response{OK: true, Agent: "primecel-gestor-cli", Version: version, Action: action, Output: "snapshot aplicado via SSH", Applied: applied, Details: details}
	case "remove", "delete":
		if strings.TrimSpace(req.Username) == "" {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: "username obrigatório"}
		}
		if err := d.acc.Remove(ctx, admin, req.Username); err != nil {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: err.Error()}
		}
		return remotesync.Response{OK: true, Agent: "primecel-gestor-cli", Version: version, Action: action, Output: "conta removida via SSH", Applied: 1}
	case "password":
		if strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.Password) == "" {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: "username e password obrigatórios"}
		}
		if _, err := d.acc.ChangePassword(ctx, admin, req.Username, req.Password); err != nil {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: err.Error()}
		}
		return remotesync.Response{OK: true, Agent: "primecel-gestor-cli", Version: version, Action: action, Output: "senha alterada via SSH", Applied: 1}
	case "limit":
		if strings.TrimSpace(req.Username) == "" || req.Limit <= 0 {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: "username e limit obrigatórios"}
		}
		if _, err := d.acc.ChangeLimit(ctx, admin, req.Username, req.Limit); err != nil {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: err.Error()}
		}
		return remotesync.Response{OK: true, Agent: "primecel-gestor-cli", Version: version, Action: action, Output: "limite alterado via SSH", Applied: 1}
	case "deviceid-user":
		if strings.TrimSpace(req.Username) == "" {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: "username obrigatório"}
		}
		if err := st.ClearDevicesForUser(ctx, req.Username, false); err != nil {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: err.Error()}
		}
		if err := checkuserdb.ClearUser(ctx, secondaryCfg.CheckUserDBPath, req.Username); err != nil {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: err.Error()}
		}
		return remotesync.Response{OK: true, Agent: "primecel-gestor-cli", Version: version, Action: action, Output: "aparelhos limpos via SSH", Applied: 1}
	case "deviceid-users":
		usernames := uniqueDeviceUsernames(req.Usernames)
		if len(usernames) == 0 {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: "nenhum usuário informado"}
		}
		for _, username := range usernames {
			if err := st.ClearDevicesForUser(ctx, username, false); err != nil {
				return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: err.Error()}
			}
		}
		if err := checkuserdb.ClearUsers(ctx, secondaryCfg.CheckUserDBPath, usernames); err != nil {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: err.Error()}
		}
		return remotesync.Response{OK: true, Agent: "primecel-gestor-cli", Version: version, Action: action, Output: "aparelhos do escopo limpos via SSH", Applied: len(usernames)}
	case "deviceid-scope":
		if err := st.Exec(ctx, `DELETE FROM devices`); err != nil {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: err.Error()}
		}
		if err := checkuserdb.ClearAll(ctx, secondaryCfg.CheckUserDBPath); err != nil {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: err.Error()}
		}
		return remotesync.Response{OK: true, Agent: "primecel-gestor-cli", Version: version, Action: action, Output: "aparelhos limpos via SSH", Applied: 1}
	case "online-count", "online-summary":
		sum, err := online.NewManager(secondaryCfg, st).LocalSummary(ctx)
		if err != nil {
			return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: err.Error()}
		}
		b, _ := json.Marshal(sum)
		return remotesync.Response{OK: true, Agent: "primecel-gestor-cli", Version: version, Action: action, Output: string(b), Applied: sum.Count}
	default:
		return remotesync.Response{OK: false, Agent: "primecel-gestor-cli", Version: version, Action: action, Error: "ação não suportada no fallback SSH: " + action}
	}
}

func reconcileMissingLocalSyncAccounts(ctx context.Context, d deps, desired map[string]bool, details *[]string) (int, int) {
	if desired == nil {
		desired = map[string]bool{}
	}
	local, err := d.st.ListAccounts(ctx, false)
	if err != nil {
		*details = append(*details, "reconciliação: "+err.Error())
		return 0, 1
	}
	removed, failed := 0, 0
	admin := model.Actor{Role: model.RoleAdmin, IsAdmin: true, Name: "Sync"}
	for _, acc := range local {
		if acc.DeletedAt != nil || acc.Status == "deleted" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(acc.Username))
		if key == "" || desired[key] {
			continue
		}
		if err := d.acc.Remove(ctx, admin, acc.Username); err != nil {
			failed++
			*details = append(*details, acc.Username+": remover ausente no principal: "+err.Error())
			continue
		}
		removed++
	}
	return removed, failed
}

func accountFromSyncSnapshot(i remotesync.SnapshotAccess) model.Account {
	exp := parseSyncExpiry(i.Expiry, i.ExpiresAt)
	expiry := i.Expiry
	if expiry == "" {
		expiry = exp.AddDate(0, 0, -1).Format("2006-01-02")
	}
	limit := i.Limit
	if limit <= 0 {
		limit = 1
	}
	ownerType := i.OwnerType
	if ownerType == "" {
		ownerType = string(model.RoleAdmin)
	}
	return model.Account{Username: i.Username, Password: i.Password, UUID: i.UUID, LimitConnections: limit, ExpiryDate: expiry, ExpiresAt: exp, OwnerTelegramID: i.OwnerTelegramID, OwnerName: i.OwnerName, OwnerType: ownerType, Status: "active", IsTrial: i.IsTrial, XrayEnabled: i.XrayEnabled, ClientWhatsApp: i.ClientWhatsApp, MonthlyValue: i.MonthlyValue}
}

func parseSyncExpiry(expiry, expiresAt string) time.Time {
	for _, raw := range []string{expiresAt, expiry} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02", "02/01/2006"} {
			if t, err := time.Parse(layout, raw); err == nil {
				if layout == "2006-01-02" || layout == "02/01/2006" {
					return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.Local).UTC()
				}
				return t
			}
		}
	}
	return time.Now().UTC().AddDate(0, 0, 30)
}

func runOnline(ctx context.Context, cfg config.Config, st *store.DB, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	switch args[0] {
	case "summary", "all", "geral":
		sum, err := online.NewManager(cfg, st).Summary(ctx)
		fatalIf(err)
		printJSON(sum)
	case "local":
		sum, err := online.NewManager(cfg, st).LocalSummary(ctx)
		fatalIf(err)
		printJSON(sum)
	case "agent-local", "agent":
		sum, err := online.NewManager(cfg, st).AgentPublicSummary(ctx)
		fatalIf(err)
		printJSON(sum)
	case "diag", "diagnostic", "diagnostico", "diagnóstico":
		printJSON(runOnlineDiagnostic(ctx, cfg, st))
	default:
		usage()
		os.Exit(1)
	}
}

type onlineDiagnosticReport struct {
	OK      bool               `json:"ok"`
	Version string             `json:"version"`
	Mode    string             `json:"mode"`
	Servers []onlineServerDiag `json:"servers"`
}

type onlineServerDiag struct {
	Name    string             `json:"name"`
	Host    string             `json:"host"`
	Enabled bool               `json:"enabled"`
	Sources []onlineSourceDiag `json:"sources"`
}

type onlineSourceDiag struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	OK         bool   `json:"ok"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Count      int    `json:"count"`
	Version    string `json:"version,omitempty"`
	Error      string `json:"error,omitempty"`
}

func runOnlineDiagnostic(ctx context.Context, cfg config.Config, st *store.DB) onlineDiagnosticReport {
	servers, _ := st.ListServers(ctx)
	report := onlineDiagnosticReport{OK: true, Version: version, Mode: "diagnostic-read-only"}
	for _, srv := range servers {
		if strings.TrimSpace(srv.Host) == "" {
			continue
		}
		port := nonZeroInt(srv.AgentPort, cfg.RemoteAgentPort, 8787)
		name := strings.TrimSpace(srv.Name)
		if name == "" {
			name = srv.Host
		}
		row := onlineServerDiag{Name: name, Host: srv.Host, Enabled: srv.Enabled}
		row.Sources = append(row.Sources,
			probeOnlineEndpoint(ctx, "agent-onlines", fmt.Sprintf("http://%s:%d/onlines", srv.Host, port), ""),
			probeOnlineEndpoint(ctx, "porta-81", fmt.Sprintf("http://%s:81/onlines", srv.Host), ""),
		)
		if token := strings.TrimSpace(firstMain(srv.AgentToken, cfg.RemoteAgentToken)); token != "" {
			row.Sources = append(row.Sources, probeOnlineEndpoint(ctx, "agent-online-summary", fmt.Sprintf("http://%s:%d/online-summary", srv.Host, port), token))
		} else {
			row.Sources = append(row.Sources, onlineSourceDiag{Name: "agent-online-summary", URL: fmt.Sprintf("http://%s:%d/online-summary", srv.Host, port), OK: false, Error: "sem token configurado"})
		}
		report.Servers = append(report.Servers, row)
	}
	return report
}

func probeOnlineEndpoint(ctx context.Context, name, rawURL, token string) onlineSourceDiag {
	out := onlineSourceDiag{Name: name, URL: rawURL}
	cctx, cancel := context.WithTimeout(ctx, 3500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, rawURL, nil)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	req.Header.Set("User-Agent", "GestorPrimecel/online-diag")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("X-Primecel-Agent-Token", token)
		q := req.URL.Query()
		q.Set("token", token)
		req.URL.RawQuery = q.Encode()
	}
	res, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer res.Body.Close()
	out.HTTPStatus = res.StatusCode
	body, _ := io.ReadAll(io.LimitReader(res.Body, 256*1024))
	if res.StatusCode >= 400 {
		out.Error = strings.TrimSpace(string(body))
		if out.Error == "" {
			out.Error = fmt.Sprintf("http %d", res.StatusCode)
		}
		return out
	}
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		out.Count = estimateOnlineCountFromText(string(body))
		out.OK = out.Count > 0 || strings.TrimSpace(string(body)) != ""
		return out
	}
	out.Count = estimateOnlineCount(raw)
	if mp, ok := raw.(map[string]any); ok {
		out.Version = stringFromAnyMain(mp["version"])
	}
	out.OK = true
	return out
}

func estimateOnlineCount(raw any) int {
	switch v := raw.(type) {
	case []any:
		return len(v)
	case map[string]any:
		for _, key := range []string{"count", "online_count", "total", "connections"} {
			if n := intFromAnyMain(v[key]); n > 0 {
				return n
			}
		}
		for _, key := range []string{"users", "onlines", "online", "items", "data", "result", "clientes", "usuarios"} {
			if val, ok := v[key]; ok {
				if n := estimateOnlineCount(val); n > 0 {
					return n
				}
			}
		}
		if user := firstMain(stringFromAnyMain(v["username"]), stringFromAnyMain(v["user"]), stringFromAnyMain(v["login"]), stringFromAnyMain(v["usuario"])); user != "" {
			return 1
		}
	}
	return 0
}

func estimateOnlineCountFromText(text string) int {
	count := 0
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		if strings.Contains(low, "nenhuma") || strings.Contains(low, "online") && len(strings.Fields(line)) <= 2 {
			continue
		}
		count++
	}
	return count
}

func firstMain(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func intFromAnyMain(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}

func stringFromAnyMain(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case fmt.Stringer:
		return strings.TrimSpace(x.String())
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

func nonZeroInt(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func runCheckUser(ctx context.Context, cfg config.Config, st *store.DB, d deps, args []string) {
	fs := flag.NewFlagSet("checkuser", flag.ExitOnError)
	start := fs.Bool("start", false, "iniciar servidor")
	host := fs.String("host", cfg.CheckUserHost, "host")
	port := fs.Int("port", cfg.CheckUserPort, "porta")
	_ = fs.Parse(args)
	if !*start {
		usage()
		os.Exit(1)
	}
	go expiredAccessSuspenderLoop(ctx, cfg, st, d)
	srv := checkuser.NewServer(cfg, st)
	addr := *host + ":" + strconv.Itoa(*port)
	fmt.Println("primecel checkuser ouvindo em", addr)
	fatalIf(http.ListenAndServe(addr, srv.Router()))
	_ = ctx
}

func runAgent(ctx context.Context, cfg config.Config, st *store.DB, d deps, args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	start := fs.Bool("start", false, "iniciar agente remoto")
	host := fs.String("host", "0.0.0.0", "host")
	port := fs.Int("port", cfg.RemoteAgentPort, "porta")
	_ = fs.Parse(args)
	if !*start {
		usage()
		os.Exit(1)
	}
	// O agente remoto sempre representa uma VPS secundária. Mesmo que o
	// config.env tenha sido copiado do principal com Gestor Only ON, aqui
	// forçamos aplicação local de SSH/usuarios.db/Xray.
	agentCfg := cfg
	agentCfg.PrincipalManagerOnly = false
	agentDeps := buildDeps(agentCfg, st)
	go expiredAccessSuspenderLoop(ctx, agentCfg, st, agentDeps)
	srv := remoteagent.NewServer(remoteagent.Services{Config: agentCfg, Store: st, Accounts: agentDeps.acc, Version: version})
	addr := *host + ":" + strconv.Itoa(*port)
	fmt.Println("primecel gestor agent ouvindo em", addr)
	fatalIf(http.ListenAndServe(addr, srv.Router()))
	_ = ctx
}

func runXray(ctx context.Context, cfg config.Config, st *store.DB, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	m := xray.NewManager(cfg)
	switch args[0] {
	case "apply":
		fs := flag.NewFlagSet("xray apply", flag.ExitOnError)
		username := fs.String("username", "", "usuário")
		uuid := fs.String("uuid", "", "uuid")
		expiry := fs.String("expiry", time.Now().UTC().Format("2006-01-02"), "validade YYYY-MM-DD")
		limit := fs.Int("limit", 1, "limite")
		noRestart := fs.Bool("no-restart", false, "não reiniciar")
		_ = fs.Parse(args[1:])
		acc := model.Account{Username: *username, UUID: *uuid, ExpiryDate: *expiry, LimitConnections: *limit}
		res, err := m.ApplyAccount(ctx, acc, xray.ApplyOptions{SafeRestart: true, NoRestart: *noRestart})
		fatalIf(err)
		printJSON(res)
	case "remove":
		fs := flag.NewFlagSet("xray remove", flag.ExitOnError)
		username := fs.String("username", "", "usuário")
		uuid := fs.String("uuid", "", "uuid")
		noRestart := fs.Bool("no-restart", false, "não reiniciar")
		_ = fs.Parse(args[1:])
		fatalIf(m.RemoveAccount(ctx, *username, *uuid, xray.ApplyOptions{SafeRestart: true, NoRestart: *noRestart}))
		fmt.Println("ok")
	default:
		usage()
		os.Exit(1)
	}
	_ = st
}

func actorFromOwner(id int64, name, typ string) model.Actor {
	role := model.ActorRole(typ)
	if role == "" {
		role = model.RoleAdmin
	}
	return model.Actor{TelegramID: id, Name: name, Role: role, IsAdmin: role == model.RoleAdmin || id == 0}
}

func fatalIf(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
func printJSON(v any) { b, _ := json.MarshalIndent(v, "", "  "); fmt.Println(string(b)) }

var _ = []any{errors.New, filepath.Join, strings.TrimSpace, time.Now}

func runApps(ctx context.Context, cfg config.Config, st *store.DB, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	mgr := apps.NewManager(cfg, st)
	switch args[0] {
	case "import":
		fs := flag.NewFlagSet("apps import", flag.ExitOnError)
		name := fs.String("name", "", "nome do app")
		file := fs.String("file", "", "arquivo .apk")
		version := fs.String("version", "1.0", "versão")
		_ = fs.Parse(args[1:])
		app, err := mgr.Import(ctx, apps.ImportOptions{Name: *name, Version: *version, SourcePath: *file})
		fatalIf(err)
		printJSON(app)
	case "list":
		list, err := mgr.List(ctx)
		fatalIf(err)
		printJSON(list)
	case "remove":
		fs := flag.NewFlagSet("apps remove", flag.ExitOnError)
		name := fs.String("name", "", "nome")
		_ = fs.Parse(args[1:])
		fatalIf(mgr.Remove(ctx, *name))
		fmt.Println("ok")
	default:
		usage()
		os.Exit(1)
	}
}

func runCloudflare(ctx context.Context, cfg config.Config, st *store.DB, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	mgr := cloudflare.NewManager(cfg, st)
	switch args[0] {
	case "server-sync", "vpn-sync":
		// Compatibilidade: o antigo vpn-sync agora garante somente o domínio padrão dos servidores.
		fs := flag.NewFlagSet("cloudflare server-sync", flag.ExitOnError)
		auto := fs.Bool("auto", false, "usar IP do principal + servidores cadastrados")
		dryRun := fs.Bool("dry-run", false, "não alterar Cloudflare")
		_ = fs.String("domain", "", "ignorado; servidores usam vpn.primecel.shop")
		_ = fs.String("ips", "", "ignorado; use --auto")
		_ = fs.Parse(args[1:])
		var ips []string
		if *auto {
			var err error
			ips, err = mgr.DesiredVPNIPs(ctx, !cfg.PrincipalManagerOnly)
			fatalIf(err)
		}
		if len(ips) == 0 {
			printJSON(map[string]any{"domain": cloudflare.DefaultServerDomain, "desired_ips": []string{}, "created": 0, "kept": 0, "deleted": 0, "dry_run": *dryRun})
			return
		}
		rep, err := mgr.SyncServerDNSIPs(ctx, ips, *dryRun)
		fatalIf(err)
		printJSON(map[string]any{"domain": rep.Domain, "desired_ips": rep.DesiredIPs, "created": rep.Created, "kept": rep.Kept, "deleted": rep.Deleted, "dry_run": rep.DryRun})
	case "checkuser-dns":
		fs := flag.NewFlagSet("cloudflare checkuser-dns", flag.ExitOnError)
		fqdn := fs.String("fqdn", "", "subdomínio check.dominio.com")
		ip := fs.String("ip", cfg.ServerHost, "IP alvo")
		dryRun := fs.Bool("dry-run", false, "não alterar Cloudflare")
		_ = fs.Parse(args[1:])
		rep, err := mgr.ConfigureCheckUserDNS(ctx, *fqdn, *ip, *dryRun)
		fatalIf(err)
		printJSON(rep)
	default:
		usage()
		os.Exit(1)
	}
}

func runSettings(ctx context.Context, cfg config.Config, st *store.DB, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	mgr := settings.NewManager(cfg, st)
	switch args[0] {
	case "set-profile":
		fs := flag.NewFlagSet("settings set-profile", flag.ExitOnError)
		name := fs.String("name", "", "nome admin")
		whatsapp := fs.String("whatsapp", "", "whatsapps admins")
		_ = fs.Parse(args[1:])
		if *name != "" {
			fatalIf(mgr.SetAdminDisplayName(ctx, *name))
		}
		if *whatsapp != "" {
			fatalIf(mgr.SetWhatsAppAdmins(ctx, *whatsapp))
		}
		fmt.Println("ok")
	case "gestor-only":
		fs := flag.NewFlagSet("settings gestor-only", flag.ExitOnError)
		enabled := fs.Bool("enabled", false, "ativar")
		_ = fs.Parse(args[1:])
		fatalIf(mgr.SetPrincipalManagerOnly(ctx, *enabled))
		fmt.Println("ok")
	case "cloudflare-token":
		fs := flag.NewFlagSet("settings cloudflare-token", flag.ExitOnError)
		token := fs.String("token", "", "token")
		_ = fs.Parse(args[1:])
		fatalIf(mgr.SetCloudflareToken(ctx, *token))
		fmt.Println("ok")
	default:
		usage()
		os.Exit(1)
	}
}

func paymentsAfterApplyHook(cfg config.Config, st *store.DB) func(context.Context, model.PaymentOrder) error {
	return func(ctx context.Context, order model.PaymentOrder) error {
		if order.Kind != payments.KindAccountRenew {
			return nil
		}
		var payload map[string]any
		_ = json.Unmarshal([]byte(order.PayloadJSON), &payload)
		username := strings.TrimSpace(fmt.Sprint(payload["username"]))
		if username == "" || username == "<nil>" {
			return errors.New("pedido de renovação sem usuário")
		}
		acc, err := st.FindAccount(ctx, username)
		if err != nil {
			return err
		}
		if acc == nil {
			return errors.New("conta renovada não encontrada")
		}

		var postErrs []string

		// Renovação pelo link/site precisa atualizar imediatamente as fontes lidas pelo CheckUser.
		// O SQLite já foi atualizado pelo pagamento; aqui regravamos users.jsonl, expirations.db
		// e usuarios.db antes de qualquer sincronização/ação externa.
		if err := mirrors.NewWriter(cfg, st).RefreshAll(ctx); err != nil {
			postErrs = append(postErrs, "mirrors: "+err.Error())
		}
		if err := syncCheckUserExpirationMirror(ctx, st, *acc); err != nil {
			postErrs = append(postErrs, "checkuser: "+err.Error())
		}

		if !cfg.PrincipalManagerOnly {
			if err := system.NewLocalManager(cfg).ApplyAccount(ctx, *acc); err != nil {
				postErrs = append(postErrs, "system: "+err.Error())
			} else if acc.XrayEnabled && acc.UUID != "" {
				if _, err := xray.NewManager(cfg).ApplyAccount(ctx, *acc, xray.ApplyOptions{SafeRestart: true, NoRestart: false}); err != nil {
					postErrs = append(postErrs, "xray: "+err.Error())
				}
			}
		}

		// Regrava depois do ApplyAccount para garantir que usuarios.db e users.jsonl
		// fiquem iguais ao estado final da conta renovada.
		if err := mirrors.NewWriter(cfg, st).RefreshAll(ctx); err != nil {
			postErrs = append(postErrs, "mirrors_final: "+err.Error())
		}

		_, err = remotesync.NewManager(cfg, st).SyncStateSnapshot(ctx)
		if err != nil {
			postErrs = append(postErrs, "remote_sync: "+err.Error())
		}
		if noticeErr := notifyAccountRenewalBySite(ctx, cfg, st, order, *acc); noticeErr != nil {
			payload, _ := json.Marshal(map[string]any{"error": noticeErr.Error(), "order_id": order.OrderID})
			_ = st.AddAccountEvent(ctx, acc.Username, "renew_site_notice_error", string(payload), order.OwnerID)
		}
		if len(postErrs) > 0 {
			payload, _ := json.Marshal(map[string]any{"errors": postErrs, "order_id": order.OrderID, "username": acc.Username})
			_ = st.AddAccountEvent(ctx, acc.Username, "renew_site_post_apply_warning", string(payload), order.OwnerID)
			return errors.New(strings.Join(postErrs, "; "))
		}
		return nil
	}
}

func notifyAccountRenewalBySite(ctx context.Context, cfg config.Config, st *store.DB, order model.PaymentOrder, acc model.Account) error {
	if strings.TrimSpace(cfg.BotToken) == "" {
		return nil
	}
	recipients := map[int64]bool{}
	if telegram.PaymentRenewalNoticeEnabled(ctx, st, 0) {
		for _, id := range cfg.AdminIDs {
			if id > 0 {
				recipients[id] = true
			}
		}
	}
	if order.OwnerID > 0 && telegram.PaymentRenewalNoticeEnabled(ctx, st, order.OwnerID) {
		recipients[order.OwnerID] = true
	}
	if acc.OwnerTelegramID > 0 && telegram.PaymentRenewalNoticeEnabled(ctx, st, acc.OwnerTelegramID) {
		recipients[acc.OwnerTelegramID] = true
	}
	sellerID := order.OwnerID
	if sellerID == 0 && acc.OwnerTelegramID > 0 {
		sellerID = acc.OwnerTelegramID
	}
	if sellerID > 0 {
		for _, watcherID := range telegram.PaymentRenewalWatchers(ctx, st, sellerID) {
			recipients[watcherID] = true
		}
	}
	if len(recipients) == 0 {
		return nil
	}

	plan := accountRenewalPlanLabel(order)
	value := accountRenewalMoneyBR(order.Amount)
	bank := strings.TrimSpace(order.Bank)
	if bank == "" {
		bank = "Pix"
	}
	text := strings.Join([]string{
		"✅ Conta renovada",
		"━━━━━━━━━━━━━━",
		"👤 Usuário: " + acc.Username,
		"━━━━━━━━━━━━━━",
		"📦 Plano: " + plan,
		"💰 Valor: " + value,
		"━━━━━━━━━━━━━━",
		"🏦 Banco: " + bank,
	}, "\n")

	sendCtx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	client := telegram.NewClient(cfg.BotToken)
	delivered := 0
	failed := 0
	var errs []string
	for id := range recipients {
		msg, err := client.SendMessage(sendCtx, id, text, telegram.InlineKeyboardMarkup{})
		if err != nil {
			failed++
			errs = append(errs, fmt.Sprintf("%d: %v", id, err))
			continue
		}
		if msg != nil && msg.MessageID != 0 {
			_ = registerExternalNoticeAutoDelete(ctx, st, id, msg.MessageID)
		}
		_ = telegram.SendMainMenuBelowNotice(sendCtx, cfg, st, id, id, version)
		delivered++
	}
	_ = st.AddNoticeEvent(ctx, "renovacao_site", text, len(recipients), delivered, failed)
	if failed > 0 && delivered == 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func registerExternalNoticeAutoDelete(ctx context.Context, st *store.DB, chatID int64, msgID int) error {
	if st == nil || chatID == 0 || msgID == 0 {
		return nil
	}
	now := time.Now().UTC()
	deleteAfter := now.Add(2 * time.Hour).Format(time.RFC3339)
	return st.Exec(ctx, `INSERT OR IGNORE INTO expiration_notice_messages(chat_id,message_id,delete_after,deleted_at,created_at) VALUES(?,?,?,?,?)`, chatID, msgID, deleteAfter, "", now.Format(time.RFC3339))
}

func accountRenewalSellerLabel(ctx context.Context, cfg config.Config, st *store.DB, ownerID int64, acc model.Account) string {
	if ownerID > 0 {
		if r, _ := st.FindReseller(ctx, ownerID); r != nil && strings.TrimSpace(r.Name) != "" {
			return strings.TrimSpace(r.Name)
		}
	}
	if strings.TrimSpace(acc.OwnerName) != "" {
		return strings.TrimSpace(acc.OwnerName)
	}
	if strings.TrimSpace(cfg.AdminDisplayName) != "" {
		return strings.TrimSpace(cfg.AdminDisplayName)
	}
	if ownerID > 0 {
		return fmt.Sprint(ownerID)
	}
	return "Admin"
}

func accountRenewalPlanLabel(order model.PaymentOrder) string {
	if order.Months > 0 {
		if order.Months == 1 {
			return "1 mês"
		}
		return fmt.Sprintf("%d meses", order.Months)
	}
	if order.Days > 0 {
		return fmt.Sprintf("%d dias", order.Days)
	}
	return "Renovação"
}

func accountRenewalMoneyBR(v float64) string {
	return "R$ " + strings.ReplaceAll(fmt.Sprintf("%.2f", v), ".", ",")
}

func accountRenewalDateBR(acc model.Account) string {
	value := strings.TrimSpace(acc.ExpiryDate)
	if value == "" && !acc.ExpiresAt.IsZero() {
		value = acc.ExpiresAt.AddDate(0, 0, -1).Format("2006-01-02")
	}
	if len(value) >= 10 {
		value = value[:10]
		parts := strings.Split(value, "-")
		if len(parts) == 3 {
			return parts[2] + "/" + parts[1] + "/" + parts[0]
		}
	}
	if !acc.ExpiresAt.IsZero() {
		return acc.ExpiresAt.AddDate(0, 0, -1).Format("02/01/2006")
	}
	return "—"
}

func syncCheckUserExpirationMirror(ctx context.Context, st *store.DB, acc model.Account) error {
	username := strings.TrimSpace(acc.Username)
	if username == "" || acc.ExpiresAt.IsZero() {
		return nil
	}
	expiry := strings.TrimSpace(acc.ExpiryDate)
	if expiry == "" {
		expiry = acc.ExpiresAt.AddDate(0, 0, -1).Format("2006-01-02")
	}
	exact := acc.ExpiresAt.UTC().Format(time.RFC3339)
	if strings.TrimSpace(exact) == "" {
		exact = expiry
	}
	line := username + " " + exact

	paths := []string{
		"/etc/DragonTeste/expirations.db",
		"/root/usuarios_expiracao.db",
		"/root/checkuser_expirations.db",
	}
	if custom := strings.TrimSpace(os.Getenv("CHECKUSER_EXACT_EXPIRATIONS_DB")); custom != "" {
		paths = append([]string{custom}, paths...)
	}

	written := 0
	var errs []string
	for _, path := range uniqueStrings(paths) {
		if path == "" {
			continue
		}
		if err := upsertCheckUserExpirationLine(path, username, line); err != nil {
			errs = append(errs, path+": "+err.Error())
			continue
		}
		written++
	}

	for _, dir := range []string{"/etc/DragonTeste/expirations", "/root/usuarios_expiracao"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			errs = append(errs, dir+": "+err.Error())
			continue
		}
		for _, name := range []string{username, strings.ToLower(username)} {
			if strings.TrimSpace(name) == "" {
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(exact+"\n"), 0o644); err != nil {
				errs = append(errs, filepath.Join(dir, name)+": "+err.Error())
				continue
			}
			written++
		}
	}

	eventPayload, _ := json.Marshal(map[string]any{
		"username": username,
		"expiry":   expiry,
		"exact":    exact,
		"written":  written,
		"errors":   errs,
	})
	_ = st.AddAccountEvent(ctx, username, "checkuser_expiration_sync", string(eventPayload), 0)

	if written == 0 && len(errs) > 0 {
		return errors.New("falha ao atualizar validade do CheckUser: " + strings.Join(errs, "; "))
	}
	return nil
}

func upsertCheckUserExpirationLine(path, username, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var out []string
	found := false
	if data, err := os.ReadFile(path); err == nil {
		for _, raw := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(raw)
			if trimmed == "" {
				continue
			}
			fields := strings.Fields(trimmed)
			if len(fields) > 0 && strings.EqualFold(fields[0], username) {
				if !found {
					out = append(out, line)
					found = true
				}
				continue
			}
			out = append(out, trimmed)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !found {
		out = append(out, line)
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func runPayments(ctx context.Context, cfg config.Config, st *store.DB, d deps, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	mgr := payments.NewManager(st).SetAfterApply(paymentsAfterApplyHook(cfg, st))
	switch args[0] {
	case "config":
		fs := flag.NewFlagSet("payments config", flag.ExitOnError)
		owner := fs.Int64("owner-id", 0, "dono/vendedor")
		bank := fs.String("bank", "mercado_pago", "mercado_pago|asaas|infinitepay")
		token := fs.String("token", "", "token do banco")
		enabled := fs.Bool("enabled", false, "ativar detector")
		data := fs.String("data-json", "{}", "config extra json")
		_ = fs.Parse(args[1:])
		cfg, err := mgr.ConfigureOwner(ctx, payments.OwnerConfigInput{OwnerID: *owner, Bank: *bank, Token: *token, Enabled: *enabled, DataJSON: *data})
		fatalIf(err)
		printJSON(cfg)
	case "package":
		if len(args) < 2 || args[1] != "upsert" {
			usage()
			os.Exit(1)
		}
		fs := flag.NewFlagSet("payments package upsert", flag.ExitOnError)
		id := fs.Int64("id", 0, "id")
		owner := fs.Int64("owner-id", 0, "dono/vendedor")
		kind := fs.String("kind", "limit", "renew|limit|renew_limit")
		name := fs.String("name", "", "nome")
		months := fs.Int("months", 0, "meses")
		days := fs.Int("days", 0, "dias")
		credits := fs.Int("credits", 0, "limites")
		limitsAlias := fs.Int("limits", 0, "limites")
		amount := fs.Float64("amount", 0, "valor")
		active := fs.Bool("active", true, "ativo")
		_ = fs.Parse(args[2:])
		limits := *credits
		if *limitsAlias > 0 {
			limits = *limitsAlias
		}
		p, err := mgr.UpsertPackage(ctx, payments.PackageInput{ID: *id, OwnerID: *owner, Kind: *kind, Name: *name, Months: *months, Days: *days, Credits: limits, Amount: *amount, Active: *active})
		fatalIf(err)
		printJSON(p)
	case "packages":
		fs := flag.NewFlagSet("payments packages", flag.ExitOnError)
		owner := fs.Int64("owner-id", 0, "dono/vendedor")
		_ = fs.Parse(args[1:])
		packs, err := st.ListPaymentPackages(ctx, *owner, false)
		fatalIf(err)
		printJSON(packs)
	case "order":
		if len(args) < 2 {
			usage()
			os.Exit(1)
		}
		switch args[1] {
		case "create":
			fs := flag.NewFlagSet("payments order create", flag.ExitOnError)
			owner := fs.Int64("owner-id", 0, "dono/vendedor")
			target := fs.Int64("target-reseller-id", 0, "revenda alvo")
			kind := fs.String("kind", "renew", "renew|limit|renew_limit")
			months := fs.Int("months", 1, "meses")
			days := fs.Int("days", 0, "dias")
			credits := fs.Int("credits", 0, "limites")
			limitsAlias := fs.Int("limits", 0, "limites")
			amount := fs.Float64("amount", 0, "valor")
			bank := fs.String("bank", "", "banco")
			desc := fs.String("description", "", "descrição")
			_ = fs.Parse(args[2:])
			limits := *credits
			if *limitsAlias > 0 {
				limits = *limitsAlias
			}
			o, err := mgr.CreateOrder(ctx, payments.OrderInput{OwnerID: *owner, TargetResellerID: *target, Kind: *kind, Months: *months, Days: *days, Credits: limits, Amount: *amount, Bank: *bank, Description: *desc})
			fatalIf(err)
			printJSON(o)
		case "paid":
			fs := flag.NewFlagSet("payments order paid", flag.ExitOnError)
			id := fs.String("order-id", "", "pedido")
			payload := fs.String("payload", "{}", "json")
			_ = fs.Parse(args[2:])
			o, err := mgr.MarkPaid(ctx, *id, []byte(*payload))
			fatalIf(err)
			printJSON(o)
		case "list":
			fs := flag.NewFlagSet("payments order list", flag.ExitOnError)
			owner := fs.Int64("owner-id", -1, "dono/vendedor")
			status := fs.String("status", "", "status")
			_ = fs.Parse(args[2:])
			orders, err := st.ListPaymentOrders(ctx, *owner, *status)
			fatalIf(err)
			printJSON(orders)
		default:
			usage()
			os.Exit(1)
		}
	case "webhook":
		fs := flag.NewFlagSet("payments webhook", flag.ExitOnError)
		start := fs.Bool("start", false, "iniciar")
		host := fs.String("host", "0.0.0.0", "host")
		port := fs.Int("port", 8099, "porta")
		_ = fs.Parse(args[1:])
		if !*start {
			usage()
			os.Exit(1)
		}
		addr := *host + ":" + strconv.Itoa(*port)
		go expiredAccessSuspenderLoop(ctx, cfg, st, d)
		fmt.Println("primecel payments webhook ouvindo em", addr)
		fatalIf(http.ListenAndServe(addr, mgr.WebhookHandler()))
		_ = cfg
	case "webhook-events":
		fs := flag.NewFlagSet("payments webhook-events", flag.ExitOnError)
		owner := fs.Int64("owner-id", -1, "dono/vendedor; -1 todos")
		limit := fs.Int("limit", 30, "limite")
		_ = fs.Parse(args[1:])
		events, err := st.ListPaymentWebhookEvents(ctx, *owner, *limit)
		fatalIf(err)
		printJSON(events)
	default:
		usage()
		os.Exit(1)
	}
}

func runWhatsAppHandle(ctx context.Context, cfg config.Config, st *store.DB, d deps, args []string) {
	fs := flag.NewFlagSet("whatsapp-handle", flag.ExitOnError)
	from := fs.String("from", "", "número remetente")
	text := fs.String("text", "", "mensagem recebida")
	jsonIn := fs.String("json", "", "request json opcional")
	_ = fs.Parse(args)
	req := whatsapp.Request{From: *from, Text: *text}
	if *jsonIn != "" {
		if err := json.Unmarshal([]byte(*jsonIn), &req); err != nil {
			fatalIf(err)
		}
		*from = req.From
		*text = req.Text
	}
	if strings.TrimSpace(req.From) == "" {
		fatalIf(errors.New("--from obrigatório"))
	}
	mgr := whatsapp.NewHandler(whatsapp.Services{Config: cfg, Store: st, Accounts: d.acc, Resellers: d.res, Apps: apps.NewManager(cfg, st), Online: online.NewManager(cfg, st)})
	resp, err := mgr.HandleRequest(ctx, req)
	fatalIf(err)
	printJSON(resp)
}

func uniqueDeviceUsernames(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}
