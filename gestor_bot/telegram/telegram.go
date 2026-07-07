package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"primecel-gestor/gestor_bot/accounts"
	"primecel-gestor/gestor_bot/apps"
	"primecel-gestor/gestor_bot/backup"
	"primecel-gestor/gestor_bot/checkuserdb"
	"primecel-gestor/gestor_bot/cloudflare"
	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/mirrors"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/notices"
	"primecel-gestor/gestor_bot/online"
	"primecel-gestor/gestor_bot/payments"
	"primecel-gestor/gestor_bot/resellers"
	"primecel-gestor/gestor_bot/settings"
	"primecel-gestor/gestor_bot/store"
	remotesync "primecel-gestor/gestor_bot/sync"
	"primecel-gestor/gestor_bot/system"
	"primecel-gestor/gestor_bot/xray"
)

type Services struct {
	Config    config.Config
	Store     *store.DB
	Accounts  *accounts.Service
	Resellers *resellers.Service
	Online    *online.Manager
	Version   string
}

type Bot struct {
	svc           Services
	client        *Client
	liveRefreshMu sync.Mutex
}

func NewBot(s Services) *Bot { return &Bot{svc: s, client: NewClient(s.Config.BotToken)} }

func (b *Bot) autoSyncStateSnapshot() {
	if b == nil || b.svc.Store == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = remotesync.NewManager(b.svc.Config, b.svc.Store).SyncStateSnapshot(ctx)
	}()
}

func (b *Bot) autoSyncRemovedAccount(username string) {
	username = strings.TrimSpace(username)
	if b == nil || b.svc.Store == nil || username == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		mgr := remotesync.NewManager(b.svc.Config, b.svc.Store)
		_, _ = mgr.SyncRemove(ctx, username)
		_, _ = mgr.SyncStateSnapshot(ctx)
	}()
}

func (b *Bot) Start(ctx context.Context) error {
	if strings.TrimSpace(b.svc.Config.BotToken) == "" {
		return errors.New("BOT_TOKEN vazio")
	}
	if len(b.svc.Config.AdminIDs) == 0 {
		return errors.New("ADMIN_IDS vazio")
	}
	_ = b.registerSystemUpdateNotice(ctx)
	go b.automaticBackupLoop(ctx)
	go b.maintenanceLoop(ctx)
	go b.expirationNoticeLoop(ctx)
	go b.expiredAccessLoop(ctx)
	go b.liveRefreshLoop(ctx)
	go b.idleHomeLoop(ctx)
	offset := int64(0)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		updates, err := b.client.GetUpdates(ctx, offset, 25)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(2 * time.Second)
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			_ = b.handleUpdate(ctx, u)
		}
	}
}

func (b *Bot) handleUpdate(ctx context.Context, u Update) error {
	if u.CallbackQuery != nil {
		cq := u.CallbackQuery
		chatID := int64(0)
		msgID := 0
		if cq.Message != nil {
			chatID = cq.Message.Chat.ID
			msgID = cq.Message.MessageID
		}
		actor := b.resolveActor(ctx, cq.From.ID, cq.From.FirstName)
		_ = b.markChatActivity(ctx, chatID, actor.TelegramID)
		_ = b.client.AnswerCallback(ctx, cq.ID, "")
		return b.handleCallback(ctx, actor, chatID, msgID, cq.Data)
	}
	if u.Message == nil {
		return nil
	}
	msg := u.Message
	actor := b.resolveActor(ctx, msg.From.ID, msg.From.FirstName)
	_ = b.markChatActivity(ctx, msg.Chat.ID, actor.TelegramID)
	if msg.Document != nil {
		if handled, err := b.handleStateDocument(ctx, actor, msg); handled || err != nil {
			if handled {
				_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
			}
			return err
		}
	}
	text := strings.TrimSpace(msg.Text)
	if handled, err := b.handleStateMessage(ctx, actor, msg.Chat.ID, msg.MessageID, text); handled || err != nil {
		if handled {
			_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		}
		return err
	}
	switch {
	case text == "/start" || text == "/menu" || text == "menu":
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		return b.showMain(ctx, actor, msg.Chat.ID, 0)
	case isBotCommand(text, "/comandos"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		return b.showCommands(ctx, actor, msg.Chat.ID, 0)
	case isBotCommand(text, "/onlines"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		return b.showOnlinePage(ctx, actor, msg.Chat.ID, 0, 0)
	case isBotCommand(text, "/contas"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		return b.showAccountsListPage(ctx, actor, msg.Chat.ID, 0, 0)
	case isBotCommand(text, "/criar"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		arg := commandArg(text, "/criar")
		if arg != "" {
			return b.startCreateAccountWithUsername(ctx, actor, msg.Chat.ID, 0, false, arg)
		}
		return b.startCreateAccount(ctx, actor, msg.Chat.ID, 0, false)
	case isBotCommand(text, "/teste"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		arg := commandArg(text, "/teste")
		if arg != "" {
			return b.startCreateAccountWithUsername(ctx, actor, msg.Chat.ID, 0, true, arg)
		}
		return b.startCreateAccount(ctx, actor, msg.Chat.ID, 0, true)
	case isBotCommand(text, "/editar"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		arg := commandArg(text, "/editar")
		if arg != "" {
			return b.showAccountPanelDirect(ctx, actor, msg.Chat.ID, 0, arg)
		}
		return b.startAccountLookup(ctx, actor, msg.Chat.ID, 0, "edit")
	case isBotCommand(text, "/remover"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		arg := strings.TrimSpace(commandArg(text, "/remover"))
		if strings.EqualFold(arg, "todos") || strings.EqualFold(arg, "todas") {
			return b.confirmRemoveAllAccounts(ctx, actor, msg.Chat.ID, 0)
		}
		if arg != "" {
			return b.confirmRemoveAccountDirect(ctx, actor, msg.Chat.ID, 0, arg)
		}
		return b.startAccountLookup(ctx, actor, msg.Chat.ID, 0, "remove")
	case isBotCommand(text, "/editarev"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		arg := strings.TrimSpace(commandArg(text, "/editarev"))
		if arg != "" {
			return b.showResellerPanelDirect(ctx, actor, msg.Chat.ID, 0, arg)
		}
		return b.startResellerLookup(ctx, actor, msg.Chat.ID, 0, "edit")
	case isBotCommand(text, "/revenda"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		arg := strings.TrimSpace(commandArg(text, "/revenda"))
		if arg != "" {
			return b.showResellerPanelDirect(ctx, actor, msg.Chat.ID, 0, arg)
		}
		return b.startResellerLookup(ctx, actor, msg.Chat.ID, 0, "edit")
	case isBotCommand(text, "/limpar"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		arg := commandArg(text, "/limpar")
		if arg != "" {
			return b.clearDevicesForUsername(ctx, actor, msg.Chat.ID, 0, arg)
		}
		if actor.IsAdmin || actor.Role == model.RoleAdmin {
			return b.clearDevices(ctx, actor, msg.Chat.ID, 0)
		}
		return b.showDevices(ctx, actor, msg.Chat.ID, 0)
	case isBotCommand(text, "/relatorio"):
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		return b.showPaymentsReportPage(ctx, actor, msg.Chat.ID, 0, 0)
	default:
		_ = b.client.DeleteMessage(ctx, msg.Chat.ID, msg.MessageID)
		return b.showMain(ctx, actor, msg.Chat.ID, 0)
	}
}

func (b *Bot) handleCallback(ctx context.Context, actor model.Actor, chatID int64, msgID int, data string) error {
	switch {
	case data == "menu_home" || data == "back_home":
		_ = b.clearFlow(ctx, actor.TelegramID)
		return b.showMain(ctx, actor, chatID, msgID)
	case data == "menu_accounts":
		_ = b.clearFlow(ctx, actor.TelegramID)
		return b.showAccounts(ctx, actor, chatID, msgID)
	case data == "menu_commands":
		_ = b.clearFlow(ctx, actor.TelegramID)
		return b.showCommands(ctx, actor, chatID, msgID)
	case data == "menu_resellers" || data == "menu_revender":
		return b.showResellers(ctx, actor, chatID, msgID)
	case data == "reseller_list":
		return b.showResellerListPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "resellers_page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "resellers_page:"))
		return b.showResellerListPage(ctx, actor, chatID, msgID, page)
	case data == "reseller_create":
		return b.startCreateReseller(ctx, actor, chatID, msgID)
	case data == "reseller_edit":
		return b.startResellerLookup(ctx, actor, chatID, msgID, "edit")
	case data == "reseller_renew":
		return b.startResellerLookup(ctx, actor, chatID, msgID, "renew")
	case data == "reseller_remove":
		return b.startResellerLookup(ctx, actor, chatID, msgID, "remove")
	case data == "reseller_block":
		return b.showExpiredResellerListPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "resellers_expired_page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "resellers_expired_page:"))
		return b.showExpiredResellerListPage(ctx, actor, chatID, msgID, page)
	case data == "res_wa_yes" || data == "res_wa_no":
		return b.handleCreateResellerWhatsAppChoice(ctx, actor, chatID, msgID, data == "res_wa_yes")
	case strings.HasPrefix(data, "res_view:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_view:"), 10, 64)
		return b.showResellerPanel(ctx, actor, chatID, msgID, id)
	case strings.HasPrefix(data, "res_edit_credits:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_edit_credits:"), 10, 64)
		return b.startResellerEditField(ctx, actor, chatID, msgID, id, "credits")
	case strings.HasPrefix(data, "res_edit_price:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_edit_price:"), 10, 64)
		return b.startResellerEditField(ctx, actor, chatID, msgID, id, "price")
	case strings.HasPrefix(data, "res_edit_wa:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_edit_wa:"), 10, 64)
		return b.startResellerEditField(ctx, actor, chatID, msgID, id, "whatsapp")
	case strings.HasPrefix(data, "res_toggle_active:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_toggle_active:"), 10, 64)
		return b.toggleResellerActive(ctx, actor, chatID, msgID, id)
	case strings.HasPrefix(data, "res_toggle_sub:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_toggle_sub:"), 10, 64)
		return b.toggleResellerSub(ctx, actor, chatID, msgID, id)
	case strings.HasPrefix(data, "res_renew_notice:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_renew_notice:"), 10, 64)
		return b.showResellerRenewalNoticePrompt(ctx, actor, chatID, msgID, id)
	case strings.HasPrefix(data, "res_set_renew_notice:"):
		return b.setResellerRenewalNoticeWatch(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "res_set_renew_notice:"))
	case strings.HasPrefix(data, "res_renew_start:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_renew_start:"), 10, 64)
		return b.showResellerRenewOptions(ctx, actor, chatID, msgID, id)
	case strings.HasPrefix(data, "res_renew_days:"):
		return b.handleResellerRenewDays(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "res_renew_days:"))
	case strings.HasPrefix(data, "res_delete_confirm:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_delete_confirm:"), 10, 64)
		return b.confirmDeleteReseller(ctx, actor, chatID, msgID, id)
	case strings.HasPrefix(data, "res_do_delete:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_do_delete:"), 10, 64)
		return b.doDeleteReseller(ctx, actor, chatID, msgID, id)
	case data == "res_xray_yes" || data == "res_xray_no":
		return b.handleCreateResellerXray(ctx, actor, chatID, msgID, data == "res_xray_yes")
	case data == "res_sub_yes" || data == "res_sub_no":
		return b.finishCreateReseller(ctx, actor, chatID, msgID, data == "res_sub_yes")
	case data == "menu_admin":
		return b.showAdministration(ctx, actor, chatID, msgID)
	case data == "admin_restart":
		return b.restartBot(ctx, actor, chatID, msgID)
	case data == "admin_clear_cache":
		return b.clearSystemCache(ctx, actor, chatID, msgID)
	case data == "settings_gestor_toggle":
		return b.setGestorOnly(ctx, actor, chatID, msgID, !b.currentGestorOnly(ctx))
	case data == "settings_xray_general_toggle":
		return b.setXrayCreateEnabled(ctx, actor, chatID, msgID, !b.currentXrayCreateEnabled(ctx))
	case data == "settings_expiration_notices_toggle":
		return b.setExpirationNoticesEnabled(ctx, actor, chatID, msgID, !b.currentExpirationNoticesEnabled(ctx))
	case data == "menu_servers":
		return b.showServers(ctx, actor, chatID, msgID)
	case data == "servers_add":
		return b.startAddServer(ctx, actor, chatID, msgID)
	case data == "servers_edit" || data == "servers_list":
		return b.showServerEditListPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "servers_edit_page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "servers_edit_page:"))
		return b.showServerEditListPage(ctx, actor, chatID, msgID, page)
	case data == "servers_sync" || data == "servers_principal_sync":
		return b.syncServersNow(ctx, actor, chatID, msgID)
	case data == "servers_restart":
		return b.confirmRestartServers(ctx, actor, chatID, msgID)
	case data == "servers_do_restart":
		return b.restartSecondaryServers(ctx, actor, chatID, msgID)
	case data == "servers_sync_logs":
		return b.showServerSyncLogsPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "servers_sync_logs_page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "servers_sync_logs_page:"))
		return b.showServerSyncLogsPage(ctx, actor, chatID, msgID, page)
	case data == "servers_cloudflare":
		return b.showServers(ctx, actor, chatID, msgID)
	case data == "servers_dns_vps" || data == "servers_dns_custom" || data == "servers_dns_apply_confirm" || data == "servers_dns_apply" || data == "servers_dns_remove_confirm" || data == "servers_dns_remove" || strings.HasPrefix(data, "servers_dns_preset:"):
		_ = b.clearFlow(ctx, actor.TelegramID)
		return b.showServers(ctx, actor, chatID, msgID)
	case strings.HasPrefix(data, "server_view:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "server_view:"), 10, 64)
		return b.showServerPanel(ctx, actor, chatID, msgID, id)
	case strings.HasPrefix(data, "server_edit_ip:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "server_edit_ip:"), 10, 64)
		return b.startEditServerField(ctx, actor, chatID, msgID, id, "ip")
	case strings.HasPrefix(data, "server_edit_token:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "server_edit_token:"), 10, 64)
		return b.startEditServerField(ctx, actor, chatID, msgID, id, "token")
	case strings.HasPrefix(data, "server_remove_confirm:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "server_remove_confirm:"), 10, 64)
		return b.confirmRemoveServer(ctx, actor, chatID, msgID, id)
	case strings.HasPrefix(data, "server_do_remove:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "server_do_remove:"), 10, 64)
		return b.doRemoveServer(ctx, actor, chatID, msgID, id)

	case data == "menu_devices":
		return b.showDevices(ctx, actor, chatID, msgID)
	case data == "devices_clear_all":
		return b.clearDevices(ctx, actor, chatID, msgID)
	case data == "menu_backup":
		return b.showBackup(ctx, actor, chatID, msgID)
	case data == "backup_export":
		return b.exportBackup(ctx, actor, chatID, msgID)
	case data == "backup_import":
		return b.startBackupImport(ctx, actor, chatID, msgID)
	case data == "backup_destination":
		return b.showBackupDestination(ctx, actor, chatID, msgID)
	case data == "backup_dest_same":
		return b.setBackupDestinationSame(ctx, actor, chatID, msgID)
	case data == "backup_dest_other":
		return b.startBackupDestinationOther(ctx, actor, chatID, msgID)
	case data == "backup_auto_menu":
		return b.showBackupAuto(ctx, actor, chatID, msgID)
	case strings.HasPrefix(data, "backup_auto_interval_"):
		return b.setBackupAutoInterval(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "backup_auto_interval_"))
	case data == "backup_auto_disable":
		return b.disableBackupAuto(ctx, actor, chatID, msgID)
	case data == "menu_payments":
		return b.showPayments(ctx, actor, chatID, msgID)
	case data == "payments_config":
		return b.showPaymentsConfig(ctx, actor, chatID, msgID)
	case data == "payments_toggle":
		return b.togglePayments(ctx, actor, chatID, msgID)
	case data == "payments_renewal_notice_toggle":
		return b.togglePaymentRenewalNotice(ctx, actor, chatID, msgID)
	case data == "payments_tutorial":
		return b.showPaymentsTutorialMenu(ctx, actor, chatID, msgID)
	case data == "payments_tutorial_asaas" || data == "payments_tutorial_mercado_pago" || data == "payments_tutorial_infinitepay":
		return b.showPaymentsTutorial(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "payments_tutorial_"))
	case data == "payments_webhook_domain":
		return b.startPaymentWebhookDomain(ctx, actor, chatID, msgID)
	case data == "payments_webhook_status":
		return b.showPaymentWebhookStatus(ctx, actor, chatID, msgID)
	case data == "payments_webhook_test":
		return b.testPaymentWebhook(ctx, actor, chatID, msgID)
	case data == "payments_webhook_events":
		return b.showPaymentWebhookEvents(ctx, actor, chatID, msgID)
	case data == "payments_orders":
		return b.showPaymentOrdersOwnerPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "payments_orders_page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "payments_orders_page:"))
		return b.showPaymentOrdersOwnerPage(ctx, actor, chatID, msgID, page)
	case data == "payments_report":
		return b.showPaymentsReportPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "payments_report_page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "payments_report_page:"))
		return b.showPaymentsReportPage(ctx, actor, chatID, msgID, page)
	case strings.HasPrefix(data, "payments_report_"):
		return b.showPaymentsReportPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "payments_bank_"):
		return b.startPaymentBankConfig(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "payments_bank_"))
	case data == "payment_plan_months":
		return b.startPaymentPlanMonths(ctx, actor, chatID, msgID)
	case data == "payment_limit_menu":
		if !paymentLimitConfigAllowed(actor) {
			return b.showPaymentsConfig(ctx, actor, chatID, msgID)
		}
		return b.showPaymentLimitPackages(ctx, actor, chatID, msgID)
	case data == "payment_limit_create":
		if !paymentLimitConfigAllowed(actor) {
			return b.showPaymentsConfig(ctx, actor, chatID, msgID)
		}
		return b.startPaymentLimitPackage(ctx, actor, chatID, msgID)
	case data == "menu_notices":
		return b.showNotices(ctx, actor, chatID, msgID)
	case data == "notice_aviso" || data == "notice_novidades":
		return b.startNotice(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "notice_"))
	case data == "menu_myreseller":
		return b.showMyReseller(ctx, actor, chatID, msgID)
	case data == "my_reseller_renew":
		return b.showAutoPaymentPlans(ctx, actor, chatID, msgID, "renew")
	case data == "my_reseller_limits":
		return b.showAutoPaymentPlans(ctx, actor, chatID, msgID, "limit")
	case data == "my_reseller_renewal_notice_toggle":
		return b.toggleMyResellerRenewalNotice(ctx, actor, chatID, msgID)
	case data == "my_payment_orders":
		return b.showMyPaymentOrdersPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "my_payment_orders_page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "my_payment_orders_page:"))
		return b.showMyPaymentOrdersPage(ctx, actor, chatID, msgID, page)
	case strings.HasPrefix(data, "pay_renew_month:"):
		months, _ := strconv.Atoi(strings.TrimPrefix(data, "pay_renew_month:"))
		return b.createRenewPaymentOrder(ctx, actor, chatID, msgID, months)
	case strings.HasPrefix(data, "pay_limit_pkg:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "pay_limit_pkg:"), 10, 64)
		return b.createLimitPaymentOrder(ctx, actor, chatID, msgID, id)
	case data == "menu_apps":
		return b.showApps(ctx, actor, chatID, msgID)
	case data == "app_list":
		return b.showAppsPage(ctx, actor, chatID, msgID, 0)
	case data == "app_download" || data == "app_download_latest":
		return b.sendLatestAppDocument(ctx, actor, chatID, msgID)
	case strings.HasPrefix(data, "app_page:"):
		return b.showAppsPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "app_send:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "app_send:"), 10, 64)
		return b.sendAppDocument(ctx, actor, chatID, msgID, id)
	case data == "app_import":
		return b.startAppImport(ctx, actor, chatID, msgID)
	case data == "menu_profile":
		return b.showProfile(ctx, actor, chatID, msgID)
	case data == "profile_name":
		return b.startProfileEdit(ctx, actor, chatID, msgID, "name")
	case data == "profile_whatsapp":
		return b.startProfileEdit(ctx, actor, chatID, msgID, "whatsapp")
	case data == "menu_settings":
		return b.showSettings(ctx, actor, chatID, msgID)
	case data == "settings_gestor_on" || data == "settings_gestor_off":
		return b.setGestorOnly(ctx, actor, chatID, msgID, data == "settings_gestor_on")
	case data == "settings_cf_token":
		return b.startSettingsEdit(ctx, actor, chatID, msgID, "cloudflare_token")
	case data == "settings_vpndns":
		_ = b.clearFlow(ctx, actor.TelegramID)
		return b.showAdministration(ctx, actor, chatID, msgID)
	case data == "menu_online":
		return b.showOnlinePage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "online_page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "online_page:"))
		return b.showOnlinePage(ctx, actor, chatID, msgID, page)
	case data == "menu_status":
		if !actor.IsAdmin {
			return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		return b.showStatus(ctx, chatID, msgID)
	case data == "admin_resources":
		return b.showResourceStatus(ctx, actor, chatID, msgID)
	case data == "admin_rotate_events":
		return b.rotateOldEventsNow(ctx, actor, chatID, msgID)
	case data == "accounts_list":
		return b.showAccountsListPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "accounts_page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "accounts_page:"))
		return b.showAccountsListPage(ctx, actor, chatID, msgID, page)
	case data == "accounts_expired":
		return b.showExpiredListPage(ctx, actor, chatID, msgID, 0)
	case strings.HasPrefix(data, "expired_page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "expired_page:"))
		return b.showExpiredListPage(ctx, actor, chatID, msgID, page)
	case data == "accounts_clear_expired":
		return b.clearExpiredAccounts(ctx, actor, chatID, msgID)
	case data == "accounts_release_days":
		return b.startReleaseDaysAll(ctx, actor, chatID, msgID)
	case data == "accounts_create":
		return b.startCreateAccount(ctx, actor, chatID, msgID, false)
	case data == "accounts_trial":
		return b.startCreateAccount(ctx, actor, chatID, msgID, true)
	case data == "accounts_edit":
		return b.startAccountLookup(ctx, actor, chatID, msgID, "edit")
	case data == "accounts_remove":
		return b.startAccountLookup(ctx, actor, chatID, msgID, "remove")
	case data == "accounts_renew":
		return b.startAccountLookup(ctx, actor, chatID, msgID, "renew")
	case data == "acc_wa_yes" || data == "acc_wa_no":
		return b.handleCreateAccountWhatsAppChoice(ctx, actor, chatID, msgID, data == "acc_wa_yes")
	case strings.HasPrefix(data, "acc_valid_"):
		return b.handleCreateValidity(ctx, actor, chatID, msgID, data)
	case strings.HasPrefix(data, "acc_trial_"):
		return b.handleTrialDuration(ctx, actor, chatID, msgID, data)
	case data == "acc_xray_yes" || data == "acc_xray_no":
		return b.finishCreateAccount(ctx, actor, chatID, msgID, data == "acc_xray_yes")
	case strings.HasPrefix(data, "acct_view:"):
		return b.showAccountPanel(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "acct_view:"))
	case strings.HasPrefix(data, "acct_copy_created:"):
		return b.showAccountCopyCreated(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "acct_copy_created:"))
	case strings.HasPrefix(data, "acct_copy:"):
		return b.showAccountCopy(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "acct_copy:"))
	case strings.HasPrefix(data, "acct_pass:"):
		return b.startEditField(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "acct_pass:"), "password")
	case strings.HasPrefix(data, "acct_limit:"):
		return b.startEditField(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "acct_limit:"), "limit")
	case strings.HasPrefix(data, "acct_renew:"):
		return b.showRenewOptions(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "acct_renew:"))
	case strings.HasPrefix(data, "acct_remove:"):
		return b.confirmRemoveAccount(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "acct_remove:"))
	case strings.HasPrefix(data, "acct_do_renew:"):
		return b.doRenewAccount(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "acct_do_renew:"))
	case strings.HasPrefix(data, "acct_do_remove:"):
		return b.doRemoveAccount(ctx, actor, chatID, msgID, strings.TrimPrefix(data, "acct_do_remove:"))
	case data == "acct_remove_all":
		return b.confirmRemoveAllAccounts(ctx, actor, chatID, msgID)
	case data == "acct_do_remove_all":
		return b.doRemoveAllAccounts(ctx, actor, chatID, msgID)
	case strings.HasPrefix(data, "res_copy_created:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "res_copy_created:"), 10, 64)
		return b.showResellerCopyCreated(ctx, actor, chatID, msgID, id)
	case data == "noop":
		return nil
	default:
		return b.showMain(ctx, actor, chatID, msgID)
	}
}

func (b *Bot) showMain(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	text, err := b.buildMainPanel(ctx, actor)
	if err != nil {
		return err
	}
	return b.sendOrEdit(ctx, chatID, msgID, text, mainKeyboard(actor), "menu")
}

func (b *Bot) showCommands(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.sendOrEdit(ctx, chatID, msgID, commandsPanelText(actor), commandsKeyboard(), "commands")
}

func (b *Bot) showAccounts(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	text, err := b.accountsPanelText(ctx, actor)
	if err != nil {
		return err
	}
	return b.sendOrEdit(ctx, chatID, msgID, text, accountsKeyboard(actor), "live_accounts")
}

func (b *Bot) showResellers(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if actor.Role == model.RoleSubReseller && !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ SubRevenda não pode criar, editar, renovar, suspender ou remover outra SubRevenda.", backKeyboard(), "flow")
	}
	text, err := b.resellersPanelText(ctx, actor)
	if err != nil {
		return err
	}
	kind := "live_resellers_admin"
	if !actor.IsAdmin {
		kind = "live_subresellers"
	}
	return b.sendOrEdit(ctx, chatID, msgID, text, resellersKeyboard(actor), kind)
}

func (b *Bot) showOnline(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showOnlinePage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) onlinePagePayload(ctx context.Context, actor model.Actor, page int) (string, InlineKeyboardMarkup) {
	sum, _ := b.svc.Online.Summary(ctx)
	items := filterOnline(actor, b.visibleOwnerIDs(ctx, actor), sum.Users)
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Username) < strings.ToLower(items[j].Username) })
	page, pages, start, end := paginateBounds(len(items), page, listPageSize)
	var sb strings.Builder
	fmt.Fprintf(&sb, "🟢 Contas Online [%d]\n━━━━━━━━━━━━━━\n", onlineConnectionsTotal(items))
	if len(items) == 0 {
		sb.WriteString("Nenhuma conta online agora.\n")
	} else {
		pageItems := items[start:end]
		for idx, it := range pageItems {
			owner := it.OwnerName
			if owner == "" {
				owner = "Admin"
			}
			fmt.Fprintf(&sb, "👤 <code>%s</code>\n", h(it.Username))
			fmt.Fprintf(&sb, "🟢 %d/%d • 👑 %s\n", it.Connections, nonZero(it.Limit, 1), h(owner))
			sb.WriteString("━━━━━━━━━━━━━━")
			if idx < len(pageItems)-1 {
				sb.WriteString("\n")
			}
		}
	}
	sb.WriteString(pageIndicator(page, pages))
	return sb.String(), pagedListKeyboard("online_page", page, pages, "menu_accounts")
}

func (b *Bot) showOnlinePage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	text, kb := b.onlinePagePayload(ctx, actor, page)
	return b.sendOrEdit(ctx, chatID, msgID, text, kb, fmt.Sprintf("live_online:%d", page))
}
func (b *Bot) showAccountsList(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showAccountsListPage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) accountsListPagePayload(ctx context.Context, actor model.Actor, page int) (string, InlineKeyboardMarkup) {
	accs, _ := b.svc.Store.ListAccounts(ctx, false)
	owners := b.directOwnerIDs(ctx, actor)
	now := time.Now().UTC()
	rows := []model.Account{}
	for _, a := range accs {
		if accountVisible(actor, owners, a) && a.DeletedAt == nil && a.ExpiresAt.After(now) && a.Status != "deleted" {
			rows = append(rows, a)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return strings.ToLower(rows[i].Username) < strings.ToLower(rows[j].Username) })
	page, pages, start, end := paginateBounds(len(rows), page, listPageSize)
	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 Contas [%d]\n━━━━━━━━━━━━━━\n", len(rows))
	if len(rows) == 0 {
		sb.WriteString("Nenhuma conta ativa.\n")
		sb.WriteString("━━━━━━━━━━━━━━")
	} else {
		pageRows := rows[start:end]
		for idx, a := range pageRows {
			owner := a.OwnerName
			if owner == "" {
				owner = "Admin"
			}
			fmt.Fprintf(&sb, "👤 <code>%s</code>\n", h(a.Username))
			fmt.Fprintf(&sb, "📳 %d • ⏳ %s • 👑 %s\n", nonZero(a.LimitConnections, 1), h(daysLeftLong(a.ExpiresAt)), h(owner))
			sb.WriteString("━━━━━━━━━━━━━━")
			if idx < len(pageRows)-1 {
				sb.WriteString("\n")
			}
		}
		if len(pageRows) > 0 {
			sb.WriteString("\nUse <code>/editar</code> usuario para renovar")
		}
	}
	sb.WriteString(pageIndicator(page, pages))
	return sb.String(), pagedListKeyboard("accounts_page", page, pages, "menu_accounts")
}

func (b *Bot) showAccountsListPage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	text, kb := b.accountsListPagePayload(ctx, actor, page)
	return b.sendOrEdit(ctx, chatID, msgID, text, kb, fmt.Sprintf("live_accounts_list:%d", page))
}

func (b *Bot) showExpiredList(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showExpiredListPage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) expiredListPagePayload(ctx context.Context, actor model.Actor, page int) (string, InlineKeyboardMarkup) {
	accs, _ := b.svc.Store.ListAccounts(ctx, false)
	owners := b.directOwnerIDs(ctx, actor)
	now := time.Now().UTC()
	rows := []model.Account{}
	for _, a := range accs {
		if accountVisible(actor, owners, a) && a.DeletedAt == nil && a.Status != "deleted" && (strings.EqualFold(a.Status, "suspended") || !a.ExpiresAt.After(now)) {
			rows = append(rows, a)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ExpiresAt.Before(rows[j].ExpiresAt) })
	page, pages, start, end := paginateBounds(len(rows), page, listPageSize)
	var sb strings.Builder
	fmt.Fprintf(&sb, "🚫 Contas Expiradas [%d]\n━━━━━━━━━━━━━━\n", len(rows))
	if len(rows) == 0 {
		sb.WriteString("Nenhuma conta expirada.\n")
	} else {
		pageRows := rows[start:end]
		for idx, a := range pageRows {
			owner := a.OwnerName
			if owner == "" {
				owner = "Admin"
			}
			fmt.Fprintf(&sb, "👤 <code>%s</code>\n", h(a.Username))
			fmt.Fprintf(&sb, "⌛ %s • 👑 %s\n", h(expiredFor(a.ExpiresAt)), h(owner))
			sb.WriteString("━━━━━━━━━━━━━━")
			if idx < len(pageRows)-1 {
				sb.WriteString("\n")
			}
		}
	}
	sb.WriteString(pageIndicator(page, pages))
	sb.WriteString("\n⚠️ Prazo: até 7 dias. Remove automático!")
	if len(rows) > 0 {
		sb.WriteString("\n\nUse <code>/editar</code> usuario para renovar")
	}
	return sb.String(), expiredListKeyboard(actor, page, pages)
}

func (b *Bot) showExpiredListPage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	text, kb := b.expiredListPagePayload(ctx, actor, page)
	return b.sendOrEdit(ctx, chatID, msgID, text, kb, fmt.Sprintf("live_expired:%d", page))
}

func (b *Bot) quickCreateHint(ctx context.Context, actor model.Actor, chatID int64) error {
	return b.startCreateAccount(ctx, actor, chatID, 0, false)
}

type flowState struct {
	State string            `json:"state"`
	Data  map[string]string `json:"data"`
}

func (b *Bot) startCreateAccountWithUsername(ctx context.Context, actor model.Actor, chatID int64, msgID int, trial bool, rawUsername string) error {
	username := accounts.NormalizeUsername(rawUsername)
	if err := accounts.ValidateUsername(username); err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ "+err.Error()+"\n\nDigite outro usuário.", backKeyboard(), "flow")
	}
	pass := randomDigits(5)
	data := map[string]string{"mode": "normal", "username": username, "password": pass}
	title := "➕ Criar conta"
	if trial {
		data["mode"] = "trial"
		title = "🧪 Criar teste"
	}
	if err := b.setFlow(ctx, actor, chatID, "acc_create_ask_whatsapp", data); err != nil {
		return err
	}
	return b.sendOrEdit(ctx, chatID, msgID, accountAskWhatsAppText(title, username, pass), yesNoAccountWhatsAppKeyboard(), "flow")
}

func (b *Bot) startCreateAccount(ctx context.Context, actor model.Actor, chatID int64, msgID int, trial bool) error {
	state := "acc_create_username"
	mode := "normal"
	text := "➕ <b>Criar conta</b>\n━━━━━━━━━━━━━━\nDigite o nome do usuário."
	if trial {
		mode = "trial"
		text = "🧪 <b>Criar teste</b>\n━━━━━━━━━━━━━━\nDigite o nome do usuário."
	}
	if err := b.setFlow(ctx, actor, chatID, state, map[string]string{"mode": mode}); err != nil {
		return err
	}
	return b.sendOrEdit(ctx, chatID, msgID, text, backKeyboard(), "flow")
}

func (b *Bot) startAccountLookup(ctx context.Context, actor model.Actor, chatID int64, msgID int, action string) error {
	labels := map[string]string{"edit": "✏️ <b>Editar conta</b>", "remove": "🗑️ <b>Remover conta</b>", "renew": "♻️ <b>Renovar conta</b>"}
	if err := b.setFlow(ctx, actor, chatID, "account_lookup", map[string]string{"action": action}); err != nil {
		return err
	}
	return b.sendOrEdit(ctx, chatID, msgID, labels[action]+"\n━━━━━━━━━━━━━━\nDigite o usuário da conta.", backKeyboard(), "flow")
}

func (b *Bot) handleStateMessage(ctx context.Context, actor model.Actor, chatID int64, messageID int, text string) (bool, error) {
	st, err := b.getFlow(ctx, actor.TelegramID)
	if err != nil || st.State == "" {
		return false, err
	}
	if strings.HasPrefix(text, "/") {
		_ = b.clearFlow(ctx, actor.TelegramID)
		return false, nil
	}
	switch st.State {
	case "acc_create_username":
		username := accounts.NormalizeUsername(text)
		if err := accounts.ValidateUsername(username); err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ "+err.Error()+"\n\nDigite outro usuário.", backKeyboard(), "flow")
		}
		pass := randomDigits(5)
		st.Data["username"] = username
		st.Data["password"] = pass
		title := "➕ Criar conta"
		if st.Data["mode"] == "trial" {
			title = "🧪 Criar teste"
		}
		_ = b.setFlow(ctx, actor, chatID, "acc_create_ask_whatsapp", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, accountAskWhatsAppText(title, username, pass), yesNoAccountWhatsAppKeyboard(), "flow")
	case "acc_create_ask_whatsapp":
		choice := yesNoInput(text)
		if choice == "yes" {
			_ = b.setFlow(ctx, actor, chatID, "acc_create_whatsapp", st.Data)
			return true, b.sendOrEdit(ctx, chatID, 0, accountWhatsAppPrompt(), backKeyboard(), "flow")
		}
		if choice == "no" {
			st.Data["client_whatsapp"] = ""
			_ = b.setFlow(ctx, actor, chatID, "acc_create_monthly", st.Data)
			return true, b.sendOrEdit(ctx, chatID, 0, monthlyValuePrompt(), backKeyboard(), "flow")
		}
		return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Responda Sim ou Não.", yesNoAccountWhatsAppKeyboard(), "flow")
	case "acc_create_whatsapp":
		phone := strings.TrimSpace(text)
		if strings.EqualFold(phone, "PULAR") || phone == "0" {
			phone = ""
		} else {
			phone = onlyDigits(phone)
			if len(phone) < 10 {
				return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ WhatsApp inválido. Digite com DDI ou envie 0 para pular.", backKeyboard(), "flow")
			}
		}
		st.Data["client_whatsapp"] = phone
		_ = b.setFlow(ctx, actor, chatID, "acc_create_monthly", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, monthlyValuePrompt(), backKeyboard(), "flow")
	case "acc_create_monthly":
		value, err := parseMoney(text)
		if err != nil || value < 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Valor inválido. Exemplo: 25", backKeyboard(), "flow")
		}
		st.Data["monthly_value"] = fmt.Sprintf("%.2f", value)
		if st.Data["mode"] == "trial" {
			_ = b.setFlow(ctx, actor, chatID, "acc_trial_duration", st.Data)
			return true, b.sendOrEdit(ctx, chatID, 0, accountPreChoiceText("🧪 Criar teste", st.Data, value, "Escolha a duração:"), trialDurationKeyboard(), "flow")
		}
		_ = b.setFlow(ctx, actor, chatID, "acc_create_validity", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, accountPreChoiceText("➕ Criar conta", st.Data, value, "Escolha a validade:"), validityKeyboard(), "flow")
	case "account_lookup":
		acc, ok, msg := b.visibleAccount(ctx, actor, text)
		if !ok {
			return true, b.sendOrEdit(ctx, chatID, 0, msg, backKeyboard(), "flow")
		}
		action := st.Data["action"]
		_ = b.clearFlow(ctx, actor.TelegramID)
		switch action {
		case "edit":
			return true, b.showAccountPanel(ctx, actor, chatID, 0, acc.Username)
		case "remove":
			return true, b.confirmRemoveAccount(ctx, actor, chatID, 0, acc.Username)
		case "renew":
			return true, b.showRenewOptions(ctx, actor, chatID, 0, acc.Username)
		}
	case "account_new_password":
		username := st.Data["username"]
		acc, ok, msg := b.visibleAccount(ctx, actor, username)
		if !ok {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, msg, backKeyboard(), "flow")
		}
		updated, err := b.svc.Accounts.ChangePassword(ctx, actor, acc.Username, text)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao mudar senha: "+err.Error(), accountPanelKeyboard(username), "flow")
		}
		b.autoSyncStateSnapshot()
		return true, b.sendOrEdit(ctx, chatID, 0, accountSuccessText("🔐 Senha alterada", b.accountForDisplay(ctx, *updated)), accountPanelKeyboardCopy(b.accountForDisplay(ctx, *updated)), "flow")
	case "account_new_limit":
		username := st.Data["username"]
		limit, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || limit <= 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Digite um limite válido, exemplo: 1", backKeyboard(), "flow")
		}
		acc, ok, msg := b.visibleAccount(ctx, actor, username)
		if !ok {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, msg, backKeyboard(), "flow")
		}
		updated, err := b.svc.Accounts.ChangeLimit(ctx, actor, acc.Username, limit)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao alterar limite: "+err.Error(), accountPanelKeyboard(username), "flow")
		}
		b.autoSyncStateSnapshot()
		return true, b.sendOrEdit(ctx, chatID, 0, accountSuccessText("📳 Limite alterado", b.accountForDisplay(ctx, *updated)), accountPanelKeyboardCopy(b.accountForDisplay(ctx, *updated)), "flow")
	case "accounts_release_days_state":
		if !actor.IsAdmin && actor.Role != model.RoleAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		days, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || days <= 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Digite uma quantidade válida de dias.\nExemplo: <code>3</code>", backKeyboard(), "flow")
		}
		_ = b.clearFlow(ctx, actor.TelegramID)
		return true, b.applyReleaseDaysAll(ctx, actor, chatID, 0, days)

	case "app_import_name":
		if !actor.IsAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		name := strings.TrimSpace(text)
		if name == "" {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Digite um nome válido para o aplicativo.", backKeyboard(), "flow")
		}
		st.Data["name"] = name
		_ = b.setFlow(ctx, actor, chatID, "app_import_path", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "📱 Aplicativo\n━━━━━━━━━━━━━━\nEnvie o arquivo .apk pelo Telegram ou digite o caminho local no servidor.\nExemplo: /root/PrimeCel.apk", backKeyboard(), "flow")
	case "app_import_path":
		st.Data["path"] = strings.TrimSpace(text)
		_ = b.setFlow(ctx, actor, chatID, "app_import_version", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "📱 Aplicativo\n━━━━━━━━━━━━━━\nDigite a versão do app.\nExemplo: 1.0", backKeyboard(), "flow")
	case "app_import_version":
		mgr := apps.NewManager(b.svc.Config, b.svc.Store)
		app, err := mgr.Import(ctx, apps.ImportOptions{Name: st.Data["name"], SourcePath: st.Data["path"], Version: strings.TrimSpace(text), FileID: st.Data["file_id"], FileUniqueID: st.Data["file_unique_id"], FileName: st.Data["file_name"], MimeType: st.Data["mime_type"]})
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao importar app: "+err.Error(), appsKeyboard(actor), "flow")
		}
		return true, b.sendOrEdit(ctx, chatID, 0, fmt.Sprintf("✅ Aplicativo importado\n━━━━━━━━━━━━━━\n📱 Nome: %s\n🏷️ Versão: %s\n📄 Arquivo: %s", app.Name, app.Version, app.FileName), appsKeyboard(actor), "flow")
	case "profile_name":
		mgr := settings.NewManager(b.svc.Config, b.svc.Store)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err := mgr.SetAdminDisplayName(ctx, text); err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro: "+err.Error(), profileKeyboard(), "flow")
		}
		return true, b.showProfile(ctx, actor, chatID, 0)
	case "profile_whatsapp":
		mgr := settings.NewManager(b.svc.Config, b.svc.Store)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err := mgr.SetWhatsAppAdmins(ctx, text); err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro: "+err.Error(), profileKeyboard(), "flow")
		}
		return true, b.showProfile(ctx, actor, chatID, 0)
	case "settings_cloudflare_token":
		mgr := settings.NewManager(b.svc.Config, b.svc.Store)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err := mgr.SetCloudflareToken(ctx, text); err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro: "+err.Error(), adminKeyboard(b.currentGestorOnly(ctx)), "flow")
		}
		b.svc.Config.CloudflareToken = strings.TrimSpace(text)
		return true, b.showAdministrationNotice(ctx, actor, chatID, 0, "✅ Token Cloudflare salvo.")
	case "settings_vpndns_server", "settings_vpndns_choice", "settings_vpndns_domain":
		_ = b.clearFlow(ctx, actor.TelegramID)
		return true, b.showServers(ctx, actor, chatID, 0)
	case "payment_bank_tag":
		if strings.TrimSpace(text) == "0" {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.showPayments(ctx, actor, chatID, 0)
		}
		// Compatibilidade com fluxos antigos: este estado agora recebe a InfiniteTag/Handle.
		st.Data["token"] = strings.TrimPrefix(strings.TrimSpace(text), "$")
		return true, b.finishPaymentBankConfig(ctx, actor, chatID, "infinitepay", st.Data["token"], st.Data)
	case "payment_bank_token":
		if strings.TrimSpace(text) == "0" {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.showPayments(ctx, actor, chatID, 0)
		}
		token := strings.TrimSpace(text)
		bank := normalizePaymentBank(st.Data["bank"])
		if strings.EqualFold(token, "PULAR") && strings.TrimSpace(st.Data["current_token"]) != "" {
			token = strings.TrimSpace(st.Data["current_token"])
		}
		if bank == "infinitepay" {
			token = strings.TrimPrefix(token, "$")
		}
		if token == "" {
			label := "token/API de produção"
			if bank == "infinitepay" {
				label = "InfiniteTag/Handle"
			}
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Dado vazio. Envie o "+h(label)+" ou digite <code>0</code> para voltar.", backKeyboard(), "flow")
		}
		st.Data["token"] = token
		if bank == "asaas" {
			_ = b.setFlow(ctx, actor, chatID, "payment_bank_asaas_customer", st.Data)
			prompt := "🏦 <b>Asaas:</b>\n━━━━━━━━━━━━━━\n2/2 — Envie o <b>Customer ID</b> ou CPF/CNPJ usado nas cobranças Pix:"
			return true, b.sendOrEdit(ctx, chatID, 0, prompt, backKeyboard(), "flow")
		}
		return true, b.finishPaymentBankConfig(ctx, actor, chatID, bank, token, st.Data)
	case "payment_bank_asaas_customer":
		if strings.TrimSpace(text) == "0" {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.showPayments(ctx, actor, chatID, 0)
		}
		raw := strings.TrimSpace(text)
		if strings.EqualFold(raw, "PULAR") {
			if st.Data["asaas_customer_id"] == "" && st.Data["asaas_customer_cpf_cnpj"] == "" {
				return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Para Asaas, informe um Customer ID ou CPF/CNPJ.\n\nDigite <code>0</code> para voltar.", backKeyboard(), "flow")
			}
		} else if raw != "" {
			delete(st.Data, "asaas_customer_id")
			delete(st.Data, "asaas_customer_cpf_cnpj")
			d := onlyDigits(raw)
			if len(d) >= 11 && len(d) <= 14 {
				st.Data["asaas_customer_cpf_cnpj"] = d
			} else {
				st.Data["asaas_customer_id"] = raw
			}
		} else if st.Data["asaas_customer_id"] == "" && st.Data["asaas_customer_cpf_cnpj"] == "" {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Para Asaas, informe um Customer ID ou CPF/CNPJ.\n\nDigite <code>0</code> para voltar.", backKeyboard(), "flow")
		}
		return true, b.finishPaymentBankConfig(ctx, actor, chatID, "asaas", st.Data["token"], st.Data)
	case "payment_bank_secret":
		if strings.TrimSpace(text) == "0" {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.showPayments(ctx, actor, chatID, 0)
		}
		// Compatibilidade com fluxos antigos: segredo de WebHook não é mais pedido nesta configuração.
		bank := normalizePaymentBank(st.Data["bank"])
		return true, b.finishPaymentBankConfig(ctx, actor, chatID, bank, st.Data["token"], st.Data)
	case "payment_token":
		if strings.TrimSpace(text) == "0" {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.showPayments(ctx, actor, chatID, 0)
		}
		ownerID := paymentOwnerID(actor)
		bank := normalizePaymentBank(st.Data["bank"])
		if bank == "infinitepay" && st.Data["tag"] == "" {
			st.Data["tag"] = strings.TrimPrefix(strings.TrimSpace(text), "$")
			return true, b.finishPaymentBankConfig(ctx, actor, chatID, "infinitepay", st.Data["tag"], st.Data)
		}
		data := map[string]any{}
		if bank == "infinitepay" && st.Data["tag"] != "" {
			data["infinitepay_tag"] = st.Data["tag"]
		}
		bts, _ := json.Marshal(data)
		mgr := payments.NewManager(b.svc.Store)
		_, err := mgr.ConfigureOwner(ctx, payments.OwnerConfigInput{OwnerID: ownerID, Bank: bank, Token: strings.TrimSpace(text), Enabled: true, DataJSON: string(bts)})
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao configurar pagamentos: "+err.Error(), paymentsConfigKeyboard(actor), "flow")
		}
		return true, b.sendOrEdit(ctx, chatID, 0, "✅ Pagamentos configurados com sucesso.\n\n"+paymentAdminText(ctx, b.svc.Store, ownerID), paymentsKeyboard(ownerID, true, paymentActorIsAdmin(actor), b.currentPaymentRenewalNoticeEnabled(ctx, ownerID)), "flow")
	case "payment_webhook_domain":
		if strings.TrimSpace(text) == "0" {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.showPayments(ctx, actor, chatID, 0)
		}
		webhookURL, domain := normalizePaymentWebhookURL(text)
		_ = b.svc.Store.SetSetting(ctx, "payments_webhook_domain", domain)
		_ = b.svc.Store.SetSetting(ctx, "payments_webhook_url", webhookURL)
		_ = b.clearFlow(ctx, actor.TelegramID)
		return true, b.sendOrEdit(ctx, chatID, 0, "✅ WebHook configurado.\n\n"+paymentAdminText(ctx, b.svc.Store, 0), paymentsKeyboard(0, true, paymentActorIsAdmin(actor), b.currentPaymentRenewalNoticeEnabled(ctx, 0)), "flow")
	case "payment_month_1":
		v, err := parseMoney(text)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Valor inválido. Exemplo: 40", backKeyboard(), "flow")
		}
		st.Data["m1"] = fmt.Sprintf("%.2f", v)
		_ = b.setFlow(ctx, actor, chatID, "payment_month_2", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "📅 <b>Meses</b>\n━━━━━━━━━━━━━━\nQual valor deseja colocar no plano de <b>2 meses</b>?\n\nExemplo: <code>80</code>", kb([]Button{{"⬅️ Voltar", "payments_config"}}), "flow")
	case "payment_month_2":
		v, err := parseMoney(text)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Valor inválido. Exemplo: 80", backKeyboard(), "flow")
		}
		st.Data["m2"] = fmt.Sprintf("%.2f", v)
		_ = b.setFlow(ctx, actor, chatID, "payment_month_3", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "📅 <b>Meses</b>\n━━━━━━━━━━━━━━\nQual valor deseja colocar no plano de <b>3 meses</b>?\n\nExemplo: <code>100</code>", kb([]Button{{"⬅️ Voltar", "payments_config"}}), "flow")
	case "payment_month_3":
		v3, err := parseMoney(text)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Valor inválido. Exemplo: 100", backKeyboard(), "flow")
		}
		v1, _ := parseMoney(st.Data["m1"])
		v2, _ := parseMoney(st.Data["m2"])
		ownerID := paymentOwnerID(actor)
		mgr := payments.NewManager(b.svc.Store)
		_, _ = mgr.UpsertPackage(ctx, payments.PackageInput{OwnerID: ownerID, Kind: "renew", Name: "Mensal", Months: 1, Days: 30, Amount: v1, Active: true})
		_, _ = mgr.UpsertPackage(ctx, payments.PackageInput{OwnerID: ownerID, Kind: "renew", Name: "2 meses", Months: 2, Days: 60, Amount: v2, Active: true})
		_, _ = mgr.UpsertPackage(ctx, payments.PackageInput{OwnerID: ownerID, Kind: "renew", Name: "3 meses", Months: 3, Days: 90, Amount: v3, Active: true})
		_ = b.clearFlow(ctx, actor.TelegramID)
		return true, b.sendOrEdit(ctx, chatID, 0, "✅ Planos de meses atualizados.", paymentsConfigKeyboard(actor), "flow")
	case "payment_limit_name":
		name := strings.TrimSpace(text)
		if name == "" {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Nome inválido.", backKeyboard(), "flow")
		}
		st.Data["name"] = name
		_ = b.setFlow(ctx, actor, chatID, "payment_limit_credits", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "Digite a quantidade de limites do pacote:\n\nExemplo: <code>10</code>", kb([]Button{{"⬅️ Voltar", "payment_limit_menu"}}), "flow")
	case "payment_limit_credits":
		credits, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || credits <= 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Quantidade inválida. Exemplo: 10", backKeyboard(), "flow")
		}
		st.Data["credits"] = strconv.Itoa(credits)
		_ = b.setFlow(ctx, actor, chatID, "payment_limit_amount", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "Digite o valor do pacote:\n\nExemplo: <code>50</code>", kb([]Button{{"⬅️ Voltar", "payment_limit_menu"}}), "flow")
	case "payment_limit_amount":
		amount, err := parseMoney(text)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Valor inválido. Exemplo: 50", backKeyboard(), "flow")
		}
		credits, _ := strconv.Atoi(st.Data["credits"])
		ownerID := paymentOwnerID(actor)
		mgr := payments.NewManager(b.svc.Store)
		_, err = mgr.UpsertPackage(ctx, payments.PackageInput{OwnerID: ownerID, Kind: "limit", Name: st.Data["name"], Credits: credits, Amount: amount, Active: true})
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao criar pacote: "+err.Error(), paymentsConfigKeyboard(actor), "flow")
		}
		return true, b.showPaymentLimitPackages(ctx, actor, chatID, 0)
	case "notice_message":
		if !actor.IsAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		kind := st.Data["kind"]
		rep, err := notices.NewManager(b.svc.Store, noticeSender{client: b.client}).Broadcast(ctx, kind, text)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao enviar aviso: "+err.Error(), noticesKeyboard(), "flow")
		}
		return true, b.sendOrEdit(ctx, chatID, 0, fmt.Sprintf("✅ Aviso enviado\n━━━━━━━━━━━━━━\nDestinatários: %d\nEntregues: %d\nFalhas: %d", rep.Targets, rep.Delivered, rep.Failed), noticesKeyboard(), "flow")
	case "backup_import_file", "backup_import_path":
		if !actor.IsAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		path := strings.TrimSpace(text)
		if path == "0" {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.showBackup(ctx, actor, chatID, 0)
		}
		st.Data["file"] = path
		_ = b.setFlow(ctx, actor, chatID, "backup_import_confirm", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Importação limpa vai substituir dados atuais.\nDigite IMPORTAR para confirmar.", backKeyboard(), "flow")
	case "backup_import_confirm":
		return true, b.finishBackupImport(ctx, actor, chatID, 0, strings.TrimSpace(text), st.Data["file"])
	case "backup_dest_other_token":
		if !actor.IsAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		token := strings.TrimSpace(text)
		if token == "0" {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.showBackup(ctx, actor, chatID, 0)
		}
		if !strings.Contains(token, ":") || len(token) < 20 {
			return true, b.sendOrEdit(ctx, chatID, 0, "❌ Token inválido.\n\n🔁 <b>Outro bot</b>\n━━━━━━━━━━━━━━\nEnvie o token do bot:", backKeyboard(), "flow")
		}
		_ = b.svc.Store.SetSetting(ctx, "backup_destination_mode", "other_bot")
		_ = b.svc.Store.SetSetting(ctx, "backup_remote_bot_token", token)
		_ = b.clearFlow(ctx, actor.TelegramID)
		return true, b.showBackup(ctx, actor, chatID, 0)
	case "res_create_id":
		id, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil || id <= 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Digite um ID Telegram válido.", backKeyboard(), "flow")
		}
		if old, _ := b.svc.Store.FindReseller(ctx, id); old != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Já existe uma revenda com esse ID.", backKeyboard(), "flow")
		}
		st.Data["telegram_id"] = strconv.FormatInt(id, 10)
		_ = b.setFlow(ctx, actor, chatID, "res_create_name", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "👥 Criar Revenda\n━━━━━━━━━━━━━━\nDigite o nome da revenda.", backKeyboard(), "flow")
	case "res_create_name":
		name := strings.TrimSpace(text)
		if name == "" {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Nome inválido.", backKeyboard(), "flow")
		}
		st.Data["name"] = name
		_ = b.setFlow(ctx, actor, chatID, "res_create_ask_whatsapp", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, resellerAskWhatsAppText(name), yesNoResellerWhatsAppKeyboard(), "flow")
	case "res_create_ask_whatsapp":
		choice := yesNoInput(text)
		if choice == "yes" {
			_ = b.setFlow(ctx, actor, chatID, "res_create_whatsapp", st.Data)
			return true, b.sendOrEdit(ctx, chatID, 0, resellerWhatsAppPrompt(), backKeyboard(), "flow")
		}
		if choice == "no" {
			st.Data["whatsapp"] = ""
			_ = b.setFlow(ctx, actor, chatID, "res_create_credits", st.Data)
			return true, b.sendOrEdit(ctx, chatID, 0, "📳 Digite o limite de acessos.", backKeyboard(), "flow")
		}
		return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Responda Sim ou Não.", yesNoResellerWhatsAppKeyboard(), "flow")
	case "res_create_whatsapp":
		phone := strings.TrimSpace(text)
		if strings.EqualFold(phone, "PULAR") || phone == "0" {
			phone = ""
		} else {
			phone = onlyDigits(phone)
			if len(phone) < 10 {
				return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ WhatsApp inválido. Digite com DDI ou envie 0 para pular.", backKeyboard(), "flow")
			}
		}
		st.Data["whatsapp"] = phone
		_ = b.setFlow(ctx, actor, chatID, "res_create_credits", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "📳 Digite o limite de acessos.", backKeyboard(), "flow")
	case "res_create_credits":
		credits, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || credits < 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Digite um limite válido.", backKeyboard(), "flow")
		}
		st.Data["credits"] = strconv.Itoa(credits)
		_ = b.setFlow(ctx, actor, chatID, "res_create_validity", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "📆 Digite a validade em dias.\nExemplo: 30", backKeyboard(), "flow")
	case "res_create_validity":
		days, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || days <= 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Digite uma validade válida em dias.", backKeyboard(), "flow")
		}
		st.Data["validity_days"] = strconv.Itoa(days)
		_ = b.setFlow(ctx, actor, chatID, "res_create_monthly", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, monthlyValuePrompt(), backKeyboard(), "flow")
	case "res_create_monthly":
		price, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(text), ",", "."), 64)
		if err != nil || price < 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Digite um valor válido.", backKeyboard(), "flow")
		}
		st.Data["monthly_price"] = fmt.Sprintf("%.2f", price)
		if actor.IsAdmin || actor.Role == model.RoleAdmin {
			_ = b.setFlow(ctx, actor, chatID, "res_create_xray", st.Data)
			return true, b.sendOrEdit(ctx, chatID, 0, "🌐 Permitir Xray para esta revenda?", yesNoResellerXrayKeyboard(), "flow")
		}
		return true, b.createSubResellerNow(ctx, actor, chatID, st.Data)
	case "res_lookup":
		r, ok, msg := b.visibleResellerByQuery(ctx, actor, text)
		if !ok {
			return true, b.sendOrEdit(ctx, chatID, 0, msg, backKeyboard(), "flow")
		}
		action := st.Data["action"]
		_ = b.clearFlow(ctx, actor.TelegramID)
		switch action {
		case "edit":
			return true, b.showResellerPanel(ctx, actor, chatID, 0, r.TelegramID)
		case "renew":
			return true, b.showResellerRenewOptions(ctx, actor, chatID, 0, r.TelegramID)
		case "remove":
			return true, b.confirmDeleteReseller(ctx, actor, chatID, 0, r.TelegramID)
		case "block":
			return true, b.toggleResellerActive(ctx, actor, chatID, 0, r.TelegramID)
		}
	case "res_edit_credits_state":
		id, _ := strconv.ParseInt(st.Data["telegram_id"], 10, 64)
		credits, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || credits < 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Digite um limite válido.", backKeyboard(), "flow")
		}
		r, err := b.svc.Resellers.ChangeCredits(ctx, actor, id, credits)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro: "+err.Error(), backKeyboard(), "flow")
		}
		return true, b.sendOrEdit(ctx, chatID, 0, resellerPanelText(*r, b.svc.Resellers.BlockReason(ctx, r)), resellerPanelKeyboard(actor, *r), "flow")
	case "res_edit_price_state":
		id, _ := strconv.ParseInt(st.Data["telegram_id"], 10, 64)
		price, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(text), ",", "."), 64)
		if err != nil || price < 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Valor inválido.", backKeyboard(), "flow")
		}
		r, err := b.svc.Resellers.ChangeMonthlyPrice(ctx, actor, id, price)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro: "+err.Error(), backKeyboard(), "flow")
		}
		return true, b.sendOrEdit(ctx, chatID, 0, resellerPanelText(*r, b.svc.Resellers.BlockReason(ctx, r)), resellerPanelKeyboard(actor, *r), "flow")
	case "res_edit_whatsapp_state":
		id, _ := strconv.ParseInt(st.Data["telegram_id"], 10, 64)
		r, err := b.svc.Resellers.ChangeWhatsApp(ctx, actor, id, text)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro: "+err.Error(), backKeyboard(), "flow")
		}
		return true, b.sendOrEdit(ctx, chatID, 0, resellerPanelText(*r, b.svc.Resellers.BlockReason(ctx, r)), resellerPanelKeyboard(actor, *r), "flow")
	case "res_renew_credits":
		id, _ := strconv.ParseInt(st.Data["telegram_id"], 10, 64)
		days, _ := strconv.Atoi(st.Data["days"])
		credits, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || credits < 0 {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Limite inválido.", backKeyboard(), "flow")
		}
		r, err := b.svc.Resellers.Renew(ctx, actor, id, days, credits, nil)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao renovar: "+err.Error(), backKeyboard(), "flow")
		}
		return true, b.sendOrEdit(ctx, chatID, 0, "✅ Revenda renovada\n━━━━━━━━━━━━━━\n"+resellerMiniLine(*r), resellerPanelKeyboard(actor, *r), "flow")

	case "server_add_host":
		if !actor.IsAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		host := strings.TrimSpace(text)
		if host == "" || strings.ContainsAny(host, " /\n\t") {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Digite um IP/host válido.", backKeyboard(), "flow")
		}
		st.Data["host"] = host
		_ = b.setFlow(ctx, actor, chatID, "server_add_token", st.Data)
		return true, b.sendOrEdit(ctx, chatID, 0, "🌐 Adicionar Servidor\n━━━━━━━━━━━━━━\nDigite a senha root/SSH da VPS secundária.", backKeyboard(), "flow")
	case "server_add_token":
		if !actor.IsAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		sshPassword := strings.TrimSpace(text)
		if sshPassword == "0" || sshPassword == "-" {
			sshPassword = ""
		}
		servers, _ := b.svc.Store.ListServers(ctx)
		srv := model.Server{Name: fmt.Sprintf("Sv%d", len(servers)+1), Host: st.Data["host"], SSHPort: 22, SSHUser: "root", SSHPassword: sshPassword, AgentPort: nonZero(b.svc.Config.RemoteAgentPort, 8787), AgentToken: b.svc.Config.RemoteAgentToken, Enabled: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		_ = b.clearFlow(ctx, actor.TelegramID)
		progressText := fmt.Sprintf("🔄 Aguarde..\n━━━━━━━━━━━━━━\n⏳ %s • %s • sincronizando\n━━━━━━━━━━━━━━", srv.Name, displayHostPlain(srv.Host))
		_ = b.sendOrEdit(ctx, chatID, 0, progressText, InlineKeyboardMarkup{}, "flow")

		err := b.svc.Store.UpsertServer(ctx, srv)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao salvar servidor: "+err.Error(), serversKeyboard(), "flow")
		}
		// Cloudflare padrão recebe o IP novo quando a VPS entra no bot.
		b.addServerToDefaultCloudflare(ctx, srv.Host)
		mgr := remotesync.NewManager(b.svc.Config, b.svc.Store)
		_ = mgr.BootstrapAgent(ctx, srv)
		_, _ = mgr.SyncStateSnapshot(ctx)
		b.ensureDefaultCloudflareForActiveServers(ctx)
		return true, b.showServers(ctx, actor, chatID, 0)
	case "server_edit_ip_state":
		if !actor.IsAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		id, _ := strconv.ParseInt(st.Data["server_id"], 10, 64)
		srv, err := b.svc.Store.FindServer(ctx, id)
		if err != nil || srv == nil {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Servidor não encontrado.", serversKeyboard(), "flow")
		}
		host := strings.TrimSpace(text)
		if host == "" || strings.ContainsAny(host, " /\n\t") {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Digite um IP/host válido.", backKeyboard(), "flow")
		}
		oldHost := strings.TrimSpace(srv.Host)
		srv.Host = host
		err = b.svc.Store.UpsertServer(ctx, *srv)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao salvar servidor: "+err.Error(), serversKeyboard(), "flow")
		}
		if !strings.EqualFold(oldHost, host) {
			// Alterar IP é tratado como adicionar novo IP e remover o IP antigo
			// somente se ele não pertencer mais a nenhum servidor ativo do bot.
			b.addServerToDefaultCloudflare(ctx, host)
			b.removeDeletedServerFromDefaultCloudflare(ctx, oldHost)
		}
		return true, b.showServerPanel(ctx, actor, chatID, 0, id)
	case "server_edit_password_state", "server_edit_token_state":
		if !actor.IsAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		id, _ := strconv.ParseInt(st.Data["server_id"], 10, 64)
		srv, err := b.svc.Store.FindServer(ctx, id)
		if err != nil || srv == nil {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Servidor não encontrado.", serversKeyboard(), "flow")
		}
		sshPassword := strings.TrimSpace(text)
		if sshPassword == "0" || sshPassword == "-" {
			sshPassword = ""
		}
		srv.SSHPassword = sshPassword
		err = b.svc.Store.UpsertServer(ctx, *srv)
		_ = b.clearFlow(ctx, actor.TelegramID)
		if err != nil {
			return true, b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao salvar servidor: "+err.Error(), serversKeyboard(), "flow")
		}
		mgr := remotesync.NewManager(b.svc.Config, b.svc.Store)
		if bootErr := mgr.BootstrapAgent(ctx, *srv); bootErr != nil {
			text := fmt.Sprintf("🔐 Senha salva\n━━━━━━━━━━━━━━\n⚠️ Agente remoto não iniciou.\n%s\n\nConfira a senha root/SSH e tente sincronizar novamente.", serverBriefError(bootErr))
			return true, b.sendOrEdit(ctx, chatID, 0, text, serverPanelKeyboard(*srv), "flow")
		}
		return true, b.showServerPanel(ctx, actor, chatID, 0, id)

	}
	return false, nil
}

func (b *Bot) handleStateDocument(ctx context.Context, actor model.Actor, msg *Message) (bool, error) {
	st, err := b.getFlow(ctx, actor.TelegramID)
	if err != nil {
		return false, nil
	}
	doc := msg.Document
	if doc == nil {
		return false, nil
	}
	switch st.State {
	case "backup_import_file":
		if !actor.IsAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, msg.Chat.ID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		name := strings.TrimSpace(doc.FileName)
		if name == "" {
			name = "backup-painel.tar.gz"
		}
		low := strings.ToLower(name)
		if low != "backup-painel.tar.gz" && !strings.HasSuffix(low, ".tar.gz") && !strings.HasSuffix(low, ".tgz") {
			return true, b.sendOrEdit(ctx, msg.Chat.ID, 0, "❌ O arquivo precisa ser .tar.gz. Nome recomendado: <code>backup-painel.tar.gz</code>.", backKeyboard(), "flow")
		}
		dir := filepath.Join(b.svc.Config.DataDir, "tmp", "backup_imports")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return true, b.sendOrEdit(ctx, msg.Chat.ID, 0, "⚠️ Erro ao preparar importação: "+err.Error(), backupKeyboard(), "flow")
		}
		dest := filepath.Join(dir, fmt.Sprintf("%d_%s", time.Now().UnixNano(), filepath.Base(name)))
		if err := b.client.DownloadFile(ctx, doc.FileID, dest); err != nil {
			return true, b.sendOrEdit(ctx, msg.Chat.ID, 0, "⚠️ Erro ao baixar backup do Telegram: "+err.Error(), backupKeyboard(), "flow")
		}
		st.Data["file"] = dest
		_ = b.setFlow(ctx, actor, msg.Chat.ID, "backup_import_confirm", st.Data)
		return true, b.sendOrEdit(ctx, msg.Chat.ID, 0, "⚠️ Importação limpa vai substituir dados atuais.\nDigite IMPORTAR para confirmar.", backKeyboard(), "flow")
	case "app_import_path":
		if !actor.IsAdmin {
			_ = b.clearFlow(ctx, actor.TelegramID)
			return true, b.sendOrEdit(ctx, msg.Chat.ID, 0, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
		}
		if strings.ToLower(filepath.Ext(doc.FileName)) != ".apk" {
			return true, b.sendOrEdit(ctx, msg.Chat.ID, 0, "⚠️ Envie um arquivo .apk válido.", backKeyboard(), "flow")
		}
		dir := filepath.Join(b.svc.Config.DataDir, "tmp", "telegram_uploads")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return true, b.sendOrEdit(ctx, msg.Chat.ID, 0, "⚠️ Erro ao preparar upload: "+err.Error(), appsKeyboard(actor), "flow")
		}
		dest := filepath.Join(dir, fmt.Sprintf("%d_%s", time.Now().UnixNano(), filepath.Base(doc.FileName)))
		if err := b.client.DownloadFile(ctx, doc.FileID, dest); err != nil {
			return true, b.sendOrEdit(ctx, msg.Chat.ID, 0, "⚠️ Erro ao baixar APK do Telegram: "+err.Error(), appsKeyboard(actor), "flow")
		}
		st.Data["path"] = dest
		st.Data["file_id"] = doc.FileID
		st.Data["file_unique_id"] = doc.FileUniqueID
		st.Data["file_name"] = doc.FileName
		st.Data["mime_type"] = nonEmpty(doc.MimeType, "application/vnd.android.package-archive")
		_ = b.setFlow(ctx, actor, msg.Chat.ID, "app_import_version", st.Data)
		return true, b.sendOrEdit(ctx, msg.Chat.ID, 0, "📱 Aplicativo\n━━━━━━━━━━━━━━\nAPK recebido. Digite a versão do app.\nExemplo: 1.0", backKeyboard(), "flow")
	default:
		return false, nil
	}
}

func (b *Bot) handleCreateValidity(ctx context.Context, actor model.Actor, chatID int64, msgID int, data string) error {
	st, err := b.getFlow(ctx, actor.TelegramID)
	if err != nil {
		return err
	}
	if st.State != "acc_create_validity" {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Fluxo expirado. Comece novamente.", accountsKeyboard(actor), "flow")
	}
	days := map[string]int{"acc_valid_30": 30, "acc_valid_60": 60, "acc_valid_90": 90}[data]
	if days == 0 {
		days = 30
	}
	st.Data["days"] = strconv.Itoa(days)
	_ = b.setFlow(ctx, actor, chatID, "acc_create_xray", st.Data)
	return b.finishCreateAccount(ctx, actor, chatID, msgID, b.actorMayAskXray(ctx, actor))
}

func (b *Bot) handleTrialDuration(ctx context.Context, actor model.Actor, chatID int64, msgID int, data string) error {
	st, err := b.getFlow(ctx, actor.TelegramID)
	if err != nil {
		return err
	}
	if st.State != "acc_trial_duration" {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Fluxo expirado. Comece novamente.", accountsKeyboard(actor), "flow")
	}
	hours := map[string]int{"acc_trial_2": 2, "acc_trial_4": 4, "acc_trial_8": 8}[data]
	if hours == 0 {
		hours = 2
	}
	st.Data["hours"] = strconv.Itoa(hours)
	_ = b.setFlow(ctx, actor, chatID, "acc_create_xray", st.Data)
	return b.finishCreateAccount(ctx, actor, chatID, msgID, b.actorMayAskXray(ctx, actor))
}

func (b *Bot) handleCreateAccountWhatsAppChoice(ctx context.Context, actor model.Actor, chatID int64, msgID int, yes bool) error {
	st, err := b.getFlow(ctx, actor.TelegramID)
	if err != nil || st.State != "acc_create_ask_whatsapp" {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Fluxo expirado. Comece novamente.", accountsKeyboard(actor), "flow")
	}
	if yes {
		_ = b.setFlow(ctx, actor, chatID, "acc_create_whatsapp", st.Data)
		return b.sendOrEdit(ctx, chatID, msgID, accountWhatsAppPrompt(), backKeyboard(), "flow")
	}
	st.Data["client_whatsapp"] = ""
	_ = b.setFlow(ctx, actor, chatID, "acc_create_monthly", st.Data)
	return b.sendOrEdit(ctx, chatID, msgID, monthlyValuePrompt(), backKeyboard(), "flow")
}

func (b *Bot) handleCreateResellerWhatsAppChoice(ctx context.Context, actor model.Actor, chatID int64, msgID int, yes bool) error {
	st, err := b.getFlow(ctx, actor.TelegramID)
	if err != nil || st.State != "res_create_ask_whatsapp" {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Fluxo expirado. Comece novamente.", resellersKeyboard(actor), "flow")
	}
	if yes {
		_ = b.setFlow(ctx, actor, chatID, "res_create_whatsapp", st.Data)
		return b.sendOrEdit(ctx, chatID, msgID, resellerWhatsAppPrompt(), backKeyboard(), "flow")
	}
	st.Data["whatsapp"] = ""
	_ = b.setFlow(ctx, actor, chatID, "res_create_credits", st.Data)
	return b.sendOrEdit(ctx, chatID, msgID, "📳 Digite o limite de acessos.", backKeyboard(), "flow")
}

func (b *Bot) finishCreateAccount(ctx context.Context, actor model.Actor, chatID int64, msgID int, useXray bool) error {
	st, err := b.getFlow(ctx, actor.TelegramID)
	if err != nil {
		return err
	}
	if st.State != "acc_create_xray" {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Fluxo expirado. Comece novamente.", accountsKeyboard(actor), "flow")
	}
	days, _ := strconv.Atoi(st.Data["days"])
	hours, _ := strconv.Atoi(st.Data["hours"])
	uuid := randomUUID()
	monthlyValue, _ := strconv.ParseFloat(st.Data["monthly_value"], 64)
	draft := accounts.CreateDraft{Username: st.Data["username"], Password: st.Data["password"], UUID: uuid, XrayEnabled: useXray, Days: days, TrialHours: hours, Limit: 1, IsTrial: st.Data["mode"] == "trial", ClientWhatsApp: st.Data["client_whatsapp"], MonthlyValue: monthlyValue}
	acc, err := b.svc.Accounts.Create(ctx, actor, draft)
	_ = b.clearFlow(ctx, actor.TelegramID)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao criar conta: "+err.Error(), accountsKeyboard(actor), "flow")
	}
	b.autoSyncStateSnapshot()
	title := "✅ Conta criada"
	if acc.IsTrial {
		title = "✅ Teste criado"
	}
	return b.sendOrEdit(ctx, chatID, msgID, accountSuccessText(title, b.accountForDisplay(ctx, *acc)), createdAccountKeyboardCopy(b.accountForDisplay(ctx, *acc), title), "flow")
}

func (b *Bot) startEditField(ctx context.Context, actor model.Actor, chatID int64, msgID int, username, field string) error {
	acc, ok, msg := b.visibleAccount(ctx, actor, username)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	state := "account_new_password"
	prompt := "Digite a nova senha."
	if field == "limit" {
		state = "account_new_limit"
		prompt = "Digite o novo limite de aparelhos/conexões."
	}
	_ = b.setFlow(ctx, actor, chatID, state, map[string]string{"username": acc.Username})
	return b.sendOrEdit(ctx, chatID, msgID, "✏️ "+acc.Username+"\n━━━━━━━━━━━━━━\n"+prompt, backKeyboard(), "flow")
}

func (b *Bot) showAccountPanelDirect(ctx context.Context, actor model.Actor, chatID int64, msgID int, rawUsername string) error {
	acc, ok, msg := b.visibleAccount(ctx, actor, rawUsername)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	return b.showAccountPanel(ctx, actor, chatID, msgID, acc.Username)
}

func (b *Bot) clearDevicesForUsername(ctx context.Context, actor model.Actor, chatID int64, msgID int, rawUsername string) error {
	acc, ok, msg := b.visibleAccount(ctx, actor, rawUsername)
	if !ok {
		return b.sendDeviceCleanupResult(ctx, actor, chatID, msgID, msg)
	}
	if err := b.clearDeviceRegistrationsForUsers(ctx, []string{acc.Username}, false); err != nil {
		return b.sendDeviceCleanupResult(ctx, actor, chatID, msgID, "⚠️ Erro ao limpar aparelhos: "+err.Error())
	}
	return b.sendDeviceCleanupResult(ctx, actor, chatID, msgID, fmt.Sprintf("✅ Aparelhos de %s limpos.", h(acc.Username)))
}

func (b *Bot) confirmRemoveAccountDirect(ctx context.Context, actor model.Actor, chatID int64, msgID int, rawUsername string) error {
	acc, ok, msg := b.visibleAccount(ctx, actor, rawUsername)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	return b.confirmRemoveAccount(ctx, actor, chatID, msgID, acc.Username)
}

func (b *Bot) startReleaseDaysAll(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin && actor.Role != model.RoleAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	if err := b.setFlow(ctx, actor, chatID, "accounts_release_days_state", map[string]string{}); err != nil {
		return err
	}
	text := "⏳ Liberar Dias\n━━━━━━━━━━━━━━\nDigite quantos dias deseja adicionar para todos os clientes ativos.\n\nInclui contas normais, revendas e subrevendas.\nNão altera contas expiradas nem contas teste.\n\nExemplo: <code>3</code>"
	return b.sendOrEdit(ctx, chatID, msgID, text, backKeyboard(), "flow")
}

func (b *Bot) applyReleaseDaysAll(ctx context.Context, actor model.Actor, chatID int64, msgID int, days int) error {
	if !actor.IsAdmin && actor.Role != model.RoleAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	if days <= 0 {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Dias inválidos.", accountsKeyboard(actor), "flow")
	}
	_ = b.sendOrEdit(ctx, chatID, msgID, fmt.Sprintf("⏳ Liberando +%d dias...", days), accountsKeyboard(actor), "flow")
	now := time.Now().UTC()
	accs, err := b.svc.Store.ListAccounts(ctx, false)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao listar contas: "+err.Error(), accountsKeyboard(actor), "flow")
	}
	accountsOK := 0
	accountFails := 0
	skippedTrials := 0
	skippedExpiredAccounts := 0
	for _, a := range accs {
		if a.DeletedAt != nil || a.Status == "deleted" {
			continue
		}
		if a.IsTrial {
			skippedTrials++
			continue
		}
		if !a.ExpiresAt.After(now) {
			skippedExpiredAccounts++
			continue
		}
		if _, err := b.svc.Accounts.Renew(ctx, actor, a.Username, days); err != nil {
			accountFails++
			continue
		}
		accountsOK++
	}

	resOK := 0
	subOK := 0
	resFails := 0
	skippedExpiredResellers := 0
	if b.svc.Resellers != nil {
		rs, err := b.svc.Store.ListResellers(ctx)
		if err != nil {
			return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao listar revendas: "+err.Error(), accountsKeyboard(actor), "flow")
		}
		for _, r := range rs {
			if r.DeletedAt != nil || !r.Active || !r.ExpiresAt.After(now) {
				skippedExpiredResellers++
				continue
			}
			if _, err := b.svc.Resellers.Renew(ctx, actor, r.TelegramID, days, 0, nil); err != nil {
				resFails++
				continue
			}
			if r.Level > 0 || r.ParentTelegramID != 0 {
				subOK++
			} else {
				resOK++
			}
		}
	}
	if accountsOK > 0 {
		b.autoSyncStateSnapshot()
	}
	text := fmt.Sprintf("✅ Liberação aplicada\n━━━━━━━━━━━━━━\n⏳ Dias adicionados: <code>%d</code>\n👤 Contas: <code>%d</code>\n👥 Revendas: <code>%d</code>\n👥 SubRevendas: <code>%d</code>\n━━━━━━━━━━━━━━\nIgnorados:\n🧪 Testes: <code>%d</code>\n🚫 Contas expiradas: <code>%d</code>\n🚫 Revendas/Sub expiradas: <code>%d</code>", days, accountsOK, resOK, subOK, skippedTrials, skippedExpiredAccounts, skippedExpiredResellers)
	if accountFails+resFails > 0 {
		text += fmt.Sprintf("\n━━━━━━━━━━━━━━\n⚠️ Falhas:\n👤 Contas: <code>%d</code>\n👥 Revendas/Sub: <code>%d</code>", accountFails, resFails)
	}
	return b.sendOrEdit(ctx, chatID, msgID, text, accountsKeyboard(actor), "flow")
}

func (b *Bot) confirmRemoveAllAccounts(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin && actor.Role != model.RoleAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	accs, _ := b.svc.Store.ListAccounts(ctx, false)
	count := 0
	for _, a := range accs {
		if a.DeletedAt == nil && a.Status != "deleted" {
			count++
		}
	}
	if count == 0 {
		return b.sendOrEdit(ctx, chatID, msgID, "🗑️ Remover Todos\n━━━━━━━━━━━━━━\nNenhuma conta ativa encontrada.", accountsKeyboard(actor), "flow")
	}
	text := fmt.Sprintf("🗑️ Remover Todos\n━━━━━━━━━━━━━━\nContas ativas: %d\n\nConfirma remover todas as contas?", count)
	return b.sendOrEdit(ctx, chatID, msgID, text, removeAllConfirmKeyboard(), "flow")
}

func (b *Bot) doRemoveAllAccounts(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin && actor.Role != model.RoleAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	accs, err := b.svc.Store.ListAccounts(ctx, false)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao listar contas: "+err.Error(), accountsKeyboard(actor), "flow")
	}
	removed := 0
	failed := 0
	for _, a := range accs {
		if a.DeletedAt != nil || a.Status == "deleted" {
			continue
		}
		if err := b.svc.Accounts.Remove(ctx, actor, a.Username); err != nil {
			failed++
			continue
		}
		removed++
	}
	if removed > 0 {
		b.autoSyncStateSnapshot()
	}
	text := fmt.Sprintf("🗑️ Remover Todos\n━━━━━━━━━━━━━━\nContas removidas: %d", removed)
	if failed > 0 {
		text += fmt.Sprintf("\nFalhas: %d", failed)
	}
	return b.sendOrEdit(ctx, chatID, msgID, text, accountsKeyboard(actor), "flow")
}

func (b *Bot) showAccountPanel(ctx context.Context, actor model.Actor, chatID int64, msgID int, username string) error {
	acc, ok, msg := b.visibleAccount(ctx, actor, username)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, accountPanelText(*acc), accountPanelKeyboardCopy(*acc), "flow")
}

func (b *Bot) showAccountCopy(ctx context.Context, actor model.Actor, chatID int64, msgID int, username string) error {
	acc, ok, msg := b.visibleAccount(ctx, actor, username)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	text := accountCopyText(*acc)
	return b.sendOrEdit(ctx, chatID, msgID, text, accountPanelKeyboard(acc.Username), "flow")
}

func (b *Bot) showAccountCopyCreated(ctx context.Context, actor model.Actor, chatID int64, msgID int, username string) error {
	acc, ok, msg := b.visibleAccount(ctx, actor, username)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	text := accountCopyText(*acc)
	return b.sendOrEdit(ctx, chatID, msgID, text, createdAccountKeyboard(acc.Username), "flow")
}

func (b *Bot) showRenewOptions(ctx context.Context, actor model.Actor, chatID int64, msgID int, username string) error {
	acc, ok, msg := b.visibleAccount(ctx, actor, username)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, fmt.Sprintf("♻️ Renovar conta\n━━━━━━━━━━━━━━\n👤 Usuário: %s\n📆 Atual: %s\n\nEscolha a renovação:", acc.Username, daysLeft(acc.ExpiresAt)), renewKeyboard(acc.Username), "flow")
}

func (b *Bot) confirmRemoveAccount(ctx context.Context, actor model.Actor, chatID int64, msgID int, username string) error {
	acc, ok, msg := b.visibleAccount(ctx, actor, username)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, fmt.Sprintf("🗑️ Remover conta\n━━━━━━━━━━━━━━\n👤 Usuário: %s\n\nConfirma remover esta conta?", acc.Username), removeConfirmKeyboard(acc.Username), "flow")
}

func (b *Bot) doRenewAccount(ctx context.Context, actor model.Actor, chatID int64, msgID int, payload string) error {
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Renovação inválida.", accountsKeyboard(actor), "flow")
	}
	username := parts[0]
	days, _ := strconv.Atoi(parts[1])
	acc, ok, msg := b.visibleAccount(ctx, actor, username)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	updated, err := b.svc.Accounts.Renew(ctx, actor, acc.Username, days)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao renovar: "+err.Error(), accountPanelKeyboard(acc.Username), "flow")
	}
	b.autoSyncStateSnapshot()
	return b.sendOrEdit(ctx, chatID, msgID, accountSuccessText("✅ Conta renovada", b.accountForDisplay(ctx, *updated)), accountPanelKeyboardCopy(b.accountForDisplay(ctx, *updated)), "flow")
}

func (b *Bot) doRemoveAccount(ctx context.Context, actor model.Actor, chatID int64, msgID int, username string) error {
	acc, ok, msg := b.visibleAccount(ctx, actor, username)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	if err := b.svc.Accounts.Remove(ctx, actor, acc.Username); err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao remover: "+err.Error(), accountPanelKeyboard(acc.Username), "flow")
	}
	b.autoSyncRemovedAccount(acc.Username)
	return b.sendOrEdit(ctx, chatID, msgID, "✅ Conta removida\n━━━━━━━━━━━━━━\n👤 Usuário: "+acc.Username, accountsKeyboard(actor), "flow")
}

func (b *Bot) visibleAccount(ctx context.Context, actor model.Actor, username string) (*model.Account, bool, string) {
	acc, err := b.svc.Store.FindAccount(ctx, username)
	if err != nil || acc == nil {
		return nil, false, "⚠️ Conta não encontrada."
	}
	if updated, err := b.svc.Accounts.ActivateAccountXrayIfEligible(ctx, *acc, b.currentXrayCreateEnabled(ctx)); err == nil && updated != nil {
		acc = updated
	}
	displayAcc := b.accountForDisplay(ctx, *acc)
	acc = &displayAcc
	if !accountVisible(actor, b.visibleOwnerIDs(ctx, actor), *acc) {
		return nil, false, "⛔ Você não tem permissão para essa conta."
	}
	return acc, true, ""
}

func (b *Bot) actorMayAskXray(ctx context.Context, actor model.Actor) bool {
	if !b.currentXrayCreateEnabled(ctx) {
		return false
	}
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return true
	}
	if r, _ := b.svc.Store.FindReseller(ctx, actor.TelegramID); r != nil {
		return r.AllowXray
	}
	return false
}

func (b *Bot) setFlow(ctx context.Context, actor model.Actor, chatID int64, state string, data map[string]string) error {
	if data == nil {
		data = map[string]string{}
	}
	bts, _ := json.Marshal(data)
	now := time.Now().UTC().Format(time.RFC3339)
	return b.svc.Store.Exec(ctx, `INSERT INTO telegram_user_state(user_id,chat_id,state,data_json,last_activity_at,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET chat_id=excluded.chat_id,state=excluded.state,data_json=excluded.data_json,last_activity_at=excluded.last_activity_at,updated_at=excluded.updated_at`, actor.TelegramID, chatID, state, string(bts), now, now)
}
func (b *Bot) getFlow(ctx context.Context, userID int64) (flowState, error) {
	rows, err := b.svc.Store.Query(ctx, `SELECT state,data_json FROM telegram_user_state WHERE user_id=? LIMIT 1`, userID)
	if err != nil || len(rows) == 0 {
		return flowState{Data: map[string]string{}}, err
	}
	fs := flowState{State: rows[0]["state"], Data: map[string]string{}}
	_ = json.Unmarshal([]byte(rows[0]["data_json"]), &fs.Data)
	if fs.Data == nil {
		fs.Data = map[string]string{}
	}
	return fs, nil
}
func (b *Bot) clearFlow(ctx context.Context, userID int64) error {
	return b.svc.Store.Exec(ctx, `DELETE FROM telegram_user_state WHERE user_id=?`, userID)
}

func (b *Bot) hasActiveFlow(ctx context.Context, userID int64) bool {
	if b == nil || b.svc.Store == nil || userID == 0 {
		return false
	}
	rows, err := b.svc.Store.Query(ctx, `SELECT state FROM telegram_user_state WHERE user_id=? LIMIT 1`, userID)
	return err == nil && len(rows) > 0 && strings.TrimSpace(rows[0]["state"]) != ""
}

func (b *Bot) shouldSkipIdleHome(ctx context.Context, userID int64, kind, text string) bool {
	if b.hasActiveFlow(ctx, userID) {
		return true
	}
	lowKind := strings.ToLower(strings.TrimSpace(kind))
	lowText := strings.ToLower(strings.TrimSpace(text))
	if strings.Contains(lowKind, "payment") || strings.Contains(lowKind, "pix") {
		return true
	}
	protectedTerms := []string{
		"sincronizando",
		"gerando backup",
		"importando backup",
		"pedido pix",
		"pix copia e cola",
		"webhook pix",
		"banco pix",
		"pagamento automático via pix",
		"pagamento automatico via pix",
		"━━━━━━pagamento━━━━━━",
		"meus pedidos",
		"pedido de pagamento",
	}
	for _, term := range protectedTerms {
		if strings.Contains(lowText, term) {
			return true
		}
	}
	return false
}

func validityKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"1 mês", "acc_valid_30"}, {"2 meses", "acc_valid_60"}, {"3 meses", "acc_valid_90"}}, []Button{{"⬅️ Voltar", "menu_accounts"}})
}
func trialDurationKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"2h", "acc_trial_2"}, {"4h", "acc_trial_4"}, {"8h", "acc_trial_8"}}, []Button{{"⬅️ Voltar", "menu_accounts"}})
}
func yesNoAccountWhatsAppKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"✅ Sim", "acc_wa_yes"}, {"❌ Não", "acc_wa_no"}}, []Button{{"⬅️ Voltar", "menu_accounts"}})
}
func yesNoXrayKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"✅ Sim", "acc_xray_yes"}, {"❌ Não", "acc_xray_no"}}, []Button{{"⬅️ Voltar", "menu_accounts"}})
}
func accountAskWhatsAppText(title, username, pass string) string {
	return fmt.Sprintf("%s\n━━━━━━━━━━━━━━\n👤 Usuário: <code>%s</code>\n🔒 Senha: <code>%s</code>\n━━━━━━━━━━━━━━\n📱 Deseja adicionar WhatsApp ao cliente?", htmlTitle(title), h(username), h(pass))
}
func accountWhatsAppPrompt() string {
	return "📞 <b>Digite o WhatsApp do cliente:</b>\n━━━━━━━━━━━━━━\nEx: <code>5585912345678</code> ou <code>0</code> para pular"
}
func resellerAskWhatsAppText(name string) string {
	return fmt.Sprintf("👥 <b>Criar Revenda</b>\n━━━━━━━━━━━━━━\n👑 Nome: <code>%s</code>\n━━━━━━━━━━━━━━\n📱 Deseja adicionar WhatsApp?", h(name))
}
func resellerWhatsAppPrompt() string {
	return "📞 <b>Digite o WhatsApp da revenda:</b>\n━━━━━━━━━━━━━━\nEx: <code>5585912345678</code> ou <code>0</code> para pular"
}
func monthlyValuePrompt() string {
	return "💰 <b>Digite o valor mensal:</b>\n━━━━━━━━━━━━━━\nEx: <code>20</code> ou <code>0</code> para sem valor"
}
func accountPreChoiceText(title string, data map[string]string, value float64, next string) string {
	lines := []string{htmlTitle(title), "━━━━━━━━━━━━━━", fmt.Sprintf("👤 Usuário: <code>%s</code>", h(data["username"])), fmt.Sprintf("🔒 Senha: <code>%s</code>", h(data["password"]))}
	if strings.TrimSpace(data["client_whatsapp"]) != "" {
		lines = append(lines, fmt.Sprintf("📱 WhatsApp: <code>%s</code>", h(data["client_whatsapp"])))
	}
	lines = append(lines, fmt.Sprintf("💰 Valor: <code>%s</code>", h(moneyBR(value))), "━━━━━━━━━━━━━━", next)
	return strings.Join(lines, "\n")
}
func yesNoInput(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	switch t {
	case "sim", "s", "yes", "y", "1", "✅ sim":
		return "yes"
	case "nao", "não", "n", "no", "0", "pular", "❌ não", "❌ nao":
		return "no"
	default:
		return ""
	}
}
func createdAccountKeyboard(username string) InlineKeyboardMarkup {
	return kb([]Button{{"⬅️ Voltar", "menu_accounts"}, {"📋 Copiar", "acct_copy_created:" + username}})
}
func createdAccountKeyboardCopy(a model.Account, title string) InlineKeyboardMarkup {
	return accountCopyKeyboard(a, title, true)
}
func accountPanelKeyboard(username string) InlineKeyboardMarkup {
	return kb([]Button{{"📋 Copiar", "acct_copy:" + username}}, []Button{{"🔐 Mudar senha", "acct_pass:" + username}, {"♻️ Renovar", "acct_renew:" + username}}, []Button{{"📳 Alterar limite", "acct_limit:" + username}, {"🗑️ Remover", "acct_remove:" + username}}, []Button{{"⬅️ Voltar", "menu_accounts"}})
}
func accountPanelKeyboardCopy(a model.Account) InlineKeyboardMarkup {
	return accountCopyKeyboard(a, "✏️ Editar Conta", false)
}
func accountCopyKeyboard(a model.Account, title string, created bool) InlineKeyboardMarkup {
	username := a.Username
	back := "menu_accounts"
	if !created {
		back = "menu_accounts"
	}
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "📋 Copiar", CopyText: &CopyTextButton{Text: accountCardPlain(title, a)}}},
		{{Text: "🔐 Mudar senha", CallbackData: "acct_pass:" + username}, {Text: "♻️ Renovar", CallbackData: "acct_renew:" + username}},
		{{Text: "📳 Alterar limite", CallbackData: "acct_limit:" + username}, {Text: "🗑️ Remover", CallbackData: "acct_remove:" + username}},
		{{Text: "⬅️ Voltar", CallbackData: back}},
	}}
}
func renewKeyboard(username string) InlineKeyboardMarkup {
	return kb([]Button{{"1 mês", "acct_do_renew:" + username + ":30"}, {"2 meses", "acct_do_renew:" + username + ":60"}, {"3 meses", "acct_do_renew:" + username + ":90"}}, []Button{{"⬅️ Voltar", "acct_view:" + username}})
}
func removeConfirmKeyboard(username string) InlineKeyboardMarkup {
	return kb([]Button{{"✅ Confirmar", "acct_do_remove:" + username}, {"❌ Cancelar", "acct_view:" + username}})
}
func removeAllConfirmKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"✅ Confirmar", "acct_do_remove_all"}, {"❌ Cancelar", "menu_accounts"}})
}

func (b *Bot) accountForDisplay(ctx context.Context, a model.Account) model.Account {
	if !b.currentXrayCreateEnabled(ctx) {
		a.XrayEnabled = false
	}
	return a
}

func accountHasDisplayUUID(a model.Account) bool {
	uuid := strings.TrimSpace(a.UUID)
	if !a.XrayEnabled || uuid == "" || uuid == "." || uuid == "-" {
		return false
	}
	if strings.EqualFold(uuid, "sem Xray") || strings.EqualFold(uuid, "sem xray") || strings.EqualFold(uuid, "none") || strings.EqualFold(uuid, "null") {
		return false
	}
	return true
}

func accountUUIDBlockHTML(a model.Account) string {
	if !accountHasDisplayUUID(a) {
		return ""
	}
	return fmt.Sprintf("\n🔑 UUID:\n<code>%s</code>", h(strings.TrimSpace(a.UUID)))
}

func accountUUIDBlockPlain(a model.Account) string {
	if !accountHasDisplayUUID(a) {
		return ""
	}
	return "\n🔑 UUID:\n" + strings.TrimSpace(a.UUID)
}

func accountCopyText(a model.Account) string {
	uuidLine := ""
	if accountHasDisplayUUID(a) {
		uuidLine = "\nUUID: " + strings.TrimSpace(a.UUID)
	}
	return fmt.Sprintf("📋 Dados da Conta\n━━━━━━━━━━━━━━\nUsuário: %s\nSenha: %s%s\nWhatsApp: %s\nValor: %s\nLimite: %d\nValidade: %s", a.Username, a.Password, uuidLine, nonEmptyText(a.ClientWhatsApp, "-"), moneyBR(a.MonthlyValue), nonZero(a.LimitConnections, 1), daysLeft(a.ExpiresAt))
}

func accountPanelText(a model.Account) string {
	return accountCardHTML("✏️ Editar Conta", a)
}
func accountSuccessText(title string, a model.Account) string {
	if a.IsTrial || strings.Contains(strings.ToLower(title), "teste") {
		return accountCardHTML("✅ Teste criado", a)
	}
	return accountCardHTML(title, a)
}
func accountCardHTML(title string, a model.Account) string {
	owner := a.OwnerName
	if strings.TrimSpace(owner) == "" {
		owner = "Admin"
	}
	return fmt.Sprintf("%s\n━━━━━━━━━━━━━━\n👤 Usuário: <code>%s</code>\n🔒 Senha: <code>%s</code>%s\n📳 Limite: %d\n━━━━━━━━━━━━━━\n📱 WhatsApp: <code>%s</code>\n💰 Valor: <code>%s</code>\n📆 Expira: <code>%s</code>\n━━━━━━━━━━━━━━\n👑 Vendedor: <code>%s</code>", htmlTitle(title), h(a.Username), h(a.Password), accountUUIDBlockHTML(a), nonZero(a.LimitConnections, 1), h(nonEmptyText(a.ClientWhatsApp, "-")), h(moneyBR(a.MonthlyValue)), h(daysLeft(a.ExpiresAt)), h(owner))
}
func accountCardPlain(title string, a model.Account) string {
	owner := a.OwnerName
	if strings.TrimSpace(owner) == "" {
		owner = "Admin"
	}
	return fmt.Sprintf("%s\n━━━━━━━━━━━━━━\n👤 Usuário: %s\n🔒 Senha: %s%s\n📳 Limite: %d\n━━━━━━━━━━━━━━\n📱 WhatsApp: %s\n💰 Valor: %s\n📆 Expira: %s\n━━━━━━━━━━━━━━\n👑 Vendedor: %s", title, a.Username, a.Password, accountUUIDBlockPlain(a), nonZero(a.LimitConnections, 1), nonEmptyText(a.ClientWhatsApp, "-"), moneyBR(a.MonthlyValue), daysLeft(a.ExpiresAt), owner)
}
func h(v any) string { return html.EscapeString(fmt.Sprint(v)) }
func htmlTitle(title string) string {
	if strings.Contains(title, "<b>") {
		return title
	}
	return "<b>" + h(title) + "</b>"
}
func boldTitle(title string) string {
	if strings.Contains(title, "<b>") {
		return title
	}
	parts := strings.SplitN(title, " ", 2)
	if len(parts) == 2 {
		return h(parts[0]) + " <b>" + h(parts[1]) + "</b>"
	}
	return "<b>" + h(title) + "</b>"
}
func brDate(t time.Time) string {
	if t.IsZero() {
		return "sem validade"
	}
	return t.Local().Format("02/01/2006")
}
func telegramParseMode(text string) string {
	if strings.Contains(text, "<b>") || strings.Contains(text, "<code>") || strings.Contains(text, "<i>") {
		return "HTML"
	}
	return ""
}

func formatTelegramOutgoing(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.Contains(line, "<pre") || strings.Contains(line, "</pre>") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		prefix := strings.TrimSpace(line[:idx])
		if !telegramLabelPrefixOK(prefix) || strings.Contains(prefix, "<b>") || strings.Contains(prefix, "</b>") {
			continue
		}
		rest := line[idx+1:]
		copyField := telegramCopyField(prefix)
		if copyField {
			trimmed := strings.TrimSpace(rest)
			if trimmed != "" && !strings.Contains(trimmed, "<code>") {
				rest = " <code>" + html.EscapeString(trimmed) + "</code>"
			} else if trimmed == "" && i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				if next != "" && !strings.Contains(next, "<code>") && !strings.Contains(next, "━━━━━━━━") {
					lines[i+1] = "<code>" + html.EscapeString(next) + "</code>"
				}
			}
		}
		lines[i] = "<b>" + line[:idx+1] + "</b>" + rest
	}
	return strings.Join(lines, "\n")
}
func telegramLabelPrefixOK(prefix string) bool {
	p := strings.TrimSpace(prefix)
	if p == "" || len([]rune(p)) > 48 || strings.Contains(p, "http") || strings.Contains(p, "/") || strings.Contains(p, "<code>") || strings.Contains(p, "━━━━━━━━") {
		return false
	}
	return true
}
func telegramCopyField(prefix string) bool {
	p := strings.ToLower(strings.TrimSpace(prefix))
	return strings.Contains(p, "usuário") || strings.Contains(p, "usuario") || strings.Contains(p, "senha") || strings.Contains(p, "uuid")
}

func randomDigits(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		x, _ := rand.Int(rand.Reader, big.NewInt(10))
		sb.WriteByte(byte('0' + x.Int64()))
	}
	return sb.String()
}
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (b *Bot) showStatus(ctx context.Context, chatID int64, msgID int) error {
	st, err := readServerStatus(b.svc.Config.ServerHost)
	if err != nil {
		text := fmt.Sprintf("❌ Erro ao obter status do servidor:\n<code>%s</code>", h(err.Error()))
		return b.sendOrEdit(ctx, chatID, msgID, text, backKeyboard(), "status")
	}
	text := "📶 Status do Servidor\n" +
		"━━━━━━━━━━━━━━\n" +
		fmt.Sprintf("IP: <code>%s</code>\n", h(st.Host)) +
		"━━━━━━━━━━━━━━\n" +
		fmt.Sprintf("Tempo: %s\n", h(st.Uptime)) +
		fmt.Sprintf("CPU: %s  |  RAM: %s\n", h(st.CPU), h(st.RAM)) +
		fmt.Sprintf("Disco: %s", h(st.Disk))
	return b.sendOrEdit(ctx, chatID, msgID, text, backKeyboard(), "status")
}

func commandsPanelText(actor model.Actor) string {
	var sb strings.Builder
	sb.WriteString("📋 <b>Comandos disponíveis</b>\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━\n")
	first := true
	writeCommand := func(title, command string) {
		if !first {
			sb.WriteString("\n")
		}
		first = false
		fmt.Fprintf(&sb, "<b>%s</b>\n<code>%s</code>\n", h(title), h(command))
	}

	writeCommand("Abrir o menu principal", "/menu")
	writeCommand("Abrir esta lista de comandos", "/comandos")
	writeCommand("Listar contas online", "/onlines")
	writeCommand("Listar contas ativas", "/contas")
	writeCommand("Criar conta", "/criar usuario")
	writeCommand("Criar teste", "/teste usuario")
	writeCommand("Editar conta", "/editar usuario")
	writeCommand("Remover conta", "/remover usuario")
	writeCommand("Limpar aparelhos de uma conta", "/limpar usuario")

	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		writeCommand("Remover todas as contas", "/remover todos")
		writeCommand("Limpar aparelhos de todas as contas", "/limpar")
		writeCommand("Editar Revenda/SubRevenda", "/revenda usuario")
		writeCommand("Editar Revenda/SubRevenda direto", "/editarev usuario")
		writeCommand("Relatório de pagamentos", "/relatorio")
	} else if actor.Role == model.RoleReseller {
		writeCommand("Editar SubRevenda", "/revenda usuario")
		writeCommand("Editar SubRevenda direto", "/editarev usuario")
		writeCommand("Relatório de pagamentos", "/relatorio")
	} else if actor.Role == model.RoleSubReseller {
		writeCommand("Relatório de pagamentos", "/relatorio")
	}
	return sb.String()
}

func (b *Bot) buildMainPanel(ctx context.Context, actor model.Actor) (string, error) {
	accs, _ := b.svc.Store.ListAccounts(ctx, false)
	rs, _ := b.svc.Store.ListResellers(ctx)
	menuOwners := b.visibleOwnerIDs(ctx, actor)
	now := time.Now().UTC()
	activeAcc, expiredAcc := 0, 0
	for _, a := range accs {
		if !accountVisible(actor, menuOwners, a) || a.DeletedAt != nil || a.Status == "deleted" {
			continue
		}
		if a.ExpiresAt.After(now) {
			activeAcc++
		} else {
			expiredAcc++
		}
	}
	resCount, resExpired, subCount, subExpired := 0, 0, 0, 0
	for _, r := range rs {
		if !resellerVisible(actor, r) {
			continue
		}
		expired := !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt)
		if r.Level == 1 || r.ParentTelegramID != 0 {
			subCount++
			if expired {
				subExpired++
			}
		} else {
			resCount++
			if expired {
				resExpired++
			}
		}
	}
	onlineCount := 0
	if sum, err := b.svc.Online.Summary(ctx); err == nil {
		onlineCount = onlineConnectionsTotal(filterOnline(actor, menuOwners, sum.Users))
	}
	servers, _ := b.svc.Store.ListServers(ctx)
	serverCount := len(servers)
	name := nonEmpty(actor.Name, b.svc.Config.AdminDisplayName, "Admin")
	var sb strings.Builder
	fmt.Fprintf(&sb, "⚡ <b>PRIMECEL - %s</b>\n", h(name))
	sb.WriteString("━━━━━━━━━━━━━━━━━━\n")
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		fmt.Fprintf(&sb, "👥 Revendas: %d | Expirado: %d\n", resCount+subCount, resExpired+subExpired)
		fmt.Fprintf(&sb, "👤 Contas: %d | Expirado: %d\n", activeAcc, expiredAcc)
		fmt.Fprintf(&sb, "🟢 Online: %d\n", onlineCount)
		sb.WriteString("━━━━━━━━━━━━━━━━━━\n")
		fmt.Fprintf(&sb, "🌐 Servidores: %d\n", serverCount)
	} else if actor.Role == model.RoleReseller {
		fmt.Fprintf(&sb, "👥 SubRevendas: %d | Expirada: %d\n", subCount, subExpired)
		fmt.Fprintf(&sb, "👤 Contas: %d/%s | Expirada: %d\n", activeAcc, h(b.resellerLimitText(ctx, actor)), expiredAcc)
		fmt.Fprintf(&sb, "🟢 Online: %d\n", onlineCount)
	} else {
		fmt.Fprintf(&sb, "👤 Contas: %d/%s | Expirada: %d\n", activeAcc, h(b.resellerLimitText(ctx, actor)), expiredAcc)
		fmt.Fprintf(&sb, "🟢 Online: %d\n", onlineCount)
	}
	sb.WriteString("━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString("<code>/menu</code>\n<code>/comandos</code>")
	if b.shouldShowSystemUpdateNotice(ctx) {
		sb.WriteString("\n\n✅ Sistema atualizado")
	}
	return sb.String(), nil
}

func (b *Bot) accountsPanelText(ctx context.Context, actor model.Actor) (string, error) {
	accs, _ := b.svc.Store.ListAccounts(ctx, false)
	accountOwners := b.directOwnerIDs(ctx, actor)
	onlineOwners := b.visibleOwnerIDs(ctx, actor)
	now := time.Now().UTC()
	contas := 0
	for _, a := range accs {
		if accountVisible(actor, accountOwners, a) && a.DeletedAt == nil && a.Status != "deleted" && a.ExpiresAt.After(now) {
			contas++
		}
	}
	onlineCount := 0
	if sum, err := b.svc.Online.Summary(ctx); err == nil {
		onlineCount = onlineConnectionsTotal(filterOnline(actor, onlineOwners, sum.Users))
	}
	contasText := strconv.Itoa(contas)
	if !actor.IsAdmin {
		contasText = fmt.Sprintf("%d/%s", contas, b.resellerLimitText(ctx, actor))
	}
	return "🚀 <b>GESTOR PRIMECEL</b>\n" +
		"━━━━━━<b>STATUS</b>━━━━━━\n" +
		fmt.Sprintf("👤 <b>Contas:</b> %s\n", contasText) +
		fmt.Sprintf("🟢 <b>Online:</b> %d", onlineCount), nil
}

func (b *Bot) resellersPanelText(ctx context.Context, actor model.Actor) (string, error) {
	rs, _ := b.svc.Store.ListResellers(ctx)
	total := 0
	for _, r := range rs {
		if resellerVisible(actor, r) {
			total++
		}
	}
	if actor.IsAdmin {
		return fmt.Sprintf("🚀 <b>GESTOR PRIMECEL</b>\n━━━━━━<b>STATUS</b>━━━━━━\n👥 <b>Revendas:</b> %d", total), nil
	}
	return fmt.Sprintf("🚀 <b>GESTOR PRIMECEL</b>\n━━━━━━<b>STATUS</b>━━━━━━\n👥 <b>Revendas:</b> %d\n👤 <b>Contas:</b> %s", total, h(b.resellerLimitText(ctx, actor))), nil
}

func (b *Bot) resellerLimitText(ctx context.Context, actor model.Actor) string {
	if actor.IsAdmin || actor.TelegramID == 0 {
		return "100"
	}
	r, _ := b.svc.Store.FindReseller(ctx, actor.TelegramID)
	if r == nil {
		return "0"
	}
	return strconv.Itoa(r.Credits)
}

func daysOnly(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	d := time.Until(t)
	if d < 0 {
		return "0"
	}
	return strconv.Itoa(int(d.Hours() / 24))
}

func appVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "-" {
		return "-"
	}
	if strings.HasPrefix(strings.ToLower(v), "v") {
		return v
	}
	return "v" + v
}

func (b *Bot) resolveActor(ctx context.Context, userID int64, name string) model.Actor {
	for _, id := range b.svc.Config.AdminIDs {
		if id == userID {
			return model.Actor{TelegramID: userID, Name: nonEmpty(b.svc.Config.AdminDisplayName, name, "Admin"), Role: model.RoleAdmin, IsAdmin: true}
		}
	}
	if r, _ := b.svc.Store.FindReseller(ctx, userID); r != nil {
		role := model.RoleReseller
		if r.Level == 1 || r.ParentTelegramID != 0 {
			role = model.RoleSubReseller
		}
		return model.Actor{TelegramID: userID, Name: nonEmpty(r.Name, name), Role: role, ParentID: r.ParentTelegramID, IsAdmin: false}
	}
	return model.Actor{TelegramID: userID, Name: name, Role: "guest", IsAdmin: false}
}

func (b *Bot) currentGestorOnly(ctx context.Context) bool {
	if v, _ := b.svc.Store.GetSetting(ctx, "principal_manager_only"); strings.TrimSpace(v) != "" {
		return parseSettingBool(v)
	}
	return b.svc.Config.PrincipalManagerOnly
}

func (b *Bot) currentXrayCreateEnabled(ctx context.Context) bool {
	if v, _ := b.svc.Store.GetSetting(ctx, "xray_create_enabled"); strings.TrimSpace(v) != "" {
		return parseSettingBool(v)
	}
	return b.svc.Config.XrayCreateEnabled
}

func parseSettingBool(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true" || v == "yes" || v == "sim" || v == "on"
}

func (b *Bot) showAdministration(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showAdministrationNotice(ctx, actor, chatID, msgID, "")
}

func (b *Bot) showAdministrationNotice(ctx context.Context, actor model.Actor, chatID int64, msgID int, notice string) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	gestorOnly := b.currentGestorOnly(ctx)
	xrayGeneral := b.currentXrayCreateEnabled(ctx)
	backupStatus := b.backupAdminStatusText(ctx)
	gestorBadge := "🔴"
	if gestorOnly {
		gestorBadge = "🟢"
	}
	xrayBadge := "🔴"
	if xrayGeneral {
		xrayBadge = "🟢"
	}
	expirationBadge := "🔴"
	if b.currentExpirationNoticesEnabled(ctx) {
		expirationBadge = "🟢"
	}
	text := "⚙️ Administração\n━━━━━━━━━━━━━━━━━━\n"
	if strings.TrimSpace(notice) != "" {
		text += strings.TrimSpace(notice) + "\n\n"
	}
	text += fmt.Sprintf("%s\nGestor Only: %s\nXray Geral: %s\nAvisos Expiração: %s", backupStatus, gestorBadge, xrayBadge, expirationBadge)
	return b.sendOrEdit(ctx, chatID, msgID, text, adminKeyboard(gestorOnly, xrayGeneral, b.currentExpirationNoticesEnabled(ctx)), "submenu")
}

func (b *Bot) restartBot(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	if err := b.sendOrEdit(ctx, chatID, msgID, "🔄 Reiniciando bot...", adminKeyboard(b.currentGestorOnly(ctx)), "flow"); err != nil {
		return err
	}
	go func() {
		time.Sleep(800 * time.Millisecond)
		_ = exec.Command("systemctl", "restart", "primecel-gestor").Start()
	}()
	return nil
}

func (b *Bot) clearSystemCache(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	if err := b.sendOrEdit(ctx, chatID, msgID, "🧹 Limpando cache do servidor...", adminKeyboard(b.currentGestorOnly(ctx), b.currentXrayCreateEnabled(ctx)), "flow"); err != nil {
		return err
	}
	cmdText := `
set +e
export DEBIAN_FRONTEND=noninteractive
if command -v sudo >/dev/null 2>&1; then SUDO=sudo; else SUDO=; fi
echo "=== Antes ==="
df -h /
$SUDO apt clean
$SUDO apt autoremove -y
$SUDO journalctl --vacuum-size=100M
$SUDO rm -rf /tmp/*
$SUDO rm -rf /var/tmp/*
go clean -cache -modcache -testcache 2>/dev/null || true
npm cache clean --force 2>/dev/null || true
rm -rf /root/.cache/*
echo "=== Depois ==="
df -h /
`
	runCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "bash", "-lc", cmdText).CombinedOutput()
	msg := "✅ Cache limpo com sucesso."
	if runCtx.Err() == context.DeadlineExceeded {
		msg = "⚠️ Limpeza de cache excedeu o tempo limite. Parte da limpeza pode ter sido executada."
	} else if err != nil {
		msg = "⚠️ Limpeza finalizada com alerta: " + err.Error()
	}
	return b.sendOrEdit(ctx, chatID, msgID, msg+"\n\n<code>"+h(tailText(string(out), 2500))+"</code>", adminKeyboard(b.currentGestorOnly(ctx), b.currentXrayCreateEnabled(ctx)), "flow")
}

func (b *Bot) resourceAdminStatusText(ctx context.Context) string {
	disk := readCommandField(ctx, "df -P / | awk 'NR==2{print $5}'")
	ram := readCommandField(ctx, `free | awk '/Mem:/ {printf "%.0f%%", ($3/$2)*100}'`)
	load := readCommandField(ctx, "awk '{print $1}' /proc/loadavg")
	status := "✅"
	if percentValue(disk) >= 80 || percentValue(ram) >= 85 {
		status = "⚠️"
	}
	return fmt.Sprintf("Recursos: %s Disco %s | RAM %s | Load %s", status, nonEmptyText(disk, "-"), nonEmptyText(ram, "-"), nonEmptyText(load, "-"))
}

func (b *Bot) showResourceStatus(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	cmd := `
echo "=== DISCO ==="
df -h /
echo ""
echo "=== RAM ==="
free -h
echo ""
echo "=== LOAD ==="
cat /proc/loadavg
echo ""
echo "=== TOP PASTAS ==="
du -h -d1 /root /var /opt /etc/primecel-gestor 2>/dev/null | sort -hr | head -20
`
	out, err := runShellLimited(ctx, cmd, 8*time.Second)
	if err != nil {
		out += "\n" + err.Error()
	}
	return b.sendOrEdit(ctx, chatID, msgID, "📊 Recursos do Servidor\n━━━━━━━━━━━━━━\n<code>"+h(tailText(out, 3500))+"</code>", adminKeyboard(b.currentGestorOnly(ctx), b.currentXrayCreateEnabled(ctx)), "flow")
}

func (b *Bot) rotateOldEventsNow(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	rep := b.rotateOldEvents(ctx)
	return b.showAdministrationNotice(ctx, actor, chatID, msgID, "✅ Eventos antigos rotacionados.\n"+rep)
}

func (b *Bot) expirationNoticeLoop(ctx context.Context) {
	if b == nil || b.svc.Store == nil || b.client == nil {
		return
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			b.cleanupExpiredNoticeMessages(ctx)
			b.runExpirationNotices(ctx)
			timer.Reset(5 * time.Minute)
		}
	}
}

func (b *Bot) runExpirationNotices(ctx context.Context) {
	if !b.currentExpirationNoticesEnabled(ctx) {
		return
	}
	now := time.Now().UTC()
	limit := renewalNoticeWindow
	accountsList, err := b.svc.Store.ListAccounts(ctx, false)
	if err == nil {
		for _, acc := range accountsList {
			if acc.DeletedAt != nil || strings.EqualFold(acc.Status, "deleted") || acc.ExpiresAt.IsZero() {
				continue
			}
			remaining := acc.ExpiresAt.Sub(now)
			if remaining <= 0 || remaining > limit || acc.OwnerTelegramID == 0 {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(acc.Username))
			if key == "" || b.expirationNoticeAlreadySent(ctx, "account_owner", key, acc.ExpiresAt) {
				continue
			}
			text := accountOwnerExpirationNoticeText(acc.Username, remaining)
			if b.sendExpirationNotice(ctx, acc.OwnerTelegramID, text) {
				_ = b.markExpirationNoticeSent(ctx, "account_owner", key, acc.ExpiresAt)
			}
		}
	}
	resellersList, err := b.svc.Store.ListResellers(ctx)
	if err != nil {
		return
	}
	for _, r := range resellersList {
		if r.DeletedAt != nil || !r.Active || r.ExpiresAt.IsZero() {
			continue
		}
		remaining := r.ExpiresAt.Sub(now)
		if remaining <= 0 || remaining > limit {
			continue
		}
		key := strconv.FormatInt(r.TelegramID, 10)
		if r.TelegramID != 0 && !b.expirationNoticeAlreadySent(ctx, "reseller_self", key, r.ExpiresAt) {
			text := selfExpirationNoticeText(r.Name, remaining)
			if b.sendExpirationNotice(ctx, r.TelegramID, text) {
				_ = b.markExpirationNoticeSent(ctx, "reseller_self", key, r.ExpiresAt)
			}
		}
		if !b.expirationNoticeAlreadySent(ctx, "reseller_owner", key, r.ExpiresAt) {
			recipients := b.resellerExpirationOwnerRecipients(r)
			if len(recipients) > 0 {
				text := resellerOwnerExpirationNoticeText(r.Name, remaining)
				if b.sendExpirationNoticeToAny(ctx, recipients, text) {
					_ = b.markExpirationNoticeSent(ctx, "reseller_owner", key, r.ExpiresAt)
				}
			}
		}
	}
}

func (b *Bot) sendExpirationNotice(ctx context.Context, chatID int64, text string) bool {
	if b == nil || b.client == nil || chatID == 0 || strings.TrimSpace(text) == "" {
		return false
	}
	msg, err := b.client.SendMessage(ctx, chatID, text, InlineKeyboardMarkup{})
	if err != nil {
		return false
	}
	if msg != nil && msg.MessageID != 0 {
		_ = b.registerExpirationNoticeMessage(ctx, chatID, msg.MessageID)
	}
	b.sendMainMenuBelowNotice(ctx, chatID)
	return true
}

func (b *Bot) registerExpirationNoticeMessage(ctx context.Context, chatID int64, msgID int) error {
	if b == nil || b.svc.Store == nil || chatID == 0 || msgID == 0 {
		return nil
	}
	now := time.Now().UTC()
	deleteAfter := now.Add(renewalNoticeAutoDeleteAfter).Format(time.RFC3339)
	return b.svc.Store.Exec(ctx, `INSERT OR IGNORE INTO expiration_notice_messages(chat_id,message_id,delete_after,deleted_at,created_at) VALUES(?,?,?,?,?)`, chatID, msgID, deleteAfter, "", now.Format(time.RFC3339))
}

func (b *Bot) cleanupExpiredNoticeMessages(ctx context.Context) {
	if b == nil || b.client == nil || b.svc.Store == nil {
		return
	}
	rows, err := b.svc.Store.Query(ctx, `SELECT id, chat_id, message_id FROM expiration_notice_messages WHERE deleted_at='' AND delete_after<=? ORDER BY id LIMIT 100`, time.Now().UTC().Format(time.RFC3339))
	if err != nil || len(rows) == 0 {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, r := range rows {
		chatID, _ := strconv.ParseInt(strings.TrimSpace(r["chat_id"]), 10, 64)
		msgID, _ := strconv.Atoi(strings.TrimSpace(r["message_id"]))
		id, _ := strconv.ParseInt(strings.TrimSpace(r["id"]), 10, 64)
		if chatID != 0 && msgID != 0 {
			_ = b.client.DeleteMessage(ctx, chatID, msgID)
			time.Sleep(40 * time.Millisecond)
		}
		if id != 0 {
			_ = b.svc.Store.Exec(ctx, `UPDATE expiration_notice_messages SET deleted_at=? WHERE id=?`, now, id)
		}
	}
}

func (b *Bot) sendExpirationNoticeToAny(ctx context.Context, chatIDs []int64, text string) bool {
	sent := false
	seen := map[int64]bool{}
	for _, chatID := range chatIDs {
		if chatID == 0 || seen[chatID] {
			continue
		}
		seen[chatID] = true
		if b.sendExpirationNotice(ctx, chatID, text) {
			sent = true
			time.Sleep(40 * time.Millisecond)
		}
	}
	return sent
}

func (b *Bot) resellerExpirationOwnerRecipients(r model.Reseller) []int64 {
	seen := map[int64]bool{}
	out := []int64{}
	add := func(id int64) {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if r.ParentTelegramID != 0 {
		add(r.ParentTelegramID)
	} else {
		for _, id := range b.svc.Config.AdminIDs {
			add(id)
		}
	}
	return out
}

func (b *Bot) expirationNoticeAlreadySent(ctx context.Context, typ, key string, expiresAt time.Time) bool {
	rows, err := b.svc.Store.Query(ctx, `SELECT 1 FROM expiration_notice_state WHERE subject_type=? AND subject_key=? AND expires_at=? LIMIT 1`, typ, key, expiresAt.UTC().Format(time.RFC3339))
	return err == nil && len(rows) > 0
}

func (b *Bot) markExpirationNoticeSent(ctx context.Context, typ, key string, expiresAt time.Time) error {
	return b.svc.Store.Exec(ctx, `INSERT OR IGNORE INTO expiration_notice_state(subject_type,subject_key,expires_at,sent_at) VALUES(?,?,?,?)`, typ, key, expiresAt.UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
}

func selfExpirationNoticeText(username string, remaining time.Duration) string {
	return fmt.Sprintf("⚠️ Aviso de Vencimento:\n━━━━━━━━━━━━━━\nOlá, <code>%s</code>, sua Revenda/Sub/Conta\n⏳ Vence em: %s\n━━━━━━━━━━━━━━\nPara evitar o bloqueio, faça a renovação.", h(username), h(expirationRemainingText(remaining)))
}

func accountOwnerExpirationNoticeText(username string, remaining time.Duration) string {
	return fmt.Sprintf("⚠️ Conta vence hoje\n━━━━━━━━━━━━━━\n👤 Conta: <code>%s</code>\n⏳ Vence em: %s\n━━━━━━━━━━━━━━\nUse <code>/editar</code> %s para renovar", h(username), h(expirationRemainingText(remaining)), h(username))
}

func resellerOwnerExpirationNoticeText(username string, remaining time.Duration) string {
	return fmt.Sprintf("⚠️ Revenda vence hoje\n━━━━━━━━━━━━━━\n👤 Revenda: <code>%s</code>\n⏳ Vence em: %s\n━━━━━━━━━━━━━━\nUse <code>/editarev</code> %s para renovar", h(username), h(expirationRemainingText(remaining)), h(username))
}

func expirationRemainingText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%02dh:%02d", h, m)
}

func (b *Bot) expiredAccessLoop(ctx context.Context) {
	if b != nil && b.svc.Accounts != nil {
		if suspended, err := b.svc.Accounts.SuspendExpiredAccess(ctx); err == nil && len(suspended) > 0 {
			b.autoSyncStateSnapshot()
		}
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if b == nil || b.svc.Accounts == nil {
				continue
			}
			suspended, err := b.svc.Accounts.SuspendExpiredAccess(ctx)
			if err == nil && len(suspended) > 0 {
				b.autoSyncStateSnapshot()
			}
		}
	}
}

func (b *Bot) maintenanceLoop(ctx context.Context) {
	b.cleanupRuntimeLogs(ctx)
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.cleanupRuntimeLogs(ctx)
		}
	}
}

func (b *Bot) cleanupRuntimeLogs(ctx context.Context) {
	now := time.Now().UTC()
	unpaidOrderCutoff := now.Add(-1 * time.Hour).Format(time.RFC3339)
	paymentLogCutoff := now.Add(-2 * time.Hour).Format(time.RFC3339)
	runtimeCutoff := now.Add(-12 * time.Hour).Format(time.RFC3339)

	b.cleanupUnpaidPaymentOrders(ctx, unpaidOrderCutoff)

	// Logs técnicos de pedidos/WebHook Pix: apaga automaticamente após 2 horas.
	// Pedidos pagos continuam em payment_orders para alimentar o relatório mensal/anual.
	for _, tbl := range []string{"payment_webhook_events", "payment_events"} {
		_ = b.svc.Store.Exec(ctx, "DELETE FROM "+tbl+" WHERE created_at<?", paymentLogCutoff)
	}

	_ = b.svc.Store.Exec(ctx, `DELETE FROM account_events WHERE event_type LIKE 'remote_sync_%' AND created_at<?`, runtimeCutoff)
	for _, tbl := range []string{"cloudflare_events", "notice_events"} {
		_ = b.svc.Store.Exec(ctx, "DELETE FROM "+tbl+" WHERE created_at<?", runtimeCutoff)
	}
	for _, pattern := range []string{
		"/tmp/primecel-gestor*.log",
		"/tmp/primecel-gestor-*.json",
		"/tmp/primecel-online-*.json",
		"/tmp/primecel-*.log",
	} {
		files, _ := filepath.Glob(pattern)
		for _, file := range files {
			info, err := os.Stat(file)
			if err != nil || info.IsDir() || time.Since(info.ModTime()) < 12*time.Hour {
				continue
			}
			_ = os.Remove(file)
		}
	}
	_ = b.svc.Store.SetSetting(ctx, "unpaid_payment_orders_last_cleanup_at", now.Format(time.RFC3339))
	_ = b.svc.Store.SetSetting(ctx, "payment_logs_last_cleanup_at", now.Format(time.RFC3339))
	_ = b.svc.Store.SetSetting(ctx, "logs_last_hidden_cleanup_at", now.Format(time.RFC3339))
}

func (b *Bot) cleanupUnpaidPaymentOrders(ctx context.Context, cutoff string) {
	if b == nil || b.svc.Store == nil || strings.TrimSpace(cutoff) == "" {
		return
	}
	rows, err := b.svc.Store.Query(ctx, `SELECT order_id FROM payment_orders WHERE created_at<? AND COALESCE(paid_at,'')='' AND COALESCE(applied_at,'')='' AND lower(COALESCE(status,'')) NOT IN ('approved','paid','confirmed','success','succeeded','applied') LIMIT 500`, cutoff)
	if err != nil || len(rows) == 0 {
		return
	}
	removed := 0
	for _, r := range rows {
		orderID := strings.TrimSpace(r["order_id"])
		if orderID == "" {
			continue
		}
		_ = b.svc.Store.Exec(ctx, `DELETE FROM payment_events WHERE order_id=?`, orderID)
		_ = b.svc.Store.Exec(ctx, `DELETE FROM payment_webhook_events WHERE order_id=?`, orderID)
		_ = b.svc.Store.Exec(ctx, `UPDATE whatsapp_renewal_requests SET status='expired', updated_at=? WHERE order_id=? AND status NOT IN ('approved','paid','applied','completed')`, time.Now().UTC().Format(time.RFC3339), orderID)
		if err := b.svc.Store.Exec(ctx, `DELETE FROM payment_orders WHERE order_id=? AND COALESCE(paid_at,'')='' AND COALESCE(applied_at,'')=''`, orderID); err == nil {
			removed++
		}
	}
	if removed > 0 {
		_ = b.svc.Store.SetSetting(ctx, "unpaid_payment_orders_last_removed", strconv.Itoa(removed))
	}
}

func (b *Bot) rotateOldEvents(ctx context.Context) string {
	cutoff := time.Now().UTC().AddDate(0, 0, -90).Format(time.RFC3339)
	tables := []string{"payment_webhook_events", "payment_events", "cloudflare_events", "notice_events", "account_events"}
	parts := []string{}
	for _, tbl := range tables {
		before := countRows(ctx, b.svc.Store, tbl, cutoff)
		if before > 0 {
			_ = b.svc.Store.Exec(ctx, "DELETE FROM "+tbl+" WHERE created_at<?", cutoff)
		}
		parts = append(parts, fmt.Sprintf("%s:%d", tbl, before))
	}
	_ = b.svc.Store.SetSetting(ctx, "events_last_rotation_at", time.Now().UTC().Format(time.RFC3339))
	return strings.Join(parts, " | ")
}

func countRows(ctx context.Context, st *store.DB, table, cutoff string) int {
	rows, err := st.Query(ctx, "SELECT COUNT(*) AS n FROM "+table+" WHERE created_at<?", cutoff)
	if err != nil || len(rows) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(rows[0]["n"])
	return n
}

func readCommandField(ctx context.Context, cmd string) string {
	out, err := runShellLimited(ctx, cmd, 3*time.Second)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func runShellLimited(ctx context.Context, cmd string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "bash", "-lc", cmd).CombinedOutput()
	return string(out), err
}

func percentValue(s string) int {
	s = strings.TrimSpace(strings.TrimSuffix(s, "%"))
	n, _ := strconv.Atoi(s)
	return n
}

func tailText(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "..." + string(r[len(r)-max:])
}

func serverBriefError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	for strings.Contains(msg, "  ") {
		msg = strings.ReplaceAll(msg, "  ", " ")
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "permission denied"):
		msg = "Senha root/SSH inválida ou login root bloqueado."
	case strings.Contains(low, "connection refused"):
		msg = "SSH ou agente recusou conexão."
	case strings.Contains(low, "no route to host"):
		msg = "VPS inacessível pela rede."
	case strings.Contains(low, "sshpass não instalado"):
		msg = "sshpass não instalado no servidor principal."
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline"):
		msg = "Timeout ao acessar a VPS."
	}
	if len([]rune(msg)) > 180 {
		r := []rune(msg)
		msg = string(r[:180]) + "..."
	}
	return msg
}

func (b *Bot) clearExpiredAccounts(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	accs, _ := b.svc.Store.ListAccounts(ctx, false)
	owners := b.directOwnerIDs(ctx, actor)
	now := time.Now().UTC()
	removed := 0
	for _, a := range accs {
		if !accountVisible(actor, owners, a) || a.DeletedAt != nil || a.Status == "deleted" || (a.ExpiresAt.After(now) && !strings.EqualFold(a.Status, "suspended")) {
			continue
		}
		if err := b.svc.Accounts.Remove(ctx, actor, a.Username); err == nil {
			removed++
		}
	}
	return b.sendOrEdit(ctx, chatID, msgID, fmt.Sprintf("🗑️ Limpar Expirados\n━━━━━━━━━━━━━━━━━━\nContas removidas: %d", removed), accountsKeyboard(actor), "flow")
}

func (b *Bot) showServers(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	servers, err := b.svc.Store.ListServers(ctx)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao listar servidores: "+err.Error(), backKeyboard(), "flow")
	}
	var sb strings.Builder
	sb.WriteString("🌐 Servidores\n━━━━━━━━━━━━━━\n")
	if len(servers) == 0 {
		sb.WriteString("Nenhuma VPS secundária cadastrada.\n")
	} else {
		for i, srv := range servers {
			label := fmt.Sprintf("Sv%d", i+1)
			status := b.serverStatusIcon(ctx, srv)
			if status == "❌" {
				status = "🚫"
			}
			fmt.Fprintf(&sb, "🌐 %s • %s • %s\n", label, displayHostPlain(srv.Host), status)
		}
	}
	sb.WriteString("━━━━━━━━━━━━━━")
	return b.sendOrEdit(ctx, chatID, msgID, sb.String(), serversKeyboard(), "submenu")
}

func (b *Bot) showServerEditList(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showServerEditListPage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) showServerEditListPage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	servers, err := b.svc.Store.ListServers(ctx)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao listar servidores: "+err.Error(), serversKeyboard(), "flow")
	}
	page, pages, start, end := paginateBounds(len(servers), page, listPageSize)
	var sb strings.Builder
	sb.WriteString("✏️ Editar Servidor\n━━━━━━━━━━━━━━\n")
	if len(servers) == 0 {
		sb.WriteString("Nenhuma VPS secundária cadastrada.\n")
	} else {
		sb.WriteString("Escolha uma VPS para editar IP, senha ou remover.\n")
	}
	sb.WriteString("━━━━━━━━━━━━━━")
	sb.WriteString(pageIndicator(page, pages))
	return b.sendOrEdit(ctx, chatID, msgID, sb.String(), serversEditPagedKeyboard(servers, start, end, page, pages), "submenu")
}

func (b *Bot) startAddServer(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	if err := b.setFlow(ctx, actor, chatID, "server_add_host", map[string]string{}); err != nil {
		return err
	}
	return b.sendOrEdit(ctx, chatID, msgID, "🌐 Adicionar Servidor\n━━━━━━━━━━━━━━\nDigite o IP da VPS secundária.", backKeyboard(), "flow")
}

func (b *Bot) showServerPanel(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	srv, err := b.svc.Store.FindServer(ctx, id)
	if err != nil || srv == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Servidor não encontrado.", serversKeyboard(), "flow")
	}
	status := b.serverStatusText(ctx, *srv)
	text := fmt.Sprintf("🌐 Servidor - %s\n━━━━━━━━━━━━━━\nIP: %s\nUsuário: %s\nSenha: %s\nStatus: %s", serverDisplayName(ctx, b.svc.Store, *srv), displayHostPlain(srv.Host), nonEmpty(srv.SSHUser, "root"), maskSecret(srv.SSHPassword), status)
	return b.sendOrEdit(ctx, chatID, msgID, text, serverPanelKeyboard(*srv), "flow")
}

func (b *Bot) startEditServerField(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64, field string) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	srv, err := b.svc.Store.FindServer(ctx, id)
	if err != nil || srv == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Servidor não encontrado.", serversKeyboard(), "flow")
	}
	state := "server_edit_ip_state"
	prompt := "Digite o novo IP da VPS secundária."
	if field == "token" || field == "password" {
		state = "server_edit_password_state"
		prompt = "Digite a nova senha root/SSH da VPS secundária."
	}
	_ = b.setFlow(ctx, actor, chatID, state, map[string]string{"server_id": strconv.FormatInt(id, 10)})
	return b.sendOrEdit(ctx, chatID, msgID, "🌐 Servidor - "+serverDisplayName(ctx, b.svc.Store, *srv)+"\n━━━━━━━━━━━━━━\n"+prompt, backKeyboard(), "flow")
}

func (b *Bot) confirmRemoveServer(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	srv, err := b.svc.Store.FindServer(ctx, id)
	if err != nil || srv == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Servidor não encontrado.", serversKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, fmt.Sprintf("🗑️ Remover Servidor\n━━━━━━━━━━━━━━\n%s | %s\n\nConfirma remover este servidor?", serverDisplayName(ctx, b.svc.Store, *srv), displayHostPlain(srv.Host)), serverRemoveKeyboard(id), "flow")
}

func (b *Bot) doRemoveServer(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	srv, err := b.svc.Store.FindServer(ctx, id)
	if err != nil || srv == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Servidor não encontrado.", serversKeyboard(), "flow")
	}
	removedIP := strings.TrimSpace(srv.Host)
	if err := b.svc.Store.DeleteServer(ctx, id); err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao remover servidor: "+err.Error(), serversKeyboard(), "flow")
	}
	// Cloudflare padrão só é alterado quando servidor entra/sai do bot.
	// Se outro servidor ativo ainda usa o mesmo IP, nada é removido.
	b.removeDeletedServerFromDefaultCloudflare(ctx, removedIP)
	return b.showServers(ctx, actor, chatID, msgID)
}

func (b *Bot) defaultCloudflareServerDomain() string {
	return cloudflare.DefaultServerDomain
}

func (b *Bot) addServerToDefaultCloudflare(ctx context.Context, ip string) {
	ip = strings.TrimSpace(ip)
	if ip == "" || strings.TrimSpace(b.cloudflareToken(ctx)) == "" {
		return
	}
	// Domínio padrão dos servidores: garante somente o IP em vpn.primecel.shop.
	_, _ = cloudflare.NewManager(b.svc.Config, b.svc.Store).EnsureServerDNSIP(ctx, ip, false)
}

func (b *Bot) removeDeletedServerFromDefaultCloudflare(ctx context.Context, removedIP string) {
	removedIP = strings.TrimSpace(removedIP)
	if removedIP == "" || strings.TrimSpace(b.cloudflareToken(ctx)) == "" {
		return
	}
	// Se outro servidor ativo ainda usa o mesmo IP, mantém o registro A.
	if b.serverIPStillRegistered(ctx, removedIP) {
		rep := cloudflare.SyncReport{Domain: cloudflare.DefaultServerDomain, DesiredIPs: []string{removedIP}}
		payload, _ := json.Marshal(rep)
		_ = b.svc.Store.AddCloudflareEvent(ctx, "server_dns_remove_kept_ip_still_active", cloudflare.DefaultServerDomain, true, string(payload))
		return
	}
	cfCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, _ = cloudflare.NewManager(b.svc.Config, b.svc.Store).RemoveServerDNSIP(cfCtx, removedIP, false)
}

func (b *Bot) cloudflareToken(ctx context.Context) string {
	if v, _ := b.svc.Store.GetSetting(ctx, "cloudflare_token"); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(b.svc.Config.CloudflareToken)
}

func (b *Bot) serverIPStillRegistered(ctx context.Context, ip string) bool {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return false
	}
	servers, err := b.svc.Store.ListServers(ctx)
	if err != nil {
		return true
	}
	for _, s := range servers {
		if s.Enabled && strings.TrimSpace(s.Host) == ip {
			return true
		}
	}
	return false
}

func (b *Bot) removeDeletedServerFromDNSVPS(ctx context.Context, removedIP string) {
	// Fluxo complementar removido; mantém compatibilidade interna sem tocar na Cloudflare.
}

func (b *Bot) confirmRestartServers(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	servers, err := b.svc.Store.ListServers(ctx)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao listar servidores: "+err.Error(), serversKeyboard(), "flow")
	}
	var sb strings.Builder
	sb.WriteString("🔁 Reiniciar VPS secundárias\n━━━━━━━━━━━━━━\n")
	if len(servers) == 0 {
		sb.WriteString("Nenhuma VPS secundária cadastrada.\n")
		sb.WriteString("━━━━━━━━━━━━━━")
		return b.sendOrEdit(ctx, chatID, msgID, sb.String(), serversKeyboard(), "flow")
	}
	for i, srv := range servers {
		fmt.Fprintf(&sb, "🌐 Sv%d • %s\n", i+1, displayHostPlain(srv.Host))
	}
	sb.WriteString("\nConfirma reiniciar todos os servidores secundários?\n")
	sb.WriteString("━━━━━━━━━━━━━━")
	return b.sendOrEdit(ctx, chatID, msgID, sb.String(), serversRestartConfirmKeyboard(), "flow")
}

func (b *Bot) restartSecondaryServers(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	servers, _ := b.svc.Store.ListServers(ctx)
	if len(servers) == 0 {
		return b.sendOrEdit(ctx, chatID, msgID, "🔁 Reiniciar VPS secundárias\n━━━━━━━━━━━━━━\nNenhum servidor secundário cadastrado.", serversKeyboard(), "flow")
	}
	statuses := make([]string, len(servers))
	for i, srv := range servers {
		statuses[i] = fmt.Sprintf("💬 Sv%d • %s • aguardando", i+1, displayHostPlain(srv.Host))
	}
	render := func(current, okCount int) string {
		var sb strings.Builder
		sb.WriteString("🔁 Reiniciando VPS secundárias..\n━━━━━━━━━━━━━━\n")
		for _, line := range statuses {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		icon := "⏳"
		if current >= len(statuses) {
			if okCount == len(statuses) {
				icon = "✅"
			} else if okCount == 0 {
				icon = "❌"
			} else {
				icon = "⚠️"
			}
		}
		fmt.Fprintf(&sb, "━━━━━━━━━━━━━━\nProgresso: %d/%d  | Enviados: %d/%d %s", current, len(statuses), okCount, len(statuses), icon)
		return sb.String()
	}
	mgr := remotesync.NewManager(b.svc.Config, b.svc.Store)
	okCount := 0
	results, err := mgr.RestartServersProgress(ctx, func(r remotesync.ServerResult, idx, total int) {
		pos := idx - 1
		if pos < 0 || pos >= len(statuses) {
			return
		}
		label := fmt.Sprintf("Sv%d", idx)
		if r.Resp.Action == "restarting" {
			statuses[pos] = fmt.Sprintf("⏳ %s • %s • enviando reinício", label, displayHostPlain(r.Server.Host))
			_ = b.sendOrEdit(ctx, chatID, msgID, render(idx, okCount), InlineKeyboardMarkup{}, "flow")
			return
		}
		if r.OK {
			okCount++
			statuses[pos] = fmt.Sprintf("✅ %s • %s • reinício enviado", label, displayHostPlain(r.Server.Host))
		} else {
			errMsg := r.Error
			if errMsg == "" {
				errMsg = r.Resp.Error
			}
			if errMsg == "" {
				errMsg = "erro"
			}
			statuses[pos] = fmt.Sprintf("🚫 %s • %s • %s", label, displayHostPlain(r.Server.Host), errMsg)
		}
		_ = b.sendOrEdit(ctx, chatID, msgID, render(idx, okCount), InlineKeyboardMarkup{}, "flow")
	})
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao reiniciar servidores: "+err.Error(), serversKeyboard(), "flow")
	}
	if len(results) == 0 {
		return b.sendOrEdit(ctx, chatID, msgID, render(0, 0), serversKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, render(len(statuses), okCount), serversKeyboard(), "flow")
}

func (b *Bot) syncServersNow(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	servers, _ := b.svc.Store.ListServers(ctx)
	if len(servers) == 0 {
		return b.sendOrEdit(ctx, chatID, msgID, "🔄 Sincronizando..\n━━━━━━━━━━━━━━\nNenhum servidor secundário cadastrado.\n━━━━━━━━━━━━━━\nProgresso: 0/0  | Resultado: 0/0 ✅", serversKeyboard(), "flow")
	}
	statuses := make([]string, len(servers))
	for i, srv := range servers {
		statuses[i] = fmt.Sprintf("💬 Sv%d • %s • aguardando", i+1, displayHostPlain(srv.Host))
	}
	render := func(current, okCount int) string {
		var sb strings.Builder
		sb.WriteString("🔄 Aguarde..\n━━━━━━━━━━━━━━\n")
		for _, line := range statuses {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		icon := "⏳"
		if current >= len(statuses) {
			if okCount == len(statuses) {
				icon = "✅"
			} else if okCount == 0 {
				icon = "❌"
			} else {
				icon = "⚠️"
			}
		}
		fmt.Fprintf(&sb, "━━━━━━━━━━━━━━\nProgresso: %d/%d  | Resultado: %d/%d %s", current, len(statuses), okCount, len(statuses), icon)
		return sb.String()
	}

	mgr := remotesync.NewManager(b.svc.Config, b.svc.Store)
	okCount := 0
	results, err := mgr.SyncStateSnapshotProgress(ctx, func(r remotesync.ServerResult, idx, total int) {
		pos := idx - 1
		if pos < 0 || pos >= len(statuses) {
			return
		}
		label := fmt.Sprintf("Sv%d", idx)
		if r.Resp.Action == "syncing" {
			statuses[pos] = fmt.Sprintf("⏳ %s • %s • sincronizando", label, r.Server.Host)
			_ = b.sendOrEdit(ctx, chatID, msgID, render(idx-1, okCount), InlineKeyboardMarkup{}, "flow")
			return
		}
		if r.OK {
			okCount++
			statuses[pos] = fmt.Sprintf("✅ %s • %s • concluído", label, r.Server.Host)
		} else {
			errMsg := r.Error
			if errMsg == "" {
				errMsg = r.Resp.Error
			}
			if errMsg == "" {
				errMsg = "erro"
			}
			statuses[pos] = fmt.Sprintf("🚫 %s • %s • %s", label, r.Server.Host, errMsg)
		}
		_ = b.sendOrEdit(ctx, chatID, msgID, render(idx, okCount), InlineKeyboardMarkup{}, "flow")
	})
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao sincronizar: "+err.Error(), serversKeyboard(), "flow")
	}
	if len(results) == 0 {
		return b.sendOrEdit(ctx, chatID, msgID, render(0, 0), serversKeyboard(), "flow")
	}
	// Sincronização de servidores aplica contas/configuração nas VPS.
	// Depois sincroniza silenciosamente o vpn.primecel.shop com os IPs ativos do bot.
	b.ensureDefaultCloudflareForActiveServers(ctx)
	return b.showServers(ctx, actor, chatID, msgID)
}

func (b *Bot) activeServerIPsForCloudflare(ctx context.Context) []string {
	// O domínio padrão vpn.primecel.shop deve receber somente os IPs das VPS secundárias cadastradas no bot.
	var ips []string
	servers, _ := b.svc.Store.ListServers(ctx)
	for _, srv := range servers {
		if srv.Enabled && strings.TrimSpace(srv.Host) != "" {
			ips = append(ips, strings.TrimSpace(srv.Host))
		}
	}
	return uniqueStrings(ips)
}

func (b *Bot) ensureDefaultCloudflareForActiveServers(ctx context.Context) {
	if strings.TrimSpace(b.cloudflareToken(ctx)) == "" {
		return
	}
	ips := b.activeServerIPsForCloudflare(ctx)
	cfCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Mantém vpn.primecel.shop sincronizado com os servidores ativos do bot:
	// adiciona IP novo, mantém IP existente e remove IP antigo.
	_, _ = cloudflare.NewManager(b.svc.Config, b.svc.Store).SyncServerDNSIPs(cfCtx, ips, false)
}

func (b *Bot) syncDefaultCloudflareDomain(ctx context.Context, results []remotesync.ServerResult) string {
	// Compatibilidade com chamadas antigas: sincroniza vpn.primecel.shop com os IPs ativos do bot.
	_ = results
	if strings.TrimSpace(b.cloudflareToken(ctx)) == "" {
		return "☁️ Cloudflare vpn.primecel.shop: ⚠️ token ausente"
	}
	ips := b.activeServerIPsForCloudflare(ctx)
	mgr := cloudflare.NewManager(b.svc.Config, b.svc.Store)
	rep, err := mgr.SyncServerDNSIPs(ctx, ips, false)
	if err != nil {
		return "☁️ Cloudflare vpn.primecel.shop: ⚠️ " + err.Error()
	}
	return fmt.Sprintf("☁️ Cloudflare vpn.primecel.shop: ✅ sincronizado | IPs: %d | +%d | mantidos: %d | removidos: %d", len(ips), rep.Created, rep.Kept, rep.Deleted)
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func (b *Bot) showServerSyncLogs(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showServerSyncLogsPage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) showServerSyncLogsPage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	rows, err := b.svc.Store.Query(ctx, `SELECT username,event_type,data_json,created_at FROM account_events WHERE event_type LIKE 'remote_sync_%' ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao listar logs: "+err.Error(), serversKeyboard(), "flow")
	}
	page, pages, start, end := paginateBounds(len(rows), page, listPageSize)
	var sb strings.Builder
	sb.WriteString("📜 Logs de Sincronização\n━━━━━━━━━━━━━━\n")
	if len(rows) == 0 {
		sb.WriteString("Nenhum log de sincronização encontrado.\n")
	} else {
		for _, r := range rows[start:end] {
			ok := "⚠️"
			if strings.Contains(r["data_json"], `"ok":1`) || strings.Contains(r["data_json"], `"ok":true`) {
				ok = "✅"
			}
			out := extractJSONFieldText(r["data_json"], "output")
			if out == "" {
				out = r["data_json"]
			}
			fmt.Fprintf(&sb, "%s <code>%s</code> | %s | %s\n", ok, h(r["created_at"]), h(r["username"]), h(tailText(out, 160)))
		}
	}
	sb.WriteString("━━━━━━━━━━━━━━")
	sb.WriteString(pageIndicator(page, pages))
	return b.sendOrEdit(ctx, chatID, msgID, sb.String(), pagedListKeyboard("servers_sync_logs_page", page, pages, "menu_servers"), "submenu")
}

func extractJSONFieldText(raw, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(m[key]))
}

func (b *Bot) serverStatusIcon(ctx context.Context, srv model.Server) string {
	if b.probeServerHealth(ctx, srv) == nil {
		return "✅"
	}
	return "❌"
}
func (b *Bot) serverStatusText(ctx context.Context, srv model.Server) string {
	if err := b.probeServerHealth(ctx, srv); err != nil {
		return "❌ Offline"
	}
	return "✅ Online"
}
func (b *Bot) probeServerHealth(ctx context.Context, srv model.Server) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://%s:%d/health", srv.Host, nonZero(srv.AgentPort, b.svc.Config.RemoteAgentPort, 8787))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(srv.AgentToken)
	if token == "" {
		token = b.svc.Config.RemoteAgentToken
	}
	if token != "" {
		req.Header.Set("X-Primecel-Agent-Token", token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}

func serverDisplayName(ctx context.Context, st *store.DB, srv model.Server) string {
	servers, _ := st.ListServers(ctx)
	for i, s := range servers {
		if s.ID == srv.ID {
			return fmt.Sprintf("Sv%d", i+1)
		}
	}
	if strings.TrimSpace(srv.Name) != "" {
		return srv.Name
	}
	return "Sv"
}

func displayHostPlain(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return host
	}
	// Telegram transforma IPs/domínios em links automaticamente.
	// O zero-width space mantém a aparência normal, mas impede o autolink.
	replacer := strings.NewReplacer(".", ".\u200b", ":", ":\u200b", "/", "/\u200b")
	return replacer.Replace(host)
}

func maskSecret(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return "não cadastrada"
	}
	if len(t) <= 4 {
		return "****"
	}
	return t[:2] + "***" + t[len(t)-2:]
}

func maskToken(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return "padrão/config.env"
	}
	if len(t) <= 4 {
		return "****"
	}
	return t[:2] + "***" + t[len(t)-2:]
}

func serversKeyboard() InlineKeyboardMarkup {
	return kb(
		[]Button{{"⬅️ Voltar", "menu_home"}},
		[]Button{{"➕ Adicionar", "servers_add"}, {"✏️ Editar", "servers_edit"}},
		[]Button{{"🔄 Sincronizar", "servers_sync"}, {"🔁 Reiniciar VPS", "servers_restart"}},
	)
}
func serversEditKeyboard(servers []model.Server) InlineKeyboardMarkup {
	return serversEditPagedKeyboard(servers, 0, minInt(len(servers), listPageSize), 0, maxInt((len(servers)+listPageSize-1)/listPageSize, 1))
}

func serversEditPagedKeyboard(servers []model.Server, start, end, page, pages int) InlineKeyboardMarkup {
	rows := [][]Button{}
	for i := start; i < end && i < len(servers); i++ {
		srv := servers[i]
		rows = append(rows, []Button{{fmt.Sprintf("Sv%d | %s", i+1, displayHostPlain(srv.Host)), fmt.Sprintf("server_view:%d", srv.ID)}})
	}
	if row := paginationRow("servers_edit_page", page, pages); len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []Button{{"⬅️ Voltar", "menu_servers"}})
	return kb(rows...)
}

func serverPanelKeyboard(srv model.Server) InlineKeyboardMarkup {
	return kb([]Button{{"🌐 Mudar IP", fmt.Sprintf("server_edit_ip:%d", srv.ID)}, {"🔐 Mudar Senha", fmt.Sprintf("server_edit_token:%d", srv.ID)}}, []Button{{"🗑️ Remover Servidor", fmt.Sprintf("server_remove_confirm:%d", srv.ID)}}, []Button{{"🔄 Sincronizar", "servers_sync"}}, []Button{{"⬅️ Voltar", "servers_edit"}})
}
func serverRemoveKeyboard(id int64) InlineKeyboardMarkup {
	return kb([]Button{{"⬅️ Voltar", fmt.Sprintf("server_view:%d", id)}}, []Button{{"✅ Confirmar", fmt.Sprintf("server_do_remove:%d", id)}})
}

func serversRestartConfirmKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"⬅️ Cancelar", "menu_servers"}}, []Button{{"✅ Confirmar", "servers_do_restart"}})
}

func (b *Bot) sendOrEdit(ctx context.Context, chatID int64, msgID int, text string, kb InlineKeyboardMarkup, kind string) error {
	if chatID == 0 {
		return nil
	}
	if msgID == 0 {
		msgID, _ = b.lastEditableMessageID(ctx, chatID)
	}
	if msgID != 0 {
		if err := b.client.EditMessageText(ctx, chatID, msgID, text, kb); err == nil {
			_ = b.trackMessage(ctx, chatID, msgID, kind, text)
			_ = b.cleanupTrackedMessages(ctx, chatID, msgID)
			return nil
		} else if strings.Contains(strings.ToLower(err.Error()), "message is not modified") {
			_ = b.trackMessage(ctx, chatID, msgID, kind, text)
			_ = b.cleanupTrackedMessages(ctx, chatID, msgID)
			return nil
		}
	}
	sent, err := b.client.SendMessage(ctx, chatID, text, kb)
	if err != nil {
		return err
	}
	_ = b.trackMessage(ctx, chatID, sent.MessageID, kind, text)
	_ = b.cleanupTrackedMessages(ctx, chatID, sent.MessageID)
	return nil
}

func (b *Bot) lastEditableMessageID(ctx context.Context, chatID int64) (int, error) {
	rows, err := b.svc.Store.Query(ctx, `SELECT message_id FROM telegram_chat_messages WHERE chat_id=? AND protected=0 AND kind NOT IN ('backup','app_document') ORDER BY updated_at DESC LIMIT 1`, chatID)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	id, _ := strconv.Atoi(rows[0]["message_id"])
	return id, nil
}

func (b *Bot) cleanupTrackedMessages(ctx context.Context, chatID int64, keepMsgID int) error {
	rows, err := b.svc.Store.Query(ctx, `SELECT message_id,kind FROM telegram_chat_messages WHERE chat_id=? AND protected=0 AND message_id<>? ORDER BY updated_at DESC LIMIT 80`, chatID, keepMsgID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		kind := r["kind"]
		if kind == "backup" || kind == "app_document" {
			continue
		}
		id, _ := strconv.Atoi(r["message_id"])
		if id > 0 {
			_ = b.client.DeleteMessage(ctx, chatID, id)
			_ = b.svc.Store.Exec(ctx, `DELETE FROM telegram_chat_messages WHERE chat_id=? AND message_id=?`, chatID, id)
		}
	}
	return nil
}

func (b *Bot) trackMessage(ctx context.Context, chatID int64, msgID int, kind, text string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return b.svc.Store.Exec(ctx, `INSERT INTO telegram_chat_messages(chat_id,message_id,kind,protected,last_text,created_at,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(chat_id,message_id) DO UPDATE SET kind=excluded.kind,last_text=excluded.last_text,updated_at=excluded.updated_at`, chatID, msgID, kind, false, text, now, now)
}

func (b *Bot) trackProtectedMessage(ctx context.Context, chatID int64, msgID int, kind, text string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return b.svc.Store.Exec(ctx, `INSERT INTO telegram_chat_messages(chat_id,message_id,kind,protected,last_text,created_at,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(chat_id,message_id) DO UPDATE SET kind=excluded.kind,protected=excluded.protected,last_text=excluded.last_text,updated_at=excluded.updated_at`, chatID, msgID, kind, true, text, now, now)
}

func (b *Bot) markChatActivity(ctx context.Context, chatID int64, userID int64) error {
	if chatID == 0 || userID == 0 || b == nil || b.svc.Store == nil {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return b.svc.Store.Exec(ctx, `INSERT INTO telegram_chat_activity(chat_id,user_id,last_activity_at,updated_at) VALUES(?,?,?,?) ON CONFLICT(chat_id) DO UPDATE SET user_id=excluded.user_id,last_activity_at=excluded.last_activity_at,updated_at=excluded.updated_at`, chatID, userID, now, now)
}

func (b *Bot) markAutoHome(ctx context.Context, chatID int64, userID int64) error {
	if chatID == 0 || userID == 0 || b == nil || b.svc.Store == nil {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return b.svc.Store.Exec(ctx, `INSERT INTO telegram_chat_activity(chat_id,user_id,last_activity_at,auto_home_at,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(chat_id) DO UPDATE SET user_id=excluded.user_id,last_activity_at=excluded.last_activity_at,auto_home_at=excluded.auto_home_at,updated_at=excluded.updated_at`, chatID, userID, now, now, now)
}

func (b *Bot) idleHomeLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	for {
		b.returnIdleChatsHome(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bot) returnIdleChatsHome(ctx context.Context) {
	if b == nil || b.svc.Store == nil {
		return
	}
	cutoff := time.Now().UTC().Add(-idleHomeAfter).Format(time.RFC3339)
	rows, err := b.svc.Store.Query(ctx, `SELECT chat_id,user_id,last_activity_at FROM telegram_chat_activity WHERE last_activity_at<? ORDER BY last_activity_at ASC LIMIT 100`, cutoff)
	if err != nil || len(rows) == 0 {
		return
	}
	for _, r := range rows {
		chatID, _ := strconv.ParseInt(r["chat_id"], 10, 64)
		userID, _ := strconv.ParseInt(r["user_id"], 10, 64)
		if chatID == 0 || userID == 0 {
			continue
		}
		msgRows, _ := b.svc.Store.Query(ctx, `SELECT message_id,kind,last_text FROM telegram_chat_messages WHERE chat_id=? AND protected=0 AND kind NOT IN ('backup','app_document') ORDER BY updated_at DESC LIMIT 1`, chatID)
		if len(msgRows) == 0 {
			_ = b.markAutoHome(ctx, chatID, userID)
			continue
		}
		kind := strings.TrimSpace(msgRows[0]["kind"])
		lastText := msgRows[0]["last_text"]
		if kind == "menu" || kind == "menu_status" || kind == "menu_cmd" {
			_ = b.markAutoHome(ctx, chatID, userID)
			continue
		}
		if b.shouldSkipIdleHome(ctx, userID, kind, lastText) {
			continue
		}
		msgID, _ := strconv.Atoi(msgRows[0]["message_id"])
		actor := b.resolveActor(ctx, userID, "")
		_ = b.clearFlow(ctx, userID)
		if err := b.showMain(ctx, actor, chatID, msgID); err != nil {
			_ = b.svc.Store.Exec(ctx, `DELETE FROM telegram_chat_messages WHERE chat_id=? AND message_id=?`, chatID, msgID)
			_ = b.showMain(ctx, actor, chatID, 0)
		}
		_ = b.markAutoHome(ctx, chatID, userID)
	}
}

func settingBool(ctx context.Context, st *store.DB, key string) bool {
	v, _ := st.GetSetting(ctx, key)
	v = strings.TrimSpace(strings.ToLower(v))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func PaymentRenewalNoticeSettingKey(ownerID int64) string {
	return fmt.Sprintf("payment_renewal_notice_enabled_%d", ownerID)
}

func PaymentRenewalWatchSettingKey(ownerID int64, watcherID int64) string {
	return fmt.Sprintf("payment_renewal_watch_owner_%d_watcher_%d", ownerID, watcherID)
}

func PaymentRenewalWatchEnabled(ctx context.Context, st *store.DB, ownerID int64, watcherID int64) bool {
	if st == nil || ownerID == 0 || watcherID == 0 {
		return false
	}
	v, err := st.GetSetting(ctx, PaymentRenewalWatchSettingKey(ownerID, watcherID))
	if err != nil || strings.TrimSpace(v) == "" {
		return false
	}
	return parseSettingBool(v)
}

func PaymentRenewalWatchers(ctx context.Context, st *store.DB, ownerID int64) []int64 {
	if st == nil || ownerID == 0 {
		return nil
	}
	prefix := fmt.Sprintf("payment_renewal_watch_owner_%d_watcher_", ownerID)
	rows, err := st.Query(ctx, `SELECT key,value FROM settings WHERE key LIKE ?`, prefix+"%")
	if err != nil || len(rows) == 0 {
		return nil
	}
	seen := map[int64]bool{}
	var out []int64
	for _, r := range rows {
		if !parseSettingBool(r["value"]) {
			continue
		}
		idText := strings.TrimPrefix(r["key"], prefix)
		id, err := strconv.ParseInt(strings.TrimSpace(idText), 10, 64)
		if err != nil || id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func PaymentRenewalNoticeEnabled(ctx context.Context, st *store.DB, ownerID int64) bool {
	if st == nil {
		return true
	}
	v, err := st.GetSetting(ctx, PaymentRenewalNoticeSettingKey(ownerID))
	if err != nil || strings.TrimSpace(v) == "" {
		return true
	}
	return parseSettingBool(v)
}

func (b *Bot) currentPaymentRenewalNoticeEnabled(ctx context.Context, ownerID int64) bool {
	if b == nil || b.svc.Store == nil {
		return true
	}
	return PaymentRenewalNoticeEnabled(ctx, b.svc.Store, ownerID)
}

func (b *Bot) togglePaymentRenewalNotice(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !paymentsManageAllowed(ctx, b.svc.Store, actor) {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin, revenda ou subrevenda.", backKeyboard(), "flow")
	}
	ownerID := paymentOwnerID(actor)
	enabled := !b.currentPaymentRenewalNoticeEnabled(ctx, ownerID)
	if err := b.svc.Store.SetSetting(ctx, PaymentRenewalNoticeSettingKey(ownerID), boolText(enabled)); err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro: "+err.Error(), backKeyboard(), "flow")
	}
	return b.showPayments(ctx, actor, chatID, msgID)
}

func (b *Bot) toggleMyResellerRenewalNotice(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if actor.IsAdmin || actor.TelegramID == 0 || (actor.Role != model.RoleReseller && actor.Role != model.RoleSubReseller) {
		return b.showMain(ctx, actor, chatID, msgID)
	}
	enabled := !b.currentPaymentRenewalNoticeEnabled(ctx, actor.TelegramID)
	if err := b.svc.Store.SetSetting(ctx, PaymentRenewalNoticeSettingKey(actor.TelegramID), boolText(enabled)); err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro: "+err.Error(), backKeyboard(), "flow")
	}
	return b.showMyReseller(ctx, actor, chatID, msgID)
}

func (b *Bot) showResellerRenewalNoticePrompt(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	r, err := b.svc.Store.FindReseller(ctx, id)
	if err != nil || r == nil || !resellerVisible(actor, *r) {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Revenda/SubRevenda não encontrada ou sem permissão.", backKeyboard(), "flow")
	}
	if actor.TelegramID == 0 {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Não foi possível identificar seu Telegram para salvar este aviso.", resellerPanelKeyboard(actor, *r), "flow")
	}
	if actor.TelegramID == r.TelegramID {
		return b.showMyReseller(ctx, actor, chatID, msgID)
	}
	current := PaymentRenewalWatchEnabled(ctx, b.svc.Store, r.TelegramID, actor.TelegramID)
	status := "🔴 OFF"
	if current {
		status = "🟢 ON"
	}
	kind := "Revenda"
	if r.Level == 1 || r.ParentTelegramID != 0 {
		kind = "SubRevenda"
	}
	text := fmt.Sprintf("🔔 Avisos de renovação\n━━━━━━━━━━━━━━\n%s: <b>%s</b>\nID: <code>%d</code>\nStatus atual: <b>%s</b>\n\nDeseja receber aviso quando uma conta desta %s for renovada?", h(kind), h(r.Name), r.TelegramID, h(status), h(kind))
	return b.sendOrEdit(ctx, chatID, msgID, text, resellerRenewalNoticeKeyboard(r.TelegramID), "flow")
}

func (b *Bot) setResellerRenewalNoticeWatch(ctx context.Context, actor model.Actor, chatID int64, msgID int, payload string) error {
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Opção inválida.", backKeyboard(), "flow")
	}
	id, _ := strconv.ParseInt(parts[0], 10, 64)
	enabled := parts[1] == "1"
	r, err := b.svc.Store.FindReseller(ctx, id)
	if err != nil || r == nil || !resellerVisible(actor, *r) {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Revenda/SubRevenda não encontrada ou sem permissão.", backKeyboard(), "flow")
	}
	if actor.TelegramID == 0 {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Não foi possível identificar seu Telegram para salvar este aviso.", resellerPanelKeyboard(actor, *r), "flow")
	}
	if actor.TelegramID == r.TelegramID {
		if err := b.svc.Store.SetSetting(ctx, PaymentRenewalNoticeSettingKey(actor.TelegramID), boolText(enabled)); err != nil {
			return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro: "+err.Error(), backKeyboard(), "flow")
		}
		return b.showMyReseller(ctx, actor, chatID, msgID)
	}
	if err := b.svc.Store.SetSetting(ctx, PaymentRenewalWatchSettingKey(r.TelegramID, actor.TelegramID), boolText(enabled)); err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro: "+err.Error(), backKeyboard(), "flow")
	}
	status := "desativados"
	if enabled {
		status = "ativados"
	}
	text := fmt.Sprintf("✅ Avisos de renovação %s para <b>%s</b>.", h(status), h(r.Name))
	return b.sendOrEdit(ctx, chatID, msgID, text+"\n\n"+resellerPanelText(*r, b.svc.Resellers.BlockReason(ctx, r)), resellerPanelKeyboard(actor, *r), "flow")
}

func SendMainMenuBelowNotice(ctx context.Context, cfg config.Config, st *store.DB, chatID int64, userID int64, version string) error {
	if strings.TrimSpace(cfg.BotToken) == "" || st == nil || chatID == 0 {
		return nil
	}
	if userID == 0 {
		userID = chatID
		rows, _ := st.Query(ctx, `SELECT user_id FROM telegram_chat_activity WHERE chat_id=? ORDER BY updated_at DESC LIMIT 1`, chatID)
		if len(rows) > 0 {
			if parsed, err := strconv.ParseInt(strings.TrimSpace(rows[0]["user_id"]), 10, 64); err == nil && parsed != 0 {
				userID = parsed
			}
		}
	}
	b := NewBot(Services{Config: cfg, Store: st, Online: online.NewManager(cfg, st), Version: version})
	actor := b.resolveActor(ctx, userID, "")
	text, err := b.buildMainPanel(ctx, actor)
	if err != nil {
		return err
	}
	msg, err := b.client.SendMessage(ctx, chatID, text, mainKeyboard(actor))
	if err != nil {
		return err
	}
	if msg != nil && msg.MessageID != 0 {
		_ = b.trackMessage(ctx, chatID, msg.MessageID, "menu", text)
		_ = b.cleanupTrackedMessages(ctx, chatID, msg.MessageID)
	}
	return nil
}

func (b *Bot) sendMainMenuBelowNotice(ctx context.Context, chatID int64) {
	if b == nil || b.svc.Store == nil || b.client == nil || chatID == 0 {
		return
	}
	userID := chatID
	rows, _ := b.svc.Store.Query(ctx, `SELECT user_id FROM telegram_chat_activity WHERE chat_id=? ORDER BY updated_at DESC LIMIT 1`, chatID)
	if len(rows) > 0 {
		if parsed, err := strconv.ParseInt(strings.TrimSpace(rows[0]["user_id"]), 10, 64); err == nil && parsed != 0 {
			userID = parsed
		}
	}
	actor := b.resolveActor(ctx, userID, "")
	text, err := b.buildMainPanel(ctx, actor)
	if err != nil {
		return
	}
	msg, err := b.client.SendMessage(ctx, chatID, text, mainKeyboard(actor))
	if err != nil || msg == nil || msg.MessageID == 0 {
		return
	}
	_ = b.trackMessage(ctx, chatID, msg.MessageID, "menu", text)
	_ = b.cleanupTrackedMessages(ctx, chatID, msg.MessageID)
}

func settingInt(ctx context.Context, st *store.DB, key string, def int) int {
	v, _ := st.GetSetting(ctx, key)
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func (b *Bot) registerSystemUpdateNotice(ctx context.Context) error {
	current := strings.TrimSpace(b.svc.Version)
	if current == "" {
		return nil
	}
	previous, _ := b.svc.Store.GetSetting(ctx, "system_last_version")
	previous = strings.TrimSpace(previous)
	if previous == current {
		return nil
	}
	until := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339)
	if err := b.svc.Store.SetSetting(ctx, "system_update_notice_until", until); err != nil {
		return err
	}
	return b.svc.Store.SetSetting(ctx, "system_last_version", current)
}

func (b *Bot) shouldShowSystemUpdateNotice(ctx context.Context) bool {
	v, _ := b.svc.Store.GetSetting(ctx, "system_update_notice_until")
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return false
	}
	return time.Now().UTC().Before(until)
}

func humanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	for _, u := range units {
		v = v / 1024
		if v < 1024 {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/1024)
}

func (b *Bot) automaticBackupLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	select {
	case <-ctx.Done():
		return
	case <-time.After(25 * time.Second):
	}
	for {
		b.runAutomaticBackupIfDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bot) runAutomaticBackupIfDue(ctx context.Context) {
	if !settingBool(ctx, b.svc.Store, "backup_auto_enabled") {
		return
	}
	hours := settingInt(ctx, b.svc.Store, "backup_auto_interval_hours", 12)
	if hours <= 0 {
		hours = 12
	}
	last, _ := b.svc.Store.GetSetting(ctx, "backup_auto_last_at")
	if strings.TrimSpace(last) != "" {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(last)); err == nil && time.Since(t) < time.Duration(hours)*time.Hour {
			return
		}
	}
	rep, err := b.backupManager().Create(ctx, backup.CreateOptions{})
	if err != nil {
		_ = b.svc.Store.SetSetting(ctx, "backup_auto_last_error", err.Error())
		return
	}
	caption := fmt.Sprintf("💾 Backup automático %dh\n\nArquivos: %d\nTamanho: %s", hours, len(rep.Files), humanSize(rep.SizeBytes))
	mode, _ := b.svc.Store.GetSetting(ctx, "backup_destination_mode")
	sentCount := 0
	var sendErrors []string
	if strings.TrimSpace(mode) == "other_bot" {
		token, _ := b.svc.Store.GetSetting(ctx, "backup_remote_bot_token")
		if strings.TrimSpace(token) != "" {
			other := NewClient(strings.TrimSpace(token))
			for _, adminID := range b.svc.Config.AdminIDs {
				if _, err := other.SendDocument(ctx, adminID, rep.Path, "", caption, filepath.Base(rep.Path)); err != nil {
					sendErrors = append(sendErrors, err.Error())
				} else {
					sentCount++
				}
			}
		} else {
			sendErrors = append(sendErrors, "token do outro bot não configurado")
		}
	} else {
		for _, adminID := range b.svc.Config.AdminIDs {
			if _, err := b.client.SendDocument(ctx, adminID, rep.Path, "", caption, filepath.Base(rep.Path)); err != nil {
				sendErrors = append(sendErrors, err.Error())
			} else {
				sentCount++
			}
		}
	}
	if sentCount > 0 {
		b.removeGeneratedBackupFile(ctx, rep.Path)
	}
	if len(sendErrors) > 0 {
		_ = b.svc.Store.SetSetting(ctx, "backup_auto_last_error", strings.Join(sendErrors, " | "))
	} else {
		_ = b.svc.Store.SetSetting(ctx, "backup_auto_last_error", "")
	}
	_ = b.svc.Store.SetSetting(ctx, "backup_auto_last_at", time.Now().UTC().Format(time.RFC3339))
}
func (b *Bot) liveRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !b.liveRefreshMu.TryLock() {
				// Atualização anterior ainda rodando: pula esta rodada para evitar fila,
				// lentidão e consultas sobrepostas nos servidores secundários.
				continue
			}
			func() {
				defer b.liveRefreshMu.Unlock()
				rctx, cancel := context.WithTimeout(ctx, 2800*time.Millisecond)
				defer cancel()
				_ = b.refreshLivePanels(rctx)
			}()
		}
	}
}

func liveKindPage(kind, prefix string) (int, bool) {
	if kind == prefix {
		return 0, true
	}
	if !strings.HasPrefix(kind, prefix+":") {
		return 0, false
	}
	page, _ := strconv.Atoi(strings.TrimPrefix(kind, prefix+":"))
	if page < 0 {
		page = 0
	}
	return page, true
}

func (b *Bot) livePayloadForKind(ctx context.Context, actor model.Actor, kind string) (string, InlineKeyboardMarkup, string) {
	switch kind {
	case "menu", "menu_status", "menu_cmd":
		text, _ := b.buildMainPanel(ctx, actor)
		return text, mainKeyboard(actor), "menu"
	case "live_accounts":
		text, _ := b.accountsPanelText(ctx, actor)
		return text, accountsKeyboard(actor), "live_accounts"
	case "live_resellers_admin", "live_subresellers":
		text, _ := b.resellersPanelText(ctx, actor)
		return text, resellersKeyboard(actor), kind
	}
	if page, ok := liveKindPage(kind, "live_online"); ok {
		text, kb := b.onlinePagePayload(ctx, actor, page)
		return text, kb, fmt.Sprintf("live_online:%d", page)
	}
	if page, ok := liveKindPage(kind, "live_accounts_list"); ok {
		text, kb := b.accountsListPagePayload(ctx, actor, page)
		return text, kb, fmt.Sprintf("live_accounts_list:%d", page)
	}
	if page, ok := liveKindPage(kind, "live_expired"); ok {
		text, kb := b.expiredListPagePayload(ctx, actor, page)
		return text, kb, fmt.Sprintf("live_expired:%d", page)
	}
	if kind == "live_resellers" {
		text, kb := b.resellerListPagePayload(ctx, actor, 0)
		return text, kb, "live_resellers_list:0"
	}
	if page, ok := liveKindPage(kind, "live_resellers_list"); ok {
		text, kb := b.resellerListPagePayload(ctx, actor, page)
		return text, kb, fmt.Sprintf("live_resellers_list:%d", page)
	}
	return "", InlineKeyboardMarkup{}, kind
}

func (b *Bot) refreshLivePanels(ctx context.Context) error {
	rows, err := b.svc.Store.Query(ctx, `SELECT chat_id,message_id,kind,last_text FROM telegram_chat_messages WHERE protected=0 AND (kind IN ('menu','menu_status','menu_cmd','live_accounts','live_resellers_admin','live_subresellers','live_online','live_expired','live_resellers') OR kind LIKE 'live_online:%' OR kind LIKE 'live_accounts_list:%' OR kind LIKE 'live_expired:%' OR kind LIKE 'live_resellers_list:%') ORDER BY updated_at DESC LIMIT 500`)
	if err != nil {
		return err
	}
	payloadCache := map[string]struct {
		Text string
		KB   InlineKeyboardMarkup
		Kind string
	}{}
	for _, r := range rows {
		chatID, _ := strconv.ParseInt(r["chat_id"], 10, 64)
		msgID, _ := strconv.Atoi(r["message_id"])
		if chatID == 0 || msgID == 0 {
			continue
		}
		actor := b.resolveActor(ctx, chatID, "")
		cacheKey := fmt.Sprintf("%d:%s", chatID, r["kind"])
		payload, ok := payloadCache[cacheKey]
		if !ok {
			text, kb, normalizedKind := b.livePayloadForKind(ctx, actor, r["kind"])
			payload = struct {
				Text string
				KB   InlineKeyboardMarkup
				Kind string
			}{Text: text, KB: kb, Kind: normalizedKind}
			payloadCache[cacheKey] = payload
		}
		if strings.TrimSpace(payload.Text) == "" || payload.Text == r["last_text"] {
			continue
		}
		if err := b.client.EditMessageText(ctx, chatID, msgID, payload.Text, payload.KB); err == nil || strings.Contains(strings.ToLower(fmt.Sprint(err)), "message is not modified") {
			_ = b.trackMessage(ctx, chatID, msgID, payload.Kind, payload.Text)
		} else {
			low := strings.ToLower(err.Error())
			if strings.Contains(low, "message to edit not found") || strings.Contains(low, "message can't be edited") {
				_ = b.svc.Store.Exec(ctx, `DELETE FROM telegram_chat_messages WHERE chat_id=? AND message_id=?`, chatID, msgID)
			}
		}
	}
	return nil
}

func commandsKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"⬅️ Voltar", "menu_home"}})
}

func mainKeyboard(actor model.Actor) InlineKeyboardMarkup {
	if actor.IsAdmin {
		return kb([]Button{{"👤 Contas", "menu_accounts"}, {"👥 Revendas", "menu_resellers"}}, []Button{{"🌐 Servidores", "menu_servers"}, {"📱 Aplicativo", "menu_apps"}}, []Button{{"💳 Pagamentos", "menu_payments"}, {"📣 Avisos", "menu_notices"}}, []Button{{"📱 Limpar Aparelhos", "menu_devices"}, {"⚙️ Administração", "menu_admin"}})
	}
	if actor.Role == model.RoleReseller {
		return kb([]Button{{"👤 Contas", "menu_accounts"}, {"👥 SubRevendas", "menu_revender"}}, []Button{{"📱 Aplicativo", "menu_apps"}, {"💼 Minha Revenda", "menu_myreseller"}}, []Button{{"💳 Pagamentos", "menu_payments"}, {"📱 Limpar Aparelhos", "menu_devices"}})
	}
	return kb([]Button{{"👤 Contas", "menu_accounts"}, {"📱 Aplicativo", "menu_apps"}}, []Button{{"💼 Minha Revenda", "menu_myreseller"}, {"💳 Pagamentos", "menu_payments"}}, []Button{{"📱 Limpar Aparelhos", "menu_devices"}})
}

func accountsKeyboard(actor model.Actor) InlineKeyboardMarkup {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return kb([]Button{{"➕ Criar", "accounts_create"}, {"🧪 Criar Teste", "accounts_trial"}}, []Button{{"✏️ Editar Conta", "accounts_edit"}, {"📋 Listar Contas", "accounts_list"}}, []Button{{"🟢 Listar Onlines", "menu_online"}, {"🚫 Expirados", "accounts_expired"}}, []Button{{"📱 Limpar Aparelhos", "menu_devices"}}, []Button{{"⏳ Liberar Dias", "accounts_release_days"}, {"🗑️ Remover Todos", "acct_remove_all"}}, []Button{{"⬅️ Voltar", "menu_home"}})
	}
	return kb([]Button{{"➕ Criar", "accounts_create"}, {"🧪 Criar Teste", "accounts_trial"}}, []Button{{"✏️ Editar Conta", "accounts_edit"}, {"📋 Listar Contas", "accounts_list"}}, []Button{{"🟢 Listar Onlines", "menu_online"}, {"🚫 Expirados", "accounts_expired"}}, []Button{{"📱 Limpar Aparelhos", "menu_devices"}}, []Button{{"⬅️ Voltar", "menu_home"}})
}

func resellersKeyboard(actor model.Actor) InlineKeyboardMarkup {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return kb([]Button{{"➕ Criar Revenda", "reseller_create"}, {"✏️ Editar Revenda", "reseller_edit"}}, []Button{{"🚫 Expiradas", "reseller_block"}, {"📋 Listar Revendas", "reseller_list"}}, []Button{{"⬅️ Voltar", "menu_home"}})
	}
	return kb([]Button{{"➕ Criar SubRevenda", "reseller_create"}, {"✏️ Editar SubRevenda", "reseller_edit"}}, []Button{{"🚫 Expiradas", "reseller_block"}, {"📋 Listar SubRevendas", "reseller_list"}}, []Button{{"⬅️ Voltar", "menu_home"}})
}

func appsKeyboard(actor model.Actor) InlineKeyboardMarkup {
	if actor.IsAdmin {
		return kb([]Button{{"Importar", "app_import"}, {"Baixar", "app_download_latest"}}, []Button{{"⬅️ Voltar", "menu_home"}})
	}
	return kb([]Button{{"Baixar", "app_download_latest"}}, []Button{{"⬅️ Voltar", "menu_home"}})
}
func profileKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"👑 Alterar Nome", "profile_name"}}, []Button{{"📱 Alterar WhatsApp", "profile_whatsapp"}}, []Button{{"⬅️ Voltar", "menu_home"}})
}

func settingsKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"⬅️ Voltar", "menu_admin"}}, []Button{{"Token Cloudflare", "settings_cf_token"}})
}
func cloudflareKeyboard() InlineKeyboardMarkup {
	return serversKeyboard()
}

func devicesKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"⬅️ Voltar", "menu_home"}}, []Button{{"📱 Limpar Aparelhos", "devices_clear_all"}})
}

func backupKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"⬅️ Voltar", "menu_home"}}, []Button{{"📥 Importar", "backup_import"}, {"📤 Exportar", "backup_export"}}, []Button{{"📍 Destino", "backup_destination"}, {"⏱️ Automático", "backup_auto_menu"}})
}
func paymentsKeyboard(ownerID int64, enabled bool, isAdmin bool, renewalNoticeOpts ...bool) InlineKeyboardMarkup {
	label := "✅ Ativar"
	if enabled {
		label = "🚫 Desativar"
	}
	renewalNotice := true
	if len(renewalNoticeOpts) > 0 {
		renewalNotice = renewalNoticeOpts[0]
	}
	noticeLabel := "🔔 Receber aviso [OFF]"
	if renewalNotice {
		noticeLabel = "🔔 Receber aviso [ON]"
	}
	rows := [][]Button{
		{{"⚙️ Configurar", "payments_config"}, {label, "payments_toggle"}},
		{{noticeLabel, "payments_renewal_notice_toggle"}},
		{{"📊 Relatório", "payments_report"}, {"📘 Tutorial", "payments_tutorial"}},
	}
	if isAdmin {
		rows = append(rows, []Button{{"📶 Testar API", "payments_webhook_test"}})
	}
	rows = append(rows, []Button{{"⬅️ Voltar", "menu_home"}})
	return kb(rows...)
}

func paymentsConfigKeyboard(actorOpts ...model.Actor) InlineKeyboardMarkup {
	rows := [][]Button{
		{{"💳 InfinitePay", "payments_bank_infinitepay"}},
		{{"🏦 Asaas", "payments_bank_asaas"}, {"💰 Mercado Pago", "payments_bank_mercado_pago"}},
	}
	showLimits := true
	if len(actorOpts) > 0 {
		a := actorOpts[0]
		showLimits = a.IsAdmin || a.Role == model.RoleAdmin
	}
	if showLimits {
		rows = append(rows, []Button{{"📅 Meses", "payment_plan_months"}, {"📦 Limites", "payment_limit_menu"}})
	} else {
		rows = append(rows, []Button{{"📅 Meses", "payment_plan_months"}})
	}
	rows = append(rows, []Button{{"⬅️ Voltar", "menu_payments"}})
	return kb(rows...)
}
func paymentsTutorialKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"1. Asaas", "payments_tutorial_asaas"}}, []Button{{"2. Mercado Pago", "payments_tutorial_mercado_pago"}}, []Button{{"3. InfinitePay", "payments_tutorial_infinitepay"}}, []Button{{"⬅️ Voltar", "menu_payments"}})
}
func noticesKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"⬅️ Voltar", "menu_home"}}, []Button{{"📢 Aviso", "notice_aviso"}, {"🆕 Novidades", "notice_novidades"}})
}

func adminKeyboard(gestorOnly bool, opts ...bool) InlineKeyboardMarkup {
	gestorLabel := "⚙️ Gestor Only [OFF]"
	if gestorOnly {
		gestorLabel = "⚙️ Gestor Only [ON]"
	}
	xrayGeneral := true
	if len(opts) > 0 {
		xrayGeneral = opts[0]
	}
	expirationNotices := true
	if len(opts) > 1 {
		expirationNotices = opts[1]
	}
	xrayLabel := "🌐 Xray Geral [OFF]"
	if xrayGeneral {
		xrayLabel = "🌐 Xray Geral [ON]"
	}
	expirationLabel := "⚠️ Avisos Expiração [OFF]"
	if expirationNotices {
		expirationLabel = "⚠️ Avisos Expiração [ON]"
	}
	return kb([]Button{{"💾 Backup", "menu_backup"}, {"📊 Status", "menu_status"}}, []Button{{gestorLabel, "settings_gestor_toggle"}, {xrayLabel, "settings_xray_general_toggle"}}, []Button{{expirationLabel, "settings_expiration_notices_toggle"}}, []Button{{"☁️ Token Cloudflare", "settings_cf_token"}, {"👤 Meu Perfil", "menu_profile"}}, []Button{{"⬅️ Voltar", "menu_home"}})
}

func backKeyboard() InlineKeyboardMarkup { return kb([]Button{{"⬅️ Voltar", "menu_home"}}) }

const listPageSize = 15

const renewalNoticeAutoDeleteAfter = 2 * time.Hour
const renewalNoticeWindow = 2 * time.Hour
const idleHomeAfter = 2 * time.Hour
const deviceCleanupResultAutoDeleteAfter = 5 * time.Second

func paginateBounds(total, page, perPage int) (int, int, int, int) {
	if perPage <= 0 {
		perPage = listPageSize
	}
	pages := 1
	if total > 0 {
		pages = (total + perPage - 1) / perPage
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return page, pages, start, end
}

func pageIndicator(page, pages int) string {
	// Só mostra paginação quando a lista realmente passa do limite da página.
	if pages <= 1 {
		return ""
	}
	return fmt.Sprintf("\nPágina: %d/%d", page+1, pages)
}

func paginationRow(prefix string, page, pages int) []Button {
	// Sem botões de paginação quando a lista cabe em uma página.
	if pages <= 1 {
		return nil
	}
	row := []Button{}
	if page > 0 {
		row = append(row, Button{"<", fmt.Sprintf("%s:%d", prefix, page-1)})
	}
	if page+1 < pages {
		row = append(row, Button{">", fmt.Sprintf("%s:%d", prefix, page+1)})
	}
	return row
}

func pagedListKeyboard(prefix string, page, pages int, back string) InlineKeyboardMarkup {
	rows := [][]Button{}
	if row := paginationRow(prefix, page, pages); len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []Button{{"⬅️ Voltar", back}})
	return kb(rows...)
}

func expiredListKeyboard(actor model.Actor, page, pages int) InlineKeyboardMarkup {
	rows := [][]Button{}
	if row := paginationRow("expired_page", page, pages); len(row) > 0 {
		rows = append(rows, row)
	}
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		rows = append(rows, []Button{{"🗑️ Limpar Expirados", "accounts_clear_expired"}})
	}
	rows = append(rows, []Button{{"⬅️ Voltar", "menu_accounts"}})
	return kb(rows...)
}

func expiredResellerListKeyboard(page, pages int) InlineKeyboardMarkup {
	rows := [][]Button{}
	if row := paginationRow("resellers_expired_page", page, pages); len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []Button{{"⬅️ Voltar", "menu_resellers"}})
	return kb(rows...)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type Button struct{ Text, Data string }

func kb(rows ...[]Button) InlineKeyboardMarkup {
	out := InlineKeyboardMarkup{}
	for _, row := range rows {
		rr := []InlineKeyboardButton{}
		for _, b := range row {
			rr = append(rr, InlineKeyboardButton{Text: b.Text, CallbackData: b.Data})
		}
		out.InlineKeyboard = append(out.InlineKeyboard, rr)
	}
	return out
}

func (b *Bot) visibleOwnerIDs(ctx context.Context, actor model.Actor) map[int64]bool {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return nil
	}
	ids := map[int64]bool{actor.TelegramID: true}
	if actor.Role == model.RoleReseller {
		rs, _ := b.svc.Store.ListResellers(ctx)
		for _, r := range rs {
			if r.ParentTelegramID == actor.TelegramID {
				ids[r.TelegramID] = true
			}
		}
	}
	return ids
}
func (b *Bot) directOwnerIDs(ctx context.Context, actor model.Actor) map[int64]bool {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return nil
	}
	return map[int64]bool{actor.TelegramID: true}
}
func accountVisible(actor model.Actor, owners map[int64]bool, a model.Account) bool {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return true
	}
	return owners[a.OwnerTelegramID]
}
func resellerVisible(actor model.Actor, r model.Reseller) bool {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return true
	}
	if actor.Role == model.RoleReseller {
		return r.TelegramID == actor.TelegramID || r.ParentTelegramID == actor.TelegramID
	}
	return r.TelegramID == actor.TelegramID
}
func onlineConnectionsTotal(items []online.Item) int {
	total := 0
	for _, it := range items {
		if it.Connections > 0 {
			total += it.Connections
		}
	}
	return total
}

func filterOnline(actor model.Actor, owners map[int64]bool, items []online.Item) []online.Item {
	out := []online.Item{}
	for _, it := range items {
		if actor.IsAdmin || actor.Role == model.RoleAdmin || owners[it.OwnerID] {
			out = append(out, it)
		}
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
func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
func daysLeft(t time.Time) string {
	if t.IsZero() {
		return "sem validade"
	}
	d := time.Until(t)
	if d < 0 {
		return "expirado"
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%02dh:%02d", h, m)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
func daysLeftLong(t time.Time) string {
	if t.IsZero() {
		return "sem validade"
	}
	d := time.Until(t)
	if d < 0 {
		return "expirado"
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%02dh:%02d", h, m)
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1 dia"
	}
	return fmt.Sprintf("%d dias", days)
}
func expiredFor(t time.Time) string {
	if t.IsZero() {
		return "sem validade"
	}
	d := time.Since(t)
	if d < 0 {
		return "0h:00"
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%02dh:%02d", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %02dh:%02d", int(d.Hours()/24), int(d.Hours())%24, int(d.Minutes())%60)
}

func (b *Bot) showApps(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showAppsPage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) showAppsPage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	list, _ := apps.NewManager(b.svc.Config, b.svc.Store).List(ctx)
	var sb strings.Builder
	sb.WriteString("⚡ <b>PRIMECEL - Aplicativo</b>\n")
	sb.WriteString("━━━━━━━━━━━━━━\n")
	if len(list) == 0 {
		if actor.IsAdmin {
			sb.WriteString("Nenhum aplicativo importado.")
		} else {
			sb.WriteString("Nenhum aplicativo disponível.")
		}
	} else {
		for _, app := range list {
			fmt.Fprintf(&sb, "🚀 %s | %s\n", h(app.Name), h(appVersion(app.Version)))
		}
	}
	return b.sendOrEdit(ctx, chatID, msgID, strings.TrimSpace(sb.String()), appsKeyboard(actor), "submenu")
}

func (b *Bot) sendLatestAppDocument(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	list, err := apps.NewManager(b.svc.Config, b.svc.Store).List(ctx)
	if err != nil || len(list) == 0 {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Nenhum aplicativo disponível.", appsKeyboard(actor), "flow")
	}
	latest := list[0]
	for _, app := range list[1:] {
		if app.UpdatedAt.After(latest.UpdatedAt) {
			latest = app
		}
	}
	return b.sendAppDocument(ctx, actor, chatID, msgID, latest.ID)
}

func (b *Bot) sendAppDocument(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	app, err := b.svc.Store.FindAppByID(ctx, id)
	if err != nil || app == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Aplicativo não encontrado.", appsKeyboard(actor), "flow")
	}
	caption := fmt.Sprintf("📲 %s | Versão %s", app.Name, nonEmptyText(app.Version, "-"))
	sent, err := b.client.SendDocument(ctx, chatID, app.Path, app.FileID, caption, app.FileName)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao enviar aplicativo: "+err.Error(), appsKeyboard(actor), "flow")
	}
	if sent != nil {
		_ = b.trackMessage(ctx, chatID, sent.MessageID, "app_document", caption)
	}
	return nil
}
func (b *Bot) startAppImport(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	_ = b.setFlow(ctx, actor, chatID, "app_import_name", map[string]string{})
	return b.sendOrEdit(ctx, chatID, msgID, "📱 Importar Aplicativo\n━━━━━━━━━━━━━━\nDigite o nome do app.", backKeyboard(), "flow")
}
func (b *Bot) showProfile(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	name, _ := b.svc.Store.GetSetting(ctx, "admin_display_name")
	if strings.TrimSpace(name) == "" {
		name = b.svc.Config.AdminDisplayName
	}
	wa, _ := b.svc.Store.GetSetting(ctx, "whatsapp_admin_numbers")
	if strings.TrimSpace(wa) == "" {
		wa = strings.Join(b.svc.Config.WhatsAppAdminNumbers, ",")
	}
	if name == "" {
		name = "Admin"
	}
	if wa == "" {
		wa = "Não definido"
	}
	text := fmt.Sprintf("🚀 <b>GESTOR PRIMECEL</b>\n━━━━━━MEU PERFIL━━━━━━\n👑 Nome: <b>%s</b>\n🆔 ID Telegram: <code>%d</code>\n📱 WhatsApp: <code>%s</code>", h(name), actor.TelegramID, h(wa))
	return b.sendOrEdit(ctx, chatID, msgID, text, profileKeyboard(), "submenu")
}

func (b *Bot) startProfileEdit(ctx context.Context, actor model.Actor, chatID int64, msgID int, field string) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	state := "profile_name"
	prompt := "Digite o novo nome do admin."
	if field == "whatsapp" {
		state = "profile_whatsapp"
		prompt = "Digite o WhatsApp com DDI. Exemplo: 5585999999999"
	}
	_ = b.setFlow(ctx, actor, chatID, state, map[string]string{})
	return b.sendOrEdit(ctx, chatID, msgID, "👤 Meu Perfil\n━━━━━━━━━━━━━━\n"+prompt, backKeyboard(), "flow")
}
func (b *Bot) showSettings(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showAdministration(ctx, actor, chatID, msgID)
}
func (b *Bot) startSettingsEdit(ctx context.Context, actor model.Actor, chatID int64, msgID int, field string) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	state := "settings_cloudflare_token"
	prompt := "Digite o token Cloudflare."
	back := kb([]Button{{"⬅️ Voltar", "menu_admin"}})
	_ = b.setFlow(ctx, actor, chatID, state, map[string]string{})
	return b.sendOrEdit(ctx, chatID, msgID, "⚙️ Administração\n━━━━━━━━━━━━━━\n"+prompt, back, "flow")
}
func (b *Bot) setGestorOnly(ctx context.Context, actor model.Actor, chatID int64, msgID int, enabled bool) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	if err := settings.NewManager(b.svc.Config, b.svc.Store).SetPrincipalManagerOnly(ctx, enabled); err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro: "+err.Error(), adminKeyboard(b.currentGestorOnly(ctx)), "flow")
	}
	b.svc.Config.PrincipalManagerOnly = enabled
	status := "desativado"
	if enabled {
		status = "ativado"
	}
	return b.showAdministrationNotice(ctx, actor, chatID, msgID, "✅ Gestor Only "+status+". Reinicie o bot para aplicar totalmente.")
}

func (b *Bot) setXrayCreateEnabled(ctx context.Context, actor model.Actor, chatID int64, msgID int, enabled bool) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	if err := settings.NewManager(b.svc.Config, b.svc.Store).SetXrayCreateEnabled(ctx, enabled); err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro: "+err.Error(), adminKeyboard(b.currentGestorOnly(ctx), b.currentXrayCreateEnabled(ctx)), "flow")
	}
	b.svc.Config.XrayCreateEnabled = enabled
	status := "desativado"
	extra := ""
	if enabled {
		status = "ativado"
		if n, err := b.svc.Accounts.ActivateEligibleHiddenXray(ctx, true); err != nil {
			extra = "\n⚠️ Algumas contas ocultas não foram ativadas: " + err.Error()
		} else if n > 0 {
			extra = fmt.Sprintf("\n🌐 UUIDs ocultos ativados: %d", n)
		}
	}
	return b.showAdministrationNotice(ctx, actor, chatID, msgID, "✅ Xray Geral "+status+"."+extra)
}
func (b *Bot) currentExpirationNoticesEnabled(ctx context.Context) bool {
	if b == nil || b.svc.Store == nil {
		return true
	}
	v, err := b.svc.Store.GetSetting(ctx, "expiration_notices_enabled")
	if err != nil || strings.TrimSpace(v) == "" {
		return true
	}
	return parseSettingBool(v)
}

func (b *Bot) setExpirationNoticesEnabled(ctx context.Context, actor model.Actor, chatID int64, msgID int, enabled bool) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	if err := b.svc.Store.SetSetting(ctx, "expiration_notices_enabled", boolText(enabled)); err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro: "+err.Error(), adminKeyboard(b.currentGestorOnly(ctx), b.currentXrayCreateEnabled(ctx), b.currentExpirationNoticesEnabled(ctx)), "flow")
	}
	status := "desativado"
	if enabled {
		status = "ativado"
	}
	return b.showAdministrationNotice(ctx, actor, chatID, msgID, "✅ Avisos Expiração "+status+".")
}

func (b *Bot) showCloudflare(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	if strings.TrimSpace(b.cloudflareToken(ctx)) == "" {
		return b.sendOrEdit(ctx, chatID, msgID, "☁️ Cloudflare\n━━━━━━━━━━━━━━\n⚠️ Token Cloudflare não configurado.\n\nSalve o token em Administração > Token Cloudflare.", serversKeyboard(), "flow")
	}
	servers, _ := b.svc.Store.ListServers(ctx)
	ips := make([]string, 0, len(servers)+1)
	if !b.currentGestorOnly(ctx) && strings.TrimSpace(b.svc.Config.ServerHost) != "" {
		ips = append(ips, strings.TrimSpace(b.svc.Config.ServerHost))
	}
	for _, srv := range servers {
		if srv.Enabled && strings.TrimSpace(srv.Host) != "" {
			ips = append(ips, strings.TrimSpace(srv.Host))
		}
	}
	ips = uniqueStrings(ips)
	if len(ips) == 0 {
		return b.sendOrEdit(ctx, chatID, msgID, "☁️ Cloudflare\n━━━━━━━━━━━━━━\n⚠️ Nenhum servidor ativo encontrado para garantir no vpn.primecel.shop.", serversKeyboard(), "flow")
	}
	mgr := cloudflare.NewManager(b.svc.Config, b.svc.Store)
	rep, err := mgr.SyncServerDNSIPs(ctx, ips, false)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "☁️ Cloudflare\n━━━━━━━━━━━━━━\n⚠️ "+err.Error(), serversKeyboard(), "flow")
	}
	text := fmt.Sprintf("☁️ Cloudflare\n━━━━━━━━━━━━━━\nDomínio: <code>%s</code>\nIPs no bot: %d\nAdicionados: %d\nMantidos: %d\nRemovidos: %d\n━━━━━━━━━━━━━━\nDomínio dos servidores sincronizado automaticamente.", h(cloudflare.DefaultServerDomain), len(ips), rep.Created, rep.Kept, rep.Deleted)
	return b.sendOrEdit(ctx, chatID, msgID, text, serversKeyboard(), "flow")
}

func isBotCommand(text, cmd string) bool {
	first := strings.Fields(strings.TrimSpace(text))
	if len(first) == 0 {
		return false
	}
	name := first[0]
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	return strings.EqualFold(name, cmd)
}

func commandArg(text, cmd string) string {
	parts := strings.Fields(strings.TrimSpace(text))
	if len(parts) < 2 {
		return ""
	}
	name := parts[0]
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	if !strings.EqualFold(name, cmd) {
		return ""
	}
	return strings.TrimSpace(strings.Join(parts[1:], " "))
}

func nonEmptyText(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func (b *Bot) showDevices(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	text := "🧹 <b>Limpar Aparelhos</b>\n━━━━━━━━━━━━━━\nRemove os aparelhos registrados das contas do seu escopo."
	return b.sendOrEdit(ctx, chatID, msgID, text, devicesKeyboard(), "submenu")
}

func (b *Bot) clearDevices(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		if err := b.clearAllDeviceRegistrations(ctx); err != nil {
			return b.sendDeviceCleanupResult(ctx, actor, chatID, msgID, "⚠️ Erro ao limpar aparelhos: "+err.Error())
		}
		return b.sendDeviceCleanupResult(ctx, actor, chatID, msgID, "✅ Aparelhos limpos.")
	}
	accs, _ := b.svc.Store.ListAccounts(ctx, false)
	owners := b.visibleOwnerIDs(ctx, actor)
	usernames := make([]string, 0, len(accs))
	for _, a := range accs {
		if accountVisible(actor, owners, a) {
			usernames = append(usernames, a.Username)
		}
	}
	if err := b.clearDeviceRegistrationsForUsers(ctx, usernames, true); err != nil {
		return b.sendDeviceCleanupResult(ctx, actor, chatID, msgID, "⚠️ Erro ao limpar aparelhos: "+err.Error())
	}
	return b.sendDeviceCleanupResult(ctx, actor, chatID, msgID, "✅ Aparelhos limpos.")
}

func (b *Bot) sendDeviceCleanupResult(ctx context.Context, actor model.Actor, chatID int64, msgID int, text string) error {
	const kind = "device_cleanup_result"
	if err := b.sendOrEdit(ctx, chatID, msgID, text, InlineKeyboardMarkup{}, kind); err != nil {
		return err
	}
	resultMsgID, _ := b.latestTrackedMessageIDByKind(ctx, chatID, kind)
	if resultMsgID != 0 {
		b.scheduleDeviceCleanupResultHome(chatID, resultMsgID, actor, text)
	}
	return nil
}

func (b *Bot) latestTrackedMessageIDByKind(ctx context.Context, chatID int64, kind string) (int, error) {
	if b == nil || b.svc.Store == nil || chatID == 0 || strings.TrimSpace(kind) == "" {
		return 0, nil
	}
	rows, err := b.svc.Store.Query(ctx, `SELECT message_id FROM telegram_chat_messages WHERE chat_id=? AND kind=? ORDER BY updated_at DESC LIMIT 1`, chatID, kind)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	msgID, _ := strconv.Atoi(rows[0]["message_id"])
	return msgID, nil
}

func (b *Bot) scheduleDeviceCleanupResultHome(chatID int64, msgID int, actor model.Actor, text string) {
	if b == nil || b.client == nil || b.svc.Store == nil || chatID == 0 || msgID == 0 {
		return
	}
	go func() {
		timer := time.NewTimer(deviceCleanupResultAutoDeleteAfter)
		defer timer.Stop()
		<-timer.C

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		rows, err := b.svc.Store.Query(ctx, `SELECT kind,last_text FROM telegram_chat_messages WHERE chat_id=? AND message_id=? LIMIT 1`, chatID, msgID)
		if err != nil || len(rows) == 0 {
			return
		}
		if rows[0]["kind"] != "device_cleanup_result" || rows[0]["last_text"] != text {
			return
		}

		_ = b.client.DeleteMessage(ctx, chatID, msgID)
		_ = b.svc.Store.Exec(ctx, `DELETE FROM telegram_chat_messages WHERE chat_id=? AND message_id=?`, chatID, msgID)
		_ = b.clearFlow(ctx, actor.TelegramID)
		freshActor := b.resolveActor(ctx, actor.TelegramID, actor.Name)
		if err := b.showMain(ctx, freshActor, chatID, 0); err != nil {
			return
		}
		_ = b.markAutoHome(ctx, chatID, actor.TelegramID)
	}()
}

func (b *Bot) clearAllDeviceRegistrations(ctx context.Context) error {
	if err := b.svc.Store.Exec(ctx, `DELETE FROM devices`); err != nil {
		return err
	}
	if err := checkuserdb.ClearAll(ctx, b.svc.Config.CheckUserDBPath); err != nil {
		return err
	}
	remoteCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err := remotesync.NewManager(b.svc.Config, b.svc.Store).SyncDeviceScope(remoteCtx)
	return err
}

func (b *Bot) clearDeviceRegistrationsForUsers(ctx context.Context, usernames []string, batch bool) error {
	usernames = uniqueDeviceCleanupUsernames(usernames)
	if len(usernames) == 0 {
		return nil
	}
	for _, username := range usernames {
		if err := b.svc.Store.ClearDevicesForUser(ctx, username, false); err != nil {
			return err
		}
	}
	if err := checkuserdb.ClearUsers(ctx, b.svc.Config.CheckUserDBPath, usernames); err != nil {
		return err
	}
	remoteCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	mgr := remotesync.NewManager(b.svc.Config, b.svc.Store)
	if len(usernames) == 1 && !batch {
		_, err := mgr.SyncDeviceUser(remoteCtx, usernames[0])
		return err
	}
	_, err := mgr.SyncDeviceUsers(remoteCtx, usernames)
	return err
}

func uniqueDeviceCleanupUsernames(values []string) []string {
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

func (b *Bot) showBackup(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	text := "💾 <b>Sistema Backup</b>\n━━━━━━━━━━━━━━\n" + h(b.backupAutoStatusText(ctx))
	return b.sendOrEdit(ctx, chatID, msgID, text, backupKeyboard(), "submenu")
}

func (b *Bot) backupManager() *backup.Manager {
	mw := mirrors.NewWriter(b.svc.Config, b.svc.Store)
	sys := system.NewLocalManager(b.svc.Config)
	xm := xray.NewManager(b.svc.Config)
	return backup.NewManager(b.svc.Config, b.svc.Store, mw, sys, xm)
}

func (b *Bot) exportBackup(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	_ = b.sendOrEdit(ctx, chatID, msgID, "⏳ Gerando backup para exportação...", backupKeyboard(), "flow")
	rep, err := b.backupManager().Create(ctx, backup.CreateOptions{})
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Erro ao exportar backup:\n<code>"+h(err.Error())+"</code>", backupKeyboard(), "flow")
	}
	caption := fmt.Sprintf("✅ Backup gerado com sucesso\n\nTipo: export\nArquivos: %d\nTamanho: %s", len(rep.Files), humanSize(rep.SizeBytes))
	sent, err := b.client.SendDocument(ctx, chatID, rep.Path, "", caption, filepath.Base(rep.Path))
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "✅ Backup gerado\n━━━━━━━━━━━━━━\nArquivo: <code>"+h(rep.Path)+"</code>\n\n⚠️ Erro ao enviar documento: <code>"+h(err.Error())+"</code>", backupKeyboard(), "flow")
	}
	if sent != nil {
		_ = b.trackProtectedMessage(ctx, chatID, sent.MessageID, "backup", caption)
	}
	b.removeGeneratedBackupFile(ctx, rep.Path)
	return b.showBackup(ctx, actor, chatID, msgID)
}

func (b *Bot) removeGeneratedBackupFile(ctx context.Context, path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_ = b.svc.Store.SetSetting(ctx, "backup_last_cleanup_error", err.Error())
	}
}

func (b *Bot) startBackupImport(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	_ = b.setFlow(ctx, actor, chatID, "backup_import_file", map[string]string{})
	return b.sendOrEdit(ctx, chatID, msgID, "📥 <b>Importar</b>\n━━━━━━━━━━━━━━\nEnvie agora o arquivo <code>backup-painel.tar.gz</code> do backup.\n\nTambém aceito o caminho local do arquivo no servidor.", kb([]Button{{"⬅️ Voltar", "menu_backup"}}), "flow")
}

func (b *Bot) finishBackupImport(ctx context.Context, actor model.Actor, chatID int64, msgID int, confirm, file string) error {
	if strings.TrimSpace(confirm) != "IMPORTAR" {
		_ = b.clearFlow(ctx, actor.TelegramID)
		return b.sendOrEdit(ctx, chatID, msgID, "Importação cancelada.", backupKeyboard(), "flow")
	}
	_ = b.sendOrEdit(ctx, chatID, msgID, "⏳ Importando backup...", backupKeyboard(), "flow")
	rep, err := b.backupManager().Import(ctx, backup.ImportOptions{File: file, Clean: true, ConfirmText: "IMPORTAR", SyncRemotes: true})
	_ = b.clearFlow(ctx, actor.TelegramID)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Erro ao importar backup:\n<code>"+h(err.Error())+"</code>", backupKeyboard(), "flow")
	}
	msg := fmt.Sprintf("✅ <b>Backup importado</b>\n\nArquivos restaurados: <code>%d</code>\nIgnorados: <code>%d</code>\nXray limpo: <code>%d</code>\nRemoções locais: <code>%d</code>\nRemoções sincronizadas: <code>%d</code>\n\nℹ️ Para recriar os logins/UUIDs no servidor principal, use:\n<b>Servidores &gt; Sinc Principal</b>\n\nContas importadas: <code>%d</code>\nRevendas: <code>%d</code>\nServidores: <code>%d</code>", len(rep.CopiedPortable), len(rep.Warnings), rep.XrayRemovals, rep.LocalRemovals, rep.SyncedRemovals, rep.LegacyReport.AccountsImported, rep.LegacyReport.ResellersImported, rep.LegacyReport.ServersDetected)
	return b.sendOrEdit(ctx, chatID, msgID, msg, backupKeyboard(), "flow")
}

func (b *Bot) showBackupDestination(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	text := "📍 <b>Destino</b>\n━━━━━━━━━━━━━━\nEscolha onde o backup automático será enviado:"
	return b.sendOrEdit(ctx, chatID, msgID, text, kb([]Button{{"🤖 Mesmo bot", "backup_dest_same"}, {"🔁 Outro bot", "backup_dest_other"}}, []Button{{"⬅️ Voltar", "menu_backup"}}), "submenu")
}

func (b *Bot) setBackupDestinationSame(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	_ = b.svc.Store.SetSetting(ctx, "backup_destination_mode", "same_bot")
	_ = b.svc.Store.SetSetting(ctx, "backup_remote_bot_token", "")
	return b.showBackup(ctx, actor, chatID, msgID)
}

func (b *Bot) startBackupDestinationOther(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	_ = b.setFlow(ctx, actor, chatID, "backup_dest_other_token", map[string]string{})
	return b.sendOrEdit(ctx, chatID, msgID, "🔁 <b>Outro bot</b>\n━━━━━━━━━━━━━━\nEnvie o token do bot:", kb([]Button{{"⬅️ Voltar", "menu_backup"}}), "flow")
}

func (b *Bot) showBackupAuto(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	enabled := settingBool(ctx, b.svc.Store, "backup_auto_enabled")
	current := settingInt(ctx, b.svc.Store, "backup_auto_interval_hours", 12)
	rows := [][]Button{{{"2h", "backup_auto_interval_2"}, {"6h", "backup_auto_interval_6"}, {"12h", "backup_auto_interval_12"}, {"24h", "backup_auto_interval_24"}}}
	if enabled {
		rows = append(rows, []Button{{"⬅️ Voltar", "menu_backup"}, {"⛔ Desativar", "backup_auto_disable"}})
	} else {
		rows = append(rows, []Button{{"⬅️ Voltar", "menu_backup"}})
	}
	text := fmt.Sprintf("⏱️ <b>Automático</b>\n━━━━━━━━━━━━━━\nEscolha o tempo do backup automático:\n\nStatus: %s | Tempo: %dh", map[bool]string{true: "🟢", false: "🔴"}[enabled], current)
	return b.sendOrEdit(ctx, chatID, msgID, text, kb(rows...), "submenu")
}

func (b *Bot) setBackupAutoInterval(ctx context.Context, actor model.Actor, chatID int64, msgID int, raw string) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	hours, _ := strconv.Atoi(raw)
	if hours != 2 && hours != 6 && hours != 12 && hours != 24 {
		hours = 12
	}
	_ = b.svc.Store.SetSetting(ctx, "backup_auto_enabled", "1")
	_ = b.svc.Store.SetSetting(ctx, "backup_auto_interval_hours", strconv.Itoa(hours))
	return b.showBackup(ctx, actor, chatID, msgID)
}

func (b *Bot) disableBackupAuto(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	_ = b.svc.Store.SetSetting(ctx, "backup_auto_enabled", "0")
	return b.showBackup(ctx, actor, chatID, msgID)
}

func (b *Bot) backupAutoStatusText(ctx context.Context) string {
	enabled := settingBool(ctx, b.svc.Store, "backup_auto_enabled")
	interval := settingInt(ctx, b.svc.Store, "backup_auto_interval_hours", 12)
	mode, _ := b.svc.Store.GetSetting(ctx, "backup_destination_mode")
	if strings.TrimSpace(mode) == "" {
		mode = "same_bot"
	}
	dest := "Mesmo bot"
	if mode == "other_bot" {
		dest = "Outro bot"
	}
	last, _ := b.svc.Store.GetSetting(ctx, "backup_auto_last_at")
	if strings.TrimSpace(last) == "" {
		last = "Nunca"
	}
	status := "🔴 Desativado"
	if enabled {
		status = "🟢 Ativado"
	}
	return fmt.Sprintf("Status: %s\nTempo: %dh\nDestino: %s\nÚltimo backup: %s", status, interval, dest, last)
}

func (b *Bot) backupAdminStatusText(ctx context.Context) string {
	enabled := settingBool(ctx, b.svc.Store, "backup_auto_enabled")
	interval := settingInt(ctx, b.svc.Store, "backup_auto_interval_hours", 12)
	status := "🔴"
	if enabled {
		status = "🟢"
	}
	return fmt.Sprintf("Backup: %s | %dh", status, interval)
}

func (b *Bot) showPayments(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !paymentsManageAllowed(ctx, b.svc.Store, actor) {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin, revenda ou subrevenda.", backKeyboard(), "flow")
	}
	ownerID := paymentOwnerID(actor)
	cfg, _ := b.svc.Store.FindPaymentOwnerConfig(ctx, ownerID)
	bankID := "mercado_pago"
	enabled := false
	if cfg != nil {
		bankID = firstNonEmpty(cfg.Bank, bankID)
		enabled = cfg.Enabled
	}
	webhook := paymentWebhookURL(ctx, b.svc.Store, b.svc.Config.CheckUserPublicURL, ownerID)
	renewalNotice := b.currentPaymentRenewalNoticeEnabled(ctx, ownerID)
	renewalBadge := "🔴 OFF"
	if renewalNotice {
		renewalBadge = "🟢 ON"
	}
	ownerLabel := "Dono"
	if actor.Role == model.RoleSubReseller {
		ownerLabel = "SubRevenda"
	} else if actor.Role == model.RoleReseller {
		ownerLabel = "Revenda"
	}
	lines := []string{
		"💳 <b>Pagamentos</b>",
		"━━━━━━━━━━━━━━━━━━",
		fmt.Sprintf("👤 %s: <b>%s</b>", ownerLabel, h(paymentOwnerLabel(ctx, b.svc.Store, ownerID))),
		fmt.Sprintf("📌 Status: %s", h(paymentStatusBadge(enabled))),
		fmt.Sprintf("🏦 Banco Pix: <b>%s</b>", h(paymentBankName(bankID))),
		fmt.Sprintf("🔔 Receber aviso: <b>%s</b>", h(renewalBadge)),
	}
	if ownerID == 0 {
		lines = append(lines, fmt.Sprintf("🌐 WebHook: %s", h(firstNonEmpty(webhook, "não configurado"))))
	}
	return b.sendOrEdit(ctx, chatID, msgID, strings.Join(lines, "\n"), paymentsKeyboard(ownerID, enabled, paymentActorIsAdmin(actor), renewalNotice), "submenu")
}

func (b *Bot) showPaymentsConfig(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !paymentsManageAllowed(ctx, b.svc.Store, actor) {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin, revenda ou subrevenda.", backKeyboard(), "flow")
	}
	text := "⚙️ <b>Configurar Pagamentos:</b>"
	return b.sendOrEdit(ctx, chatID, msgID, text, paymentsConfigKeyboard(actor), "submenu")
}

func (b *Bot) togglePayments(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !paymentsManageAllowed(ctx, b.svc.Store, actor) {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin, revenda ou subrevenda.", backKeyboard(), "flow")
	}
	ownerID := paymentOwnerID(actor)
	mgr := payments.NewManager(b.svc.Store)
	cfg, _ := b.svc.Store.FindPaymentOwnerConfig(ctx, ownerID)
	bank := "mercado_pago"
	token := ""
	dataJSON := "{}"
	enabled := true
	if cfg != nil {
		bank = firstNonEmpty(cfg.Bank, bank)
		token = cfg.Token
		dataJSON = cfg.DataJSON
		enabled = !cfg.Enabled
	}
	_, _ = mgr.ConfigureOwner(ctx, payments.OwnerConfigInput{OwnerID: ownerID, Bank: bank, Token: token, Enabled: enabled, DataJSON: dataJSON})
	return b.showPayments(ctx, actor, chatID, msgID)
}

func (b *Bot) showPaymentsTutorialMenu(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	text := "📘 <b>Tutorial Pagamentos</b>\n━━━━━━━━━━━━━━\n1. Asaas\n2. Mercado Pago\n3. InfinitePay\n━━━━━━━━━━━━━━\n0. Voltar\n━━━━━━━━━━━━━━\nDigite a Opção:"
	return b.sendOrEdit(ctx, chatID, msgID, text, paymentsTutorialKeyboard(), "submenu")
}

func (b *Bot) showPaymentsTutorial(ctx context.Context, actor model.Actor, chatID int64, msgID int, bank string) error {
	webhook := paymentWebhookURL(ctx, b.svc.Store, b.svc.Config.CheckUserPublicURL, 0)
	webhookLine := firstNonEmpty(webhook, "não configurado pelo Admin")
	var text string
	switch normalizePaymentBank(bank) {
	case "asaas":
		text = "📘 <b>Tutorial Asaas</b>\n━━━━━━━━━━━━━━\n<b>1. Dados necessários</b>\n• Token de API do Asaas.\n• Customer ID do Asaas ou CPF/CNPJ para criar o cliente usado na cobrança Pix.\n\n<b>2. Configurar no bot</b>\n• Vá em Pagamentos.\n• Clique em Configurar.\n• Escolha Asaas.\n• Envie o Token de API.\n• Envie o Customer ID ou CPF/CNPJ quando o bot pedir.\n\n<b>3. Como o cliente paga</b>\n• O bot cria a cobrança no valor da revenda, subrevenda ou conta.\n• O cliente recebe o Pix copia e cola.\n• Depois de pago, o sistema confirma e libera/renova.\n━━━━━━━━━━━━━━\n0. Voltar"
	case "mercado_pago":
		text = "📘 <b>Tutorial Mercado Pago</b>\n━━━━━━━━━━━━━━\n<b>1. Dado necessário</b>\n• Access Token de produção.\n\n<b>2. Configurar no bot</b>\n• Vá em Pagamentos.\n• Clique em Configurar.\n• Escolha Mercado Pago.\n• Envie o Access Token.\n\n<b>3. Como o cliente paga</b>\n• O bot gera o Pix pelo Mercado Pago.\n• O cliente paga pelo Pix copia e cola.\n• Quando aprovado, o bot libera/renova o acesso automaticamente.\n━━━━━━━━━━━━━━\n0. Voltar"
	default:
		text = "📘 <b>Tutorial InfinitePay</b>\n━━━━━━━━━━━━━━\n<b>1. Dado necessário</b>\n• InfiniteTag/Handle da conta InfinitePay.\n• Exemplo: se aparecer <code>$primecel</code>, envie apenas <code>primecel</code>.\n\n<b>2. Configurar no bot</b>\n• Vá em Pagamentos.\n• Clique em Configurar.\n• Escolha InfinitePay.\n• Envie a InfiniteTag/Handle.\n\n<b>3. Como o cliente paga</b>\n• O bot cria o link com o valor do pedido definido no próprio bot.\n• Planos criados manualmente na InfinitePay não alteram os valores do bot.\n\n<b>4. Configurar WebHook</b>\n• No painel da InfinitePay, cadastre este link:\n<code>" + h(webhookLine) + "</code>\n• Esse link é usado para confirmar pagamento automático.\n━━━━━━━━━━━━━━\n0. Voltar"
	}
	return b.sendOrEdit(ctx, chatID, msgID, text, kb([]Button{{"⬅️ Voltar", "payments_tutorial"}}), "submenu")
}

func (b *Bot) showPaymentOrdersOwner(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showPaymentOrdersOwnerPage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) showPaymentOrdersOwnerPage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	if !paymentsManageAllowed(ctx, b.svc.Store, actor) {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin, revenda ou subrevenda.", backKeyboard(), "flow")
	}
	ownerID := paymentOwnerID(actor)
	orders, _ := b.svc.Store.ListPaymentOrders(ctx, ownerID, "")
	page, pages, start, end := paginateBounds(len(orders), page, listPageSize)
	var sb strings.Builder
	sb.WriteString("💳 <b>Pagamentos</b>\n━━━━━━PEDIDOS━━━━━━\n")
	if len(orders) == 0 {
		sb.WriteString("Nenhum pedido encontrado.\n")
	} else {
		for i, o := range orders[start:end] {
			if i > 0 {
				sb.WriteString("\n")
			}
			buyer := paymentOwnerLabel(ctx, b.svc.Store, o.TargetResellerID)
			fmt.Fprintf(&sb, "🧾 Pedido: <code>%s</code>\n", h(o.OrderID))
			fmt.Fprintf(&sb, "👤 Cliente: <b>%s</b>\n", h(buyer))
			fmt.Fprintf(&sb, "📦 Tipo: <b>%s</b>\n", h(paymentKindText(o.Kind)))
			fmt.Fprintf(&sb, "💰 Valor: <code>%s</code>\n", h(moneyBR(o.Amount)))
			fmt.Fprintf(&sb, "📌 Status: <code>%s</code>\n", h(paymentStatusText(o.Status, o.AppliedAt != nil)))
			fmt.Fprintf(&sb, "🏦 Banco: <b>%s</b>\n", h(paymentBankName(o.Bank)))
			if start+i < end-1 {
				sb.WriteString("━━━━━━━━━━━━━━\n")
			}
		}
	}
	sb.WriteString("\n━━━━━━━━━━━━━━\n✅ Pedidos aprovados pelo WebHook liberam automaticamente.\n🛡️ O sistema evita duplicar limite/renovação.")
	return b.sendOrEdit(ctx, chatID, msgID, sb.String(), pagedListKeyboard("payments_orders_page", page, pages, "menu_payments"), "submenu")
}
func (b *Bot) startPaymentBankConfig(ctx context.Context, actor model.Actor, chatID int64, msgID int, bank string) error {
	if !paymentsManageAllowed(ctx, b.svc.Store, actor) {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin, revenda ou subrevenda.", backKeyboard(), "flow")
	}
	bank = normalizePaymentBank(bank)
	if bank == "" {
		return b.showPaymentsConfig(ctx, actor, chatID, msgID)
	}
	data := b.currentPaymentBankFlowData(ctx, actor, bank)
	_ = b.setFlow(ctx, actor, chatID, "payment_bank_token", data)
	var prompt string
	switch bank {
	case "infinitepay":
		prompt = "🏦 <b>InfinitePay:</b>\n━━━━━━━━━━━━━━\nEx: Se no app aparecer <code>$primecel</code>, envie apenas <code>primecel</code>.\n━━━━━━━━━━━━━━\nEnvie o <b>InfiniteTag/Handle</b>:"
	case "asaas":
		prompt = "🏦 <b>Asaas:</b>\n━━━━━━━━━━━━━━\n1/2 — Envie o <b>Token de API</b>:"
	case "mercado_pago":
		prompt = "🏦 <b>Mercado Pago:</b>\n━━━━━━━━━━━━━━\nEnvie o <b>Access Token de produção</b>:"
	default:
		prompt = "🏦 <b>Banco Pix:</b>\n━━━━━━━━━━━━━━\nEnvie o dado solicitado:"
	}
	return b.sendOrEdit(ctx, chatID, msgID, prompt, backKeyboard(), "flow")
}

func (b *Bot) currentPaymentBankFlowData(ctx context.Context, actor model.Actor, bank string) map[string]string {
	data := map[string]string{"bank": bank}
	cfg, _ := b.svc.Store.FindPaymentOwnerConfig(ctx, paymentOwnerID(actor))
	if cfg == nil || normalizePaymentBank(cfg.Bank) != bank {
		return data
	}
	if strings.TrimSpace(cfg.Token) != "" {
		data["current_token"] = strings.TrimSpace(cfg.Token)
	}
	var raw map[string]any
	if json.Unmarshal([]byte(cfg.DataJSON), &raw) == nil {
		for _, key := range []string{"asaas_customer_id", "asaas_customer_cpf_cnpj", "infinitepay_handle", "infinitepay_tag", "webhook_secret", "asaas_webhook_token", "mp_webhook_secret", "infinitepay_webhook_secret", "webhook_require_auth"} {
			if v := strings.TrimSpace(fmt.Sprint(raw[key])); v != "" && v != "<nil>" {
				data[key] = v
			}
		}
	}
	return data
}

func (b *Bot) finishPaymentBankConfig(ctx context.Context, actor model.Actor, chatID int64, bank, token string, flowData map[string]string) error {
	bank = normalizePaymentBank(bank)
	if bank == "" {
		_ = b.clearFlow(ctx, actor.TelegramID)
		return b.showPayments(ctx, actor, chatID, 0)
	}
	token = strings.TrimSpace(token)
	if bank == "infinitepay" {
		token = strings.TrimPrefix(token, "$")
	}
	if token == "" {
		label := "token/API de produção"
		if bank == "infinitepay" {
			label = "InfiniteTag/Handle"
		}
		return b.sendOrEdit(ctx, chatID, 0, "⚠️ Dado vazio. Envie o "+h(label)+" ou digite <code>0</code> para voltar.", backKeyboard(), "flow")
	}
	data := map[string]any{}
	for _, key := range []string{"webhook_secret", "asaas_webhook_token", "mp_webhook_secret", "infinitepay_webhook_secret", "webhook_require_auth"} {
		if v := strings.TrimSpace(flowData[key]); v != "" {
			data[key] = v
		}
	}
	if bank == "asaas" {
		if v := strings.TrimSpace(flowData["asaas_customer_id"]); v != "" {
			data["asaas_customer_id"] = v
		}
		if v := strings.TrimSpace(flowData["asaas_customer_cpf_cnpj"]); v != "" {
			data["asaas_customer_cpf_cnpj"] = v
		}
	}
	if bank == "infinitepay" {
		handle := strings.TrimPrefix(firstNonEmpty(token, flowData["infinitepay_handle"], flowData["infinitepay_tag"]), "$")
		data["infinitepay_handle"] = handle
		token = handle
	}
	bts, _ := json.Marshal(data)
	ownerID := paymentOwnerID(actor)
	mgr := payments.NewManager(b.svc.Store)
	_, err := mgr.ConfigureOwner(ctx, payments.OwnerConfigInput{OwnerID: ownerID, Bank: bank, Token: token, Enabled: true, DataJSON: string(bts)})
	_ = b.clearFlow(ctx, actor.TelegramID)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao configurar pagamentos: "+err.Error(), paymentsConfigKeyboard(actor), "flow")
	}
	return b.sendOrEdit(ctx, chatID, 0, "✅ Pagamentos configurados com sucesso.\n\n"+paymentAdminText(ctx, b.svc.Store, ownerID), paymentsKeyboard(ownerID, true, paymentActorIsAdmin(actor), b.currentPaymentRenewalNoticeEnabled(ctx, ownerID)), "flow")
}

func (b *Bot) startPaymentWebhookDomain(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Apenas o admin pode alterar o WebHook.", backKeyboard(), "flow")
	}
	_ = b.setFlow(ctx, actor, chatID, "payment_webhook_domain", map[string]string{})
	text := `🌐 <b>WebHook Pix</b>
━━━━━━━━━━━━━━
Domínio padrão: <code>api.primecel.shop/pix</code>

Envie outro domínio/link apenas se quiser alterar.
Exemplo: <code>api.seudominio.com/pix</code>

Digite <code>0</code> para voltar.`
	return b.sendOrEdit(ctx, chatID, msgID, text, backKeyboard(), "flow")
}

func (b *Bot) showPaymentWebhookStatus(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Apenas o admin pode ver o WebHook.", backKeyboard(), "flow")
	}
	url := paymentWebhookURL(ctx, b.svc.Store, b.svc.Config.CheckUserPublicURL, 0)
	status := "⚠️ não configurado"
	if strings.TrimSpace(url) != "" {
		status = "✅ configurado"
	}
	events, _ := b.svc.Store.ListPaymentWebhookEvents(ctx, -1, 1)
	last := "nenhum evento recebido"
	if len(events) > 0 {
		last = fmt.Sprintf("%s | %s | %s", events[0].CreatedAt.Local().Format("02/01 15:04"), events[0].Result, firstNonEmpty(events[0].OrderID, "sem pedido"))
	}
	text := "🌐 <b>WebHook Pix</b>\n━━━━━━━━━━━━━━\n" +
		fmt.Sprintf("Status: <code>%s</code>\n", h(status)) +
		fmt.Sprintf("URL: <code>%s</code>\n", h(firstNonEmpty(url, "não configurado"))) +
		"Porta padrão: <code>8099</code>\n" +
		fmt.Sprintf("Último evento: <code>%s</code>\n", h(last)) +
		"━━━━━━━━━━━━━━\n" +
		"O WebHook agora registra eventos, bloqueia duplicidade, valida token/assinatura quando configurado e confirma o pagamento antes de liberar."
	return b.sendOrEdit(ctx, chatID, msgID, text, kb([]Button{{"⬅️ Voltar", "menu_payments"}, {"🧾 Eventos", "payments_webhook_events"}}), "submenu")
}

func (b *Bot) showPaymentWebhookEvents(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Apenas o admin pode ver eventos do WebHook.", backKeyboard(), "flow")
	}
	events, _ := b.svc.Store.ListPaymentWebhookEvents(ctx, -1, 10)
	var sb strings.Builder
	sb.WriteString("🧾 <b>Eventos WebHook</b>\n━━━━━━━━━━━━━━\n")
	if len(events) == 0 {
		sb.WriteString("Nenhum evento recebido.\n")
	} else {
		for _, ev := range events {
			status := ev.Result
			if ev.ErrorText != "" {
				status += " / " + ev.ErrorText
			}
			fmt.Fprintf(&sb, "• <code>%s</code> | %s | <b>%s</b> | %s\n", h(ev.CreatedAt.Local().Format("02/01 15:04")), h(firstNonEmpty(ev.OrderID, "sem pedido")), h(firstNonEmpty(ev.Bank, "banco")), h(status))
		}
	}
	sb.WriteString("━━━━━━━━━━━━━━\nEventos duplicados são ignorados e pedidos já aplicados não liberam limite novamente.")
	return b.sendOrEdit(ctx, chatID, msgID, sb.String(), kb([]Button{{"⬅️ Voltar", "menu_payments"}, {"🌐 Status", "payments_webhook_status"}}), "submenu")
}

func (b *Bot) testPaymentWebhook(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Apenas o admin pode testar a API.", backKeyboard(), "flow")
	}
	url := strings.TrimRight(paymentWebhookURL(ctx, b.svc.Store, b.svc.Config.CheckUserPublicURL, 0), "/")
	enabled := true
	if cfg, _ := b.svc.Store.FindPaymentOwnerConfig(ctx, 0); cfg != nil {
		enabled = cfg.Enabled
	}
	if url == "" {
		return b.sendOrEdit(ctx, chatID, msgID, "📶 Testar API\n━━━━━━━━━━━━━━\n⚠️ API Pix não configurada.", paymentsKeyboard(0, enabled, true, b.currentPaymentRenewalNoticeEnabled(ctx, 0)), "flow")
	}
	statusURL := paymentWebhookStatusURL(url)
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(runCtx, http.MethodGet, statusURL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "📶 Testar API\n━━━━━━━━━━━━━━\nURL: <code>"+h(statusURL)+"</code>\nStatus: ❌ falhou\nErro: <code>"+h(err.Error())+"</code>", paymentsKeyboard(0, enabled, true, b.currentPaymentRenewalNoticeEnabled(ctx, 0)), "flow")
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	ok := "✅ online"
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		ok = "⚠️ HTTP " + strconv.Itoa(res.StatusCode)
	}
	text := fmt.Sprintf("📶 Testar API\n━━━━━━━━━━━━━━\nURL: <code>%s</code>\nStatus: <code>%s</code>\nResposta:\n<code>%s</code>", h(statusURL), h(ok), h(tailText(string(body), 1500)))
	return b.sendOrEdit(ctx, chatID, msgID, text, paymentsKeyboard(0, enabled, true, b.currentPaymentRenewalNoticeEnabled(ctx, 0)), "flow")
}

func (b *Bot) showPaymentsReportMenu(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showPaymentsReportPage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) showPaymentsReport(ctx context.Context, actor model.Actor, chatID int64, msgID int, period string) error {
	return b.showPaymentsReportPage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) showPaymentsReportPage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	if !paymentsManageAllowed(ctx, b.svc.Store, actor) {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin, revenda ou subrevenda.", backKeyboard(), "flow")
	}
	ownerID := paymentOwnerID(actor)
	now := time.Now().UTC()
	monthlyStart, annualStart := b.paymentReportCycleStarts(ctx, ownerID, now)

	q := `SELECT order_id, kind, months, days, amount, payload_json, status, paid_at, applied_at, created_at FROM payment_orders WHERE kind='account_renew' AND created_at>=?`
	args := []any{annualStart.Format(time.RFC3339)}
	if ownerID >= 0 {
		q += ` AND owner_id=?`
		args = append(args, ownerID)
	}
	q += ` ORDER BY created_at DESC LIMIT 2000`
	rows, err := b.svc.Store.Query(ctx, q, args...)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro no relatório: "+err.Error(), paymentsKeyboard(ownerID, true, paymentActorIsAdmin(actor), b.currentPaymentRenewalNoticeEnabled(ctx, ownerID)), "flow")
	}

	type reportLine struct {
		Username string
		Months   string
		Amount   float64
		When     time.Time
	}
	monthlyLines := []reportLine{}
	var monthlyTotal float64
	var annualTotal float64

	for _, r := range rows {
		if !paymentOrderPaidForReport(r) {
			continue
		}
		paidAt := paymentReportPaidAt(r)
		if paidAt.IsZero() {
			continue
		}
		amount, _ := strconv.ParseFloat(r["amount"], 64)
		if !paidAt.Before(annualStart) {
			annualTotal += amount
		}
		if paidAt.Before(monthlyStart) {
			continue
		}
		username := paymentReportUsername(r["payload_json"])
		if username == "" {
			continue
		}
		monthlyTotal += amount
		monthlyLines = append(monthlyLines, reportLine{Username: username, Months: paymentReportMonths(r["months"], r["days"]), Amount: amount, When: paidAt})
	}

	sort.Slice(monthlyLines, func(i, j int) bool { return monthlyLines[i].When.After(monthlyLines[j].When) })

	const reportPageSize = 15
	page, pages, start, end := paginateBounds(len(monthlyLines), page, reportPageSize)

	var sb strings.Builder
	sb.WriteString("<b>💹 Relatório de Renovações</b>\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━\n")
	if len(monthlyLines) == 0 {
		sb.WriteString("Nenhuma conta paga no ciclo mensal atual.\n")
		sb.WriteString("━━━━━━━━━━━━━━━━━━\n")
	} else {
		for _, item := range monthlyLines[start:end] {
			fmt.Fprintf(&sb, "👤 %s\n", h(item.Username))
			fmt.Fprintf(&sb, "⏳ %s • 💰 %s\n", h(item.Months), h(moneyBR(item.Amount)))
			sb.WriteString("━━━━━━━━━━━━━━━━━━\n")
		}
	}
	sb.WriteString("<b>💰 Total:</b>\n")
	fmt.Fprintf(&sb, "<b>Mensal:</b> %s • <b>Anual:</b> %s", h(moneyBR(monthlyTotal)), h(moneyBR(annualTotal)))
	sb.WriteString(pageIndicator(page, pages))

	return b.sendOrEdit(ctx, chatID, msgID, sb.String(), pagedListKeyboard("payments_report_page", page, pages, "menu_payments"), "submenu")
}

func (b *Bot) paymentReportCycleStarts(ctx context.Context, ownerID int64, now time.Time) (time.Time, time.Time) {
	monthlyDefault := now.AddDate(0, 0, -31)
	annualDefault := now.AddDate(-1, 0, 0)
	if b == nil || b.svc.Store == nil {
		return monthlyDefault, annualDefault
	}
	monthlyKey := fmt.Sprintf("payment_report_monthly_start_%d", ownerID)
	annualKey := fmt.Sprintf("payment_report_annual_start_%d", ownerID)
	monthlyStart := paymentReportCycleStart(ctx, b.svc.Store, monthlyKey, monthlyDefault)
	annualStart := paymentReportCycleStart(ctx, b.svc.Store, annualKey, annualDefault)
	if !monthlyStart.IsZero() && !now.Before(monthlyStart.AddDate(0, 0, 31)) {
		monthlyStart = now
		_ = b.svc.Store.SetSetting(ctx, monthlyKey, monthlyStart.Format(time.RFC3339))
	}
	if !annualStart.IsZero() && !now.Before(annualStart.AddDate(1, 0, 0)) {
		annualStart = now
		_ = b.svc.Store.SetSetting(ctx, annualKey, annualStart.Format(time.RFC3339))
	}
	return monthlyStart, annualStart
}

func paymentReportCycleStart(ctx context.Context, st *store.DB, key string, def time.Time) time.Time {
	v, _ := st.GetSetting(ctx, key)
	v = strings.TrimSpace(v)
	if v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UTC()
		}
	}
	def = def.UTC()
	_ = st.SetSetting(ctx, key, def.Format(time.RFC3339))
	return def
}

func paymentReportPaidAt(r map[string]string) time.Time {
	for _, key := range []string{"applied_at", "paid_at", "created_at"} {
		v := strings.TrimSpace(r[key])
		if v == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UTC()
		}
		if t, err := time.Parse("2006-01-02 15:04:05", v); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func paymentOrderPaidForReport(r map[string]string) bool {
	if strings.TrimSpace(r["applied_at"]) != "" || strings.TrimSpace(r["paid_at"]) != "" {
		return true
	}
	status := strings.ToLower(strings.TrimSpace(r["status"]))
	switch status {
	case "approved", "paid", "confirmed", "success", "succeeded":
		return true
	}
	return false
}

func paymentReportUsername(payload string) string {
	data := map[string]any{}
	_ = json.Unmarshal([]byte(payload), &data)
	for _, key := range []string{"username", "renewed_username", "user", "account", "login"} {
		v := strings.TrimSpace(fmt.Sprint(data[key]))
		if v != "" && v != "<nil>" {
			return v
		}
	}
	return ""
}

func paymentReportMonths(monthsRaw, daysRaw string) string {
	months, _ := strconv.Atoi(strings.TrimSpace(monthsRaw))
	days, _ := strconv.Atoi(strings.TrimSpace(daysRaw))
	if months <= 0 && days > 0 {
		months = days / 30
		if months <= 0 {
			months = 1
		}
	}
	if months <= 0 {
		months = 1
	}
	if months == 1 {
		return "1 mês"
	}
	return fmt.Sprintf("%d meses", months)
}

func (b *Bot) startPaymentPlanMonths(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !paymentsManageAllowed(ctx, b.svc.Store, actor) {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin, revenda ou subrevenda.", backKeyboard(), "flow")
	}
	_ = b.setFlow(ctx, actor, chatID, "payment_month_1", map[string]string{})
	return b.sendOrEdit(ctx, chatID, msgID, "📅 <b>Meses</b>\n━━━━━━━━━━━━━━\nQual valor deseja colocar no plano <b>Mensal / 1 mês</b>?\n\nExemplo: <code>40</code>", kb([]Button{{"⬅️ Voltar", "payments_config"}}), "flow")
}

func (b *Bot) showPaymentLimitPackages(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !paymentLimitConfigAllowed(actor) {
		return b.showPaymentsConfig(ctx, actor, chatID, msgID)
	}
	ownerID := paymentOwnerID(actor)
	pkgs, _ := b.svc.Store.ListPaymentPackages(ctx, ownerID, false)
	var sb strings.Builder
	sb.WriteString("📦 <b>Planos:</b>\n━━━━━━━━━━━━━━\n")
	shown := false
	for _, p := range pkgs {
		if p.Kind == "limit" || p.Kind == "renew_limit" {
			fmt.Fprintf(&sb, "• <b>%s</b> | R$ %.2f | Limite: %d\n", h(p.Name), p.Amount, p.Credits)
			shown = true
		}
	}
	if !shown {
		sb.WriteString("Nenhum pacote cadastrado.\n")
	}
	return b.sendOrEdit(ctx, chatID, msgID, sb.String(), kb([]Button{{"➕ Criar pacote", "payment_limit_create"}}, []Button{{"⬅️ Voltar", "payments_config"}}), "submenu")
}

func (b *Bot) startPaymentLimitPackage(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !paymentLimitConfigAllowed(actor) {
		return b.showPaymentsConfig(ctx, actor, chatID, msgID)
	}
	if !paymentsManageAllowed(ctx, b.svc.Store, actor) {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin, revenda ou subrevenda.", backKeyboard(), "flow")
	}
	_ = b.setFlow(ctx, actor, chatID, "payment_limit_name", map[string]string{})
	return b.sendOrEdit(ctx, chatID, msgID, "Digite o nome do pacote:", kb([]Button{{"⬅️ Voltar", "payment_limit_menu"}}), "flow")
}

func myResellerKeyboard(renewalNotice bool) InlineKeyboardMarkup {
	noticeLabel := "🔔 Avisos de renovação [OFF]"
	if renewalNotice {
		noticeLabel = "🔔 Avisos de renovação [ON]"
	}
	return kb(
		[]Button{{"♻️ Renovar revenda", "my_reseller_renew"}, {"➕ Comprar limites", "my_reseller_limits"}},
		[]Button{{noticeLabel, "my_reseller_renewal_notice_toggle"}},
		[]Button{{"🧾 Meus pedidos", "my_payment_orders"}},
		[]Button{{"⬅️ Voltar", "menu_home"}},
	)
}

func paymentSellerIDForBuyer(r model.Reseller) int64 {
	if r.ParentTelegramID > 0 {
		return r.ParentTelegramID
	}
	return 0
}

func (b *Bot) getBuyerPaymentConfig(ctx context.Context, r model.Reseller) (int64, *model.PaymentOwnerConfig, error) {
	sellerID := paymentSellerIDForBuyer(r)
	cfg, _ := b.svc.Store.FindPaymentOwnerConfig(ctx, sellerID)
	if cfg == nil || !cfg.Enabled {
		return sellerID, cfg, errors.New("Pagamentos automáticos ainda não estão ativados para sua revenda")
	}
	if strings.TrimSpace(cfg.Bank) == "" {
		return sellerID, cfg, errors.New("Banco Pix não configurado pelo vendedor")
	}
	return sellerID, cfg, nil
}

func (b *Bot) showAutoPaymentPlans(ctx context.Context, actor model.Actor, chatID int64, msgID int, kind string) error {
	if actor.IsAdmin || actor.TelegramID == 0 {
		return b.showPayments(ctx, actor, chatID, msgID)
	}
	r, _ := b.svc.Store.FindReseller(ctx, actor.TelegramID)
	if r == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Revenda não encontrada.", kb([]Button{{"⬅️ Voltar", "menu_myreseller"}}), "flow")
	}
	sellerID, cfg, err := b.getBuyerPaymentConfig(ctx, *r)
	title := "♻️ <b>Renovar revenda</b>"
	if kind == "limit" {
		title = "➕ <b>Comprar mais limites</b>"
	}
	if err != nil {
		text := strings.Join([]string{
			"🚀 <b>GESTOR PRIMECEL</b>",
			"━━━━━━PAGAMENTO━━━━━━",
			title,
			"━━━━━━━━━━━━━━━━━━",
			"❌ " + h(err.Error()) + ".",
		}, "\n")
		return b.sendOrEdit(ctx, chatID, msgID, text, kb([]Button{{"⬅️ Voltar", "menu_myreseller"}}), "flow")
	}
	if kind == "renew" {
		monthly := r.MonthlyPrice
		if monthly <= 0 {
			_, err := b.svc.Resellers.Renew(ctx, model.Actor{Role: model.RoleAdmin, IsAdmin: true}, r.TelegramID, 30, 0, nil)
			if err != nil {
				text := strings.Join([]string{
					"🚀 <b>GESTOR PRIMECEL</b>",
					"━━━━━━PAGAMENTO━━━━━━",
					title,
					"━━━━━━━━━━━━━━━━━━",
					"❌ Não foi possível renovar automaticamente:",
					"<code>" + h(err.Error()) + "</code>",
				}, "\n")
				return b.sendOrEdit(ctx, chatID, msgID, text, kb([]Button{{"⬅️ Voltar", "menu_myreseller"}}), "flow")
			}
			text := strings.Join([]string{
				"🚀 <b>GESTOR PRIMECEL</b>",
				"━━━━━━PAGAMENTO━━━━━━",
				title,
				"━━━━━━━━━━━━━━━━━━",
				"✅ Renovação liberada automaticamente por +30 dias.",
			}, "\n")
			return b.sendOrEdit(ctx, chatID, msgID, text, kb([]Button{{"⬅️ Voltar", "menu_myreseller"}}), "flow")
		}
		lines := []string{
			"🚀 <b>GESTOR PRIMECEL</b>",
			"━━━━━━PAGAMENTO━━━━━━",
			title,
			"━━━━━━━━RESUMO━━━━━━━━",
			fmt.Sprintf("👤 Vendedor: <b>%s</b>", h(paymentOwnerLabel(ctx, b.svc.Store, sellerID))),
			fmt.Sprintf("🏦 Banco Pix: <b>%s</b>", h(paymentBankName(cfg.Bank))),
			fmt.Sprintf("📳 Limite atual: <code>%d</code>", r.Credits),
			fmt.Sprintf("💰 Valor mensal: <code>%s</code>", h(moneyBR(monthly))),
			"━━━━━━━━PLANOS━━━━━━━━",
		}
		rows := [][]Button{}
		for _, months := range []int{1, 2, 3} {
			price := b.renewPaymentPrice(ctx, sellerID, monthly, months)
			lines = append(lines, fmt.Sprintf("• <b>%d mês(es)</b> — <code>%s</code>", months, h(moneyBR(price))))
			rows = append(rows, []Button{{fmt.Sprintf("♻️ %d mês | %s", months, moneyBR(price)), fmt.Sprintf("pay_renew_month:%d", months)}})
		}
		lines = append(lines, "━━━━━━━━━━━━━━━━━━", "Escolha um plano abaixo para gerar o Pix.")
		rows = append(rows, []Button{{"⬅️ Voltar", "menu_myreseller"}})
		return b.sendOrEdit(ctx, chatID, msgID, strings.Join(lines, "\n"), kb(rows...), "submenu")
	}
	pkgs, _ := b.svc.Store.ListPaymentPackages(ctx, sellerID, true)
	lines := []string{
		"🚀 <b>GESTOR PRIMECEL</b>",
		"━━━━━━PAGAMENTO━━━━━━",
		title,
		"━━━━━━━━RESUMO━━━━━━━━",
		fmt.Sprintf("👤 Vendedor: <b>%s</b>", h(paymentOwnerLabel(ctx, b.svc.Store, sellerID))),
		fmt.Sprintf("🏦 Banco Pix: <b>%s</b>", h(paymentBankName(cfg.Bank))),
		fmt.Sprintf("📳 Limite atual: <code>%d</code>", r.Credits),
		fmt.Sprintf("💰 Valor mensal atual: <code>%s</code>", h(moneyBR(r.MonthlyPrice))),
		"━━━━━━━━PACOTES━━━━━━━━",
	}
	rows := [][]Button{}
	for _, p := range pkgs {
		if p.Kind != payments.KindLimit && p.Kind != payments.KindRenewLimit {
			continue
		}
		newLimit := r.Credits + p.Credits
		lines = append(lines,
			fmt.Sprintf("📦 <b>%s</b>", h(p.Name)),
			fmt.Sprintf("   Atual: <code>%d</code> | Contratado: <code>+%d</code> | Total: <code>%d</code>", r.Credits, p.Credits, newLimit),
			fmt.Sprintf("   Pix: <code>%s</code>", h(moneyBR(p.Amount))),
		)
		rows = append(rows, []Button{{fmt.Sprintf("📦 %s | +%d | %s", p.Name, p.Credits, moneyBR(p.Amount)), fmt.Sprintf("pay_limit_pkg:%d", p.ID)}})
	}
	if len(rows) == 0 {
		lines = append(lines, "Nenhum pacote de limites cadastrado pelo vendedor.")
	} else {
		lines = append(lines, "━━━━━━━━━━━━━━━━━━", "Escolha um pacote abaixo para gerar o Pix.")
	}
	rows = append(rows, []Button{{"⬅️ Voltar", "menu_myreseller"}})
	return b.sendOrEdit(ctx, chatID, msgID, strings.Join(lines, "\n"), kb(rows...), "submenu")
}
func (b *Bot) renewPaymentPrice(ctx context.Context, ownerID int64, monthly float64, months int) float64 {
	pkgs, _ := b.svc.Store.ListPaymentPackages(ctx, ownerID, true)
	for _, p := range pkgs {
		if p.Kind == payments.KindRenew && p.Months == months && p.Amount > 0 {
			return p.Amount
		}
	}
	return monthly * float64(months)
}

func (b *Bot) createRenewPaymentOrder(ctx context.Context, actor model.Actor, chatID int64, msgID int, months int) error {
	if months <= 0 {
		months = 1
	}
	if actor.IsAdmin || actor.TelegramID == 0 {
		return b.showPayments(ctx, actor, chatID, msgID)
	}
	r, _ := b.svc.Store.FindReseller(ctx, actor.TelegramID)
	if r == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Revenda não encontrada.", kb([]Button{{"⬅️ Voltar", "menu_myreseller"}}), "flow")
	}
	sellerID, cfg, err := b.getBuyerPaymentConfig(ctx, *r)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ "+h(err.Error()), kb([]Button{{"⬅️ Voltar", "menu_myreseller"}}), "flow")
	}
	amount := b.renewPaymentPrice(ctx, sellerID, r.MonthlyPrice, months)
	if amount <= 0 {
		return b.showAutoPaymentPlans(ctx, actor, chatID, msgID, "renew")
	}
	mgr := payments.NewManager(b.svc.Store)
	order, err := mgr.CreateOrder(ctx, payments.OrderInput{OwnerID: sellerID, TargetResellerID: r.TelegramID, Kind: payments.KindRenew, Months: months, Days: months * 30, Amount: amount, Bank: cfg.Bank, Description: fmt.Sprintf("Renovação %d mês(es) - %s", months, r.Name)})
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Não foi possível gerar o Pix automático.\n\n<code>"+h(err.Error())+"</code>", kb([]Button{{"⬅️ Voltar", "my_reseller_renew"}}), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, paymentOrderText(ctx, b.svc.Store, *order), kb([]Button{{"🧾 Meus pedidos", "my_payment_orders"}}, []Button{{"⬅️ Voltar", "menu_myreseller"}}), "flow")
}

func (b *Bot) createLimitPaymentOrder(ctx context.Context, actor model.Actor, chatID int64, msgID int, packageID int64) error {
	if actor.IsAdmin || actor.TelegramID == 0 {
		return b.showPayments(ctx, actor, chatID, msgID)
	}
	r, _ := b.svc.Store.FindReseller(ctx, actor.TelegramID)
	if r == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Revenda não encontrada.", kb([]Button{{"⬅️ Voltar", "menu_myreseller"}}), "flow")
	}
	sellerID, cfg, err := b.getBuyerPaymentConfig(ctx, *r)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ "+h(err.Error()), kb([]Button{{"⬅️ Voltar", "menu_myreseller"}}), "flow")
	}
	pkgs, _ := b.svc.Store.ListPaymentPackages(ctx, sellerID, true)
	var pkg *model.PaymentPackage
	for i := range pkgs {
		if pkgs[i].ID == packageID {
			pkg = &pkgs[i]
			break
		}
	}
	if pkg == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Plano não encontrado.", kb([]Button{{"⬅️ Voltar", "my_reseller_limits"}}), "flow")
	}
	mgr := payments.NewManager(b.svc.Store)
	order, err := mgr.CreateOrder(ctx, payments.OrderInput{OwnerID: sellerID, TargetResellerID: r.TelegramID, Kind: payments.KindLimit, Months: pkg.Months, Days: pkg.Days, Credits: pkg.Credits, Amount: pkg.Amount, Bank: cfg.Bank, Description: fmt.Sprintf("Compra de %d limite(s) - %s", pkg.Credits, r.Name)})
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "❌ Não foi possível gerar o Pix automático.\n\n<code>"+h(err.Error())+"</code>", kb([]Button{{"⬅️ Voltar", "my_reseller_limits"}}), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, paymentOrderText(ctx, b.svc.Store, *order), kb([]Button{{"🧾 Meus pedidos", "my_payment_orders"}}, []Button{{"⬅️ Voltar", "menu_myreseller"}}), "flow")
}

func (b *Bot) showMyPaymentOrders(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showMyPaymentOrdersPage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) showMyPaymentOrdersPage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	if actor.IsAdmin || actor.TelegramID == 0 {
		return b.showPayments(ctx, actor, chatID, msgID)
	}
	orders, _ := b.svc.Store.ListPaymentOrdersByTarget(ctx, actor.TelegramID, "")
	page, pages, start, end := paginateBounds(len(orders), page, listPageSize)
	var sb strings.Builder
	sb.WriteString("🚀 <b>GESTOR PRIMECEL</b>\n━━━━━━MEUS PEDIDOS━━━━━━\n")
	if len(orders) == 0 {
		sb.WriteString("Nenhum pedido encontrado.\n")
	}
	for _, o := range orders[start:end] {
		fmt.Fprintf(&sb, "• <code>%s</code> | %s | <b>%s</b> | %s\n", h(o.OrderID), h(paymentKindText(o.Kind)), h(moneyBR(o.Amount)), h(paymentStatusText(o.Status, o.AppliedAt != nil)))
	}
	sb.WriteString(pageIndicator(page, pages))
	return b.sendOrEdit(ctx, chatID, msgID, sb.String(), pagedListKeyboard("my_payment_orders_page", page, pages, "menu_myreseller"), "submenu")
}

func paymentOrderText(ctx context.Context, st *store.DB, o model.PaymentOrder) string {
	bank := paymentBankName(o.Bank)
	title := "💳 <b>Pedido Pix</b>"
	var paymentBlock []string
	if o.PixCopyPaste != "" {
		paymentBlock = append(paymentBlock, "<b>Pix copia e cola:</b>", "<code>"+h(o.PixCopyPaste)+"</code>")
	}
	if o.PaymentURL != "" {
		if o.PixCopyPaste == "" {
			title = "💳 <b>Pedido de pagamento</b>"
		}
		paymentBlock = append(paymentBlock, "", "<b>Link de pagamento:</b>", h(o.PaymentURL))
	}
	if len(paymentBlock) == 0 {
		paymentBlock = append(paymentBlock, "⚠️ Pix/link ainda não retornado pelo banco. Consulte os pedidos em alguns instantes.")
	}
	lines := []string{
		"🚀 <b>GESTOR PRIMECEL</b>",
		"━━━━━━PAGAMENTO━━━━━━",
		title,
		"━━━━━━━━RESUMO━━━━━━━━",
		"🧾 Pedido: <code>" + h(o.OrderID) + "</code>",
		"📦 Tipo: <b>" + h(paymentKindText(o.Kind)) + "</b>",
		"🏦 Banco Pix: <b>" + h(bank) + "</b>",
		"💰 Valor exato: <code>" + h(moneyBR(o.Amount)) + "</code>",
	}
	if details := paymentOrderDetails(o); details != "" {
		lines = append(lines, strings.Split(strings.TrimSpace(details), "\n")...)
	}
	lines = append(lines,
		"━━━━━━━━PIX━━━━━━━━",
	)
	lines = append(lines, paymentBlock...)
	lines = append(lines,
		"━━━━━━━━AVISO━━━━━━━━",
		"✅ Após a aprovação, o WebHook libera automaticamente.",
		"🛡️ Não pague valor diferente do exibido.",
	)
	return strings.Join(lines, "\n")
}
func paymentOrderDetails(o model.PaymentOrder) string {
	var lines []string
	if o.Kind == payments.KindRenew || o.Kind == payments.KindRenewLimit {
		lines = append(lines, fmt.Sprintf("♻️ Renovação: <code>%d mês(es)</code>", o.Months), fmt.Sprintf("📅 Dias: <code>%d</code>", o.Days))
	}
	if o.Kind == payments.KindLimit || o.Kind == payments.KindRenewLimit {
		lines = append(lines, fmt.Sprintf("📳 Limites: <code>+%d</code>", o.Credits))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}
func paymentKindText(k string) string {
	switch k {
	case payments.KindRenew:
		return "Renovação"
	case payments.KindLimit:
		return "Limites"
	case payments.KindRenewLimit:
		return "Renovação + Limites"
	default:
		return k
	}
}
func paymentStatusText(status string, applied bool) string {
	if applied {
		return "aprovado/liberado"
	}
	if strings.TrimSpace(status) == "" {
		return "pendente"
	}
	return status
}
func moneyBR(v float64) string { return "R$ " + strings.ReplaceAll(fmt.Sprintf("%.2f", v), ".", ",") }

func (b *Bot) showNotices(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, "📢 <b>Avisos</b>\n━━━━━━━━━━━━━━\n1. 📢 Aviso\n2. 🆕 Novidades\n\nEscolha o tipo de mensagem.", noticesKeyboard(), "submenu")
}

func (b *Bot) startNotice(ctx context.Context, actor model.Actor, chatID int64, msgID int, kind string) error {
	if !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Acesso permitido somente ao admin.", backKeyboard(), "flow")
	}
	_ = b.setFlow(ctx, actor, chatID, "notice_message", map[string]string{"kind": kind})
	return b.sendOrEdit(ctx, chatID, msgID, "📢 Avisos\n━━━━━━━━━━━━━━\nDigite a mensagem para enviar às revendas/subrevendas.", backKeyboard(), "flow")
}

type noticeSender struct{ client *Client }

func (s noticeSender) SendMessage(ctx context.Context, chatID int64, text string) error {
	_, err := s.client.SendMessage(ctx, chatID, text, InlineKeyboardMarkup{})
	return err
}

func (b *Bot) showMyReseller(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if actor.IsAdmin || actor.TelegramID == 0 {
		return b.showMain(ctx, actor, chatID, msgID)
	}
	r, _ := b.svc.Store.FindReseller(ctx, actor.TelegramID)
	if r == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Revenda não encontrada.", backKeyboard(), "flow")
	}
	renewalNotice := b.currentPaymentRenewalNoticeEnabled(ctx, actor.TelegramID)
	renewalBadge := "🔴 OFF"
	if renewalNotice {
		renewalBadge = "🟢 ON"
	}
	text := resellerPanelText(*r, b.svc.Resellers.BlockReason(ctx, r))
	text += "\n\n💳 <b>Pagamento automático via Pix</b>\n━━━━━━━━━━━━━━\nUse os botões abaixo para renovar sua revenda ou comprar mais limites. O Pix é gerado pelo vendedor responsável e, após aprovação do WebHook, o sistema libera automaticamente."
	text += "\n\n🔔 <b>Avisos de renovação de contas:</b> " + h(renewalBadge)
	return b.sendOrEdit(ctx, chatID, msgID, text, myResellerKeyboard(renewalNotice), "flow")
}

// Telegram API client.
type Client struct {
	token string
	base  string
	http  *http.Client
}

func NewClient(token string) *Client {
	return &Client{token: token, base: "https://api.telegram.org/bot" + token + "/", http: &http.Client{Timeout: 35 * time.Second}}
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}
type Message struct {
	MessageID int       `json:"message_id"`
	From      User      `json:"from"`
	Chat      Chat      `json:"chat"`
	Text      string    `json:"text"`
	Document  *Document `json:"document"`
}
type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}
type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
	FilePath     string `json:"file_path"`
}
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}
type Chat struct {
	ID int64 `json:"id"`
}
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard,omitempty"`
}
type InlineKeyboardButton struct {
	Text         string          `json:"text"`
	CallbackData string          `json:"callback_data,omitempty"`
	CopyText     *CopyTextButton `json:"copy_text,omitempty"`
}
type CopyTextButton struct {
	Text string `json:"text"`
}

type apiResp[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
}

func (c *Client) do(ctx context.Context, method string, payload any, out any) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram %s: %s", method, string(body))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return err
		}
	}
	return nil
}
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var r apiResp[[]Update]
	err := c.do(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": timeout, "allowed_updates": []string{"message", "callback_query"}}, &r)
	if err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, errors.New(r.Description)
	}
	return r.Result, nil
}
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, kb InlineKeyboardMarkup) (*Message, error) {
	formatted := formatTelegramOutgoing(text)
	msg, err := c.sendMessageRaw(ctx, chatID, formatted, kb)
	if err != nil && formatted != text {
		return c.sendMessageRaw(ctx, chatID, text, kb)
	}
	return msg, err
}
func (c *Client) sendMessageRaw(ctx context.Context, chatID int64, text string, kb InlineKeyboardMarkup) (*Message, error) {
	var r apiResp[Message]
	payload := map[string]any{"chat_id": chatID, "text": text, "reply_markup": kb}
	if pm := telegramParseMode(text); pm != "" {
		payload["parse_mode"] = pm
	}
	err := c.do(ctx, "sendMessage", payload, &r)
	if err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, errors.New(r.Description)
	}
	return &r.Result, nil
}
func (c *Client) EditMessageText(ctx context.Context, chatID int64, msgID int, text string, kb InlineKeyboardMarkup) error {
	formatted := formatTelegramOutgoing(text)
	if err := c.editMessageTextRaw(ctx, chatID, msgID, formatted, kb); err != nil {
		if formatted != text {
			return c.editMessageTextRaw(ctx, chatID, msgID, text, kb)
		}
		return err
	}
	return nil
}
func (c *Client) editMessageTextRaw(ctx context.Context, chatID int64, msgID int, text string, kb InlineKeyboardMarkup) error {
	var r apiResp[json.RawMessage]
	payload := map[string]any{"chat_id": chatID, "message_id": msgID, "text": text, "reply_markup": kb}
	if pm := telegramParseMode(text); pm != "" {
		payload["parse_mode"] = pm
	}
	err := c.do(ctx, "editMessageText", payload, &r)
	if err != nil {
		return err
	}
	if !r.OK {
		return errors.New(r.Description)
	}
	return nil
}
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	var r apiResp[json.RawMessage]
	return c.do(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, &r)
}

func (c *Client) DeleteMessage(ctx context.Context, chatID int64, msgID int) error {
	if chatID == 0 || msgID == 0 {
		return nil
	}
	var r apiResp[json.RawMessage]
	err := c.do(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": msgID}, &r)
	if err != nil {
		return err
	}
	if !r.OK {
		return errors.New(r.Description)
	}
	return nil
}

func (c *Client) GetFile(ctx context.Context, fileID string) (*File, error) {
	var r apiResp[File]
	err := c.do(ctx, "getFile", map[string]any{"file_id": fileID}, &r)
	if err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, errors.New(r.Description)
	}
	return &r.Result, nil
}

func (c *Client) DownloadFile(ctx context.Context, fileID, dest string) error {
	f, err := c.GetFile(ctx, fileID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(f.FilePath) == "" {
		return errors.New("Telegram não retornou file_path")
	}
	url := "https://api.telegram.org/file/bot" + c.token + "/" + f.FilePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram download: %s", string(body))
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func (c *Client) SendDocument(ctx context.Context, chatID int64, path, fileID, caption, fileName string) (*Message, error) {
	if strings.TrimSpace(fileID) != "" {
		var r apiResp[Message]
		err := c.do(ctx, "sendDocument", map[string]any{"chat_id": chatID, "document": fileID, "caption": caption}, &r)
		if err == nil && r.OK {
			return &r.Result, nil
		}
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("arquivo do aplicativo não encontrado")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	if strings.TrimSpace(fileName) == "" {
		fileName = filepath.Base(path)
	}
	part, err := mw.CreateFormFile("document", fileName)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"sendDocument", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("telegram sendDocument: %s", string(b))
	}
	var r apiResp[Message]
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, errors.New(r.Description)
	}
	return &r.Result, nil
}

func (b *Bot) showResellerList(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	return b.showResellerListPage(ctx, actor, chatID, msgID, 0)
}

func (b *Bot) resellerListPagePayload(ctx context.Context, actor model.Actor, page int) (string, InlineKeyboardMarkup) {
	rs, _ := b.svc.Store.ListResellers(ctx)
	accs, _ := b.svc.Store.ListAccounts(ctx, false)
	usedByReseller := resellerUsedCredits(accs)
	parentNames := map[int64]string{}
	for _, r := range rs {
		parentNames[r.TelegramID] = r.Name
	}
	rows := []model.Reseller{}
	for _, r := range rs {
		if resellerVisible(actor, r) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name) })
	page, pages, start, end := paginateBounds(len(rows), page, listPageSize)
	var sb strings.Builder
	title := "👥 Revendas"
	if !actor.IsAdmin && actor.Role == model.RoleReseller {
		title = "👥 SubRevendas"
	}
	fmt.Fprintf(&sb, "%s [%d]\n━━━━━━━━━━━━━━\n", title, len(rows))
	if len(rows) == 0 {
		sb.WriteString("Nenhum registro encontrado.\n")
		sb.WriteString("━━━━━━━━━━━━━━")
	} else {
		pageRows := rows[start:end]
		for idx, r := range pageRows {
			used := usedByReseller[r.TelegramID]
			total := used + r.Credits
			if total < used {
				total = used
			}
			owner := "Admin"
			if r.ParentTelegramID != 0 {
				owner = nonEmpty(parentNames[r.ParentTelegramID], "Admin")
			}
			fmt.Fprintf(&sb, "👤 <code>%s</code> • 📳 %d/%d\n", h(r.Name), used, total)
			fmt.Fprintf(&sb, "⏳ %s • 👑 %s\n", h(daysLeftLong(r.ExpiresAt)), h(owner))
			sb.WriteString("━━━━━━━━━━━━━━")
			if idx < len(pageRows)-1 {
				sb.WriteByte('\n')
			}
		}
		if len(pageRows) > 0 {
			sb.WriteString("\nUse <code>/editarev</code> usuario para renovar")
		}
	}
	sb.WriteString(pageIndicator(page, pages))
	return sb.String(), pagedListKeyboard("resellers_page", page, pages, "menu_resellers")
}

func (b *Bot) showResellerListPage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	text, kb := b.resellerListPagePayload(ctx, actor, page)
	return b.sendOrEdit(ctx, chatID, msgID, text, kb, fmt.Sprintf("live_resellers_list:%d", page))
}

func (b *Bot) expiredResellerListPagePayload(ctx context.Context, actor model.Actor, page int) (string, InlineKeyboardMarkup) {
	rs, _ := b.svc.Store.ListResellers(ctx)
	accs, _ := b.svc.Store.ListAccounts(ctx, false)
	usedByReseller := resellerUsedCredits(accs)
	parentNames := map[int64]string{}
	for _, r := range rs {
		parentNames[r.TelegramID] = r.Name
	}
	now := time.Now().UTC()
	rows := []model.Reseller{}
	for _, r := range rs {
		if r.DeletedAt != nil {
			continue
		}
		if !resellerVisible(actor, r) {
			continue
		}
		if r.ExpiresAt.IsZero() || r.ExpiresAt.After(now) {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ExpiresAt.Before(rows[j].ExpiresAt) })
	page, pages, start, end := paginateBounds(len(rows), page, listPageSize)
	var sb strings.Builder
	title := "🚫 Revendas Expiradas"
	if !actor.IsAdmin && actor.Role == model.RoleReseller {
		title = "🚫 SubRevendas Expiradas"
	}
	fmt.Fprintf(&sb, "%s [%d]\n━━━━━━━━━━━━━━\n", title, len(rows))
	if len(rows) == 0 {
		if !actor.IsAdmin && actor.Role == model.RoleReseller {
			sb.WriteString("Nenhuma subrevenda expirada.\n")
		} else {
			sb.WriteString("Nenhuma revenda expirada.\n")
		}
	} else {
		pageRows := rows[start:end]
		for idx, r := range pageRows {
			used := usedByReseller[r.TelegramID]
			total := used + r.Credits
			if total < used {
				total = used
			}
			owner := "Admin"
			if r.ParentTelegramID != 0 {
				owner = nonEmpty(parentNames[r.ParentTelegramID], "Admin")
			}
			fmt.Fprintf(&sb, "👤 <code>%s</code> • 📳 %d/%d\n", h(r.Name), used, total)
			fmt.Fprintf(&sb, "⌛ %s • 👑 %s\n", h(expiredFor(r.ExpiresAt)), h(owner))
			sb.WriteString("━━━━━━━━━━━━━━")
			if idx < len(pageRows)-1 {
				sb.WriteByte('\n')
			}
		}
	}
	sb.WriteString(pageIndicator(page, pages))
	sb.WriteString("\n⚠️ Prazo: até 7 dias. Remove automático!")
	if len(rows) > 0 {
		sb.WriteString("\n\nUse <code>/editarev</code> usuario para renovar")
	}
	return sb.String(), expiredResellerListKeyboard(page, pages)
}

func (b *Bot) showExpiredResellerListPage(ctx context.Context, actor model.Actor, chatID int64, msgID int, page int) error {
	text, kb := b.expiredResellerListPagePayload(ctx, actor, page)
	return b.sendOrEdit(ctx, chatID, msgID, text, kb, fmt.Sprintf("live_resellers_expired:%d", page))
}

func (b *Bot) startCreateReseller(ctx context.Context, actor model.Actor, chatID int64, msgID int) error {
	if actor.Role == model.RoleSubReseller && !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ SubRevenda não pode criar, editar, renovar, suspender ou remover outra SubRevenda.", backKeyboard(), "flow")
	}
	if actor.Role == model.RoleReseller && !actor.IsAdmin {
		parent, _ := b.svc.Store.FindReseller(ctx, actor.TelegramID)
		if parent == nil || !parent.AllowSubReseller {
			return b.sendOrEdit(ctx, chatID, msgID, "⛔ Sua revenda não tem permissão para criar SubRevenda.", backKeyboard(), "flow")
		}
	}
	_ = b.setFlow(ctx, actor, chatID, "res_create_id", map[string]string{})
	title := "➕ Criar Revenda"
	if !actor.IsAdmin {
		title = "➕ Criar SubRevenda"
	}
	return b.sendOrEdit(ctx, chatID, msgID, title+"\n━━━━━━━━━━━━━━\nDigite o ID Telegram.", backKeyboard(), "flow")
}

func (b *Bot) handleCreateResellerXray(ctx context.Context, actor model.Actor, chatID int64, msgID int, allow bool) error {
	st, err := b.getFlow(ctx, actor.TelegramID)
	if err != nil || st.State != "res_create_xray" {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Fluxo expirado. Comece novamente.", resellersKeyboard(actor), "flow")
	}
	st.Data["allow_xray"] = boolText(allow)
	_ = b.setFlow(ctx, actor, chatID, "res_create_sub", st.Data)
	return b.sendOrEdit(ctx, chatID, msgID, "👥 Permitir que esta revenda crie SubRevendas?", yesNoResellerSubKeyboard(), "flow")
}

func (b *Bot) finishCreateReseller(ctx context.Context, actor model.Actor, chatID int64, msgID int, allowSub bool) error {
	st, err := b.getFlow(ctx, actor.TelegramID)
	if err != nil || st.State != "res_create_sub" {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Fluxo expirado. Comece novamente.", resellersKeyboard(actor), "flow")
	}
	st.Data["allow_sub"] = boolText(allowSub)
	r, err := b.createResellerFromData(ctx, actor, st.Data)
	_ = b.clearFlow(ctx, actor.TelegramID)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao criar revenda: "+err.Error(), resellersKeyboard(actor), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, resellerCreatedText(*r, false), createdResellerKeyboardCopy(*r, false), "flow")
}

func (b *Bot) createSubResellerNow(ctx context.Context, actor model.Actor, chatID int64, data map[string]string) error {
	r, err := b.createResellerFromData(ctx, actor, data)
	_ = b.clearFlow(ctx, actor.TelegramID)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, 0, "⚠️ Erro ao criar SubRevenda: "+err.Error(), resellersKeyboard(actor), "flow")
	}
	return b.sendOrEdit(ctx, chatID, 0, resellerCreatedText(*r, true), createdResellerKeyboardCopy(*r, true), "flow")
}

func (b *Bot) showResellerCopyCreated(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	r, err := b.svc.Store.FindReseller(ctx, id)
	if err != nil || r == nil || !resellerVisible(actor, *r) {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Revenda/SubRevenda não encontrada.", backKeyboard(), "flow")
	}
	kind := "Revenda"
	if r.Level > 0 || r.ParentTelegramID != 0 {
		kind = "SubRevenda"
	}
	text := fmt.Sprintf("📋 Dados da %s\n━━━━━━━━━━━━━━\nNome: %s\nID Telegram: %d\nWhatsApp: %s\nLimite: %d\nValidade: %s\nValor Mensal: R$ %.2f", kind, r.Name, r.TelegramID, r.WhatsAppPhone, r.Credits, daysLeft(r.ExpiresAt), r.MonthlyPrice)
	return b.sendOrEdit(ctx, chatID, msgID, text, createdResellerKeyboard(r.TelegramID), "flow")
}

func (b *Bot) createResellerFromData(ctx context.Context, actor model.Actor, data map[string]string) (*model.Reseller, error) {
	id, _ := strconv.ParseInt(data["telegram_id"], 10, 64)
	credits, _ := strconv.Atoi(data["credits"])
	days, _ := strconv.Atoi(data["validity_days"])
	price, _ := strconv.ParseFloat(data["monthly_price"], 64)
	d := resellers.CreateDraft{TelegramID: id, Name: data["name"], WhatsAppPhone: data["whatsapp"], Credits: credits, ValidityDays: days, MaxDays: 3650, MaxLimit: 1, MonthlyPrice: price, AllowXray: data["allow_xray"] == "1", AllowSubReseller: data["allow_sub"] == "1"}
	return b.svc.Resellers.Create(ctx, actor, d)
}

func (b *Bot) startResellerLookup(ctx context.Context, actor model.Actor, chatID int64, msgID int, action string) error {
	if actor.Role == model.RoleSubReseller && !actor.IsAdmin {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ SubRevenda não pode criar, editar, renovar, suspender ou remover outra SubRevenda.", backKeyboard(), "flow")
	}
	labels := map[string]string{"edit": "✏️ Editar Revenda", "renew": "♻️ Renovar Revenda", "remove": "🗑️ Remover Revenda", "block": "🚫 Bloquear/Ativar"}
	_ = b.setFlow(ctx, actor, chatID, "res_lookup", map[string]string{"action": action})
	return b.sendOrEdit(ctx, chatID, msgID, labels[action]+"\n━━━━━━━━━━━━━━\nDigite o nome ou ID Telegram.", backKeyboard(), "flow")
}

func (b *Bot) visibleResellerByQuery(ctx context.Context, actor model.Actor, q string) (*model.Reseller, bool, string) {
	r, err := b.svc.Resellers.FindByQuery(ctx, q)
	if err != nil || r == nil {
		return nil, false, "⚠️ Revenda/SubRevenda não encontrada."
	}
	if !resellerVisible(actor, *r) || (actor.Role == model.RoleSubReseller && !actor.IsAdmin) {
		return nil, false, "⛔ Você não tem permissão para esta revenda."
	}
	return r, true, ""
}

func (b *Bot) showResellerPanelDirect(ctx context.Context, actor model.Actor, chatID int64, msgID int, q string) error {
	r, ok, msg := b.visibleResellerByQuery(ctx, actor, q)
	if !ok {
		return b.sendOrEdit(ctx, chatID, msgID, msg, backKeyboard(), "flow")
	}
	return b.showResellerPanel(ctx, actor, chatID, msgID, r.TelegramID)
}

func (b *Bot) showResellerPanel(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	r, err := b.svc.Store.FindReseller(ctx, id)
	if err != nil || r == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Revenda não encontrada.", backKeyboard(), "flow")
	}
	if !resellerVisible(actor, *r) {
		return b.sendOrEdit(ctx, chatID, msgID, "⛔ Sem permissão.", backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, resellerPanelText(*r, b.svc.Resellers.BlockReason(ctx, r)), resellerPanelKeyboard(actor, *r), "flow")
}

func (b *Bot) startResellerEditField(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64, field string) error {
	r, err := b.svc.Store.FindReseller(ctx, id)
	if err != nil || r == nil || !resellerVisible(actor, *r) {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Revenda não encontrada ou sem permissão.", backKeyboard(), "flow")
	}
	state := "res_edit_credits_state"
	prompt := "Digite o novo limite de acessos."
	if field == "price" {
		state = "res_edit_price_state"
		prompt = "Digite o novo valor mensal."
	} else if field == "whatsapp" {
		state = "res_edit_whatsapp_state"
		prompt = "Digite o novo WhatsApp com DDI."
	}
	_ = b.setFlow(ctx, actor, chatID, state, map[string]string{"telegram_id": strconv.FormatInt(id, 10)})
	return b.sendOrEdit(ctx, chatID, msgID, "✏️ "+r.Name+"\n━━━━━━━━━━━━━━\n"+prompt, backKeyboard(), "flow")
}

func (b *Bot) showResellerRenewOptions(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	r, err := b.svc.Store.FindReseller(ctx, id)
	if err != nil || r == nil || !resellerVisible(actor, *r) {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Revenda não encontrada ou sem permissão.", backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, "♻️ Renovar Revenda\n━━━━━━━━━━━━━━\n"+resellerMiniLine(*r)+"\n\nEscolha a validade:", resellerRenewKeyboard(id), "flow")
}

func (b *Bot) handleResellerRenewDays(ctx context.Context, actor model.Actor, chatID int64, msgID int, payload string) error {
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Renovação inválida.", resellersKeyboard(actor), "flow")
	}
	id, _ := strconv.ParseInt(parts[0], 10, 64)
	days, _ := strconv.Atoi(parts[1])
	r, err := b.svc.Resellers.Renew(ctx, actor, id, days, 0, nil)
	_ = b.clearFlow(ctx, actor.TelegramID)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao renovar: "+err.Error(), backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, "✅ Revenda renovada\n━━━━━━━━━━━━━━\n"+resellerMiniLine(*r), resellerPanelKeyboard(actor, *r), "flow")
}

func (b *Bot) toggleResellerActive(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	r, err := b.svc.Store.FindReseller(ctx, id)
	if err != nil || r == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Revenda não encontrada.", backKeyboard(), "flow")
	}
	updated, err := b.svc.Resellers.SetActive(ctx, actor, id, !r.Active)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro: "+err.Error(), backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, resellerPanelText(*updated, b.svc.Resellers.BlockReason(ctx, updated)), resellerPanelKeyboard(actor, *updated), "flow")
}

func (b *Bot) toggleResellerSub(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	r, err := b.svc.Store.FindReseller(ctx, id)
	if err != nil || r == nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Revenda não encontrada.", backKeyboard(), "flow")
	}
	updated, err := b.svc.Resellers.SetAllowSubReseller(ctx, actor, id, !r.AllowSubReseller)
	if err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro: "+err.Error(), backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, resellerPanelText(*updated, b.svc.Resellers.BlockReason(ctx, updated)), resellerPanelKeyboard(actor, *updated), "flow")
}

func (b *Bot) confirmDeleteReseller(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	r, err := b.svc.Store.FindReseller(ctx, id)
	if err != nil || r == nil || !resellerVisible(actor, *r) {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Revenda não encontrada ou sem permissão.", backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, "🗑️ Remover Revenda\n━━━━━━━━━━━━━━\n"+resellerMiniLine(*r)+"\n\nConfirma remover?", resellerDeleteKeyboard(id), "flow")
}

func (b *Bot) doDeleteReseller(ctx context.Context, actor model.Actor, chatID int64, msgID int, id int64) error {
	if err := b.svc.Resellers.Delete(ctx, actor, id); err != nil {
		return b.sendOrEdit(ctx, chatID, msgID, "⚠️ Erro ao remover: "+err.Error(), backKeyboard(), "flow")
	}
	return b.sendOrEdit(ctx, chatID, msgID, "✅ Revenda removida.", resellersKeyboard(actor), "flow")
}

func yesNoResellerXrayKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"✅ Sim", "res_xray_yes"}, {"❌ Não", "res_xray_no"}}, []Button{{"⬅️ Voltar", "menu_resellers"}})
}
func yesNoResellerWhatsAppKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"✅ Sim", "res_wa_yes"}, {"❌ Não", "res_wa_no"}}, []Button{{"⬅️ Voltar", "menu_resellers"}})
}
func yesNoResellerSubKeyboard() InlineKeyboardMarkup {
	return kb([]Button{{"✅ Sim", "res_sub_yes"}, {"❌ Não", "res_sub_no"}}, []Button{{"⬅️ Voltar", "menu_resellers"}})
}
func createdResellerKeyboard(id int64) InlineKeyboardMarkup {
	sid := strconv.FormatInt(id, 10)
	return kb([]Button{{"⬅️ Voltar", "menu_resellers"}, {"📋 Copiar", "res_copy_created:" + sid}})
}
func createdResellerKeyboardCopy(r model.Reseller, sub bool) InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "📋 Copiar", CopyText: &CopyTextButton{Text: resellerCardPlain(r, sub)}}},
		{{Text: "⬅️ Voltar", CallbackData: "menu_resellers"}},
	}}
}
func resellerPanelKeyboard(actor model.Actor, r model.Reseller) InlineKeyboardMarkup {
	sid := strconv.FormatInt(r.TelegramID, 10)
	rows := [][]Button{{{"✏️ Limite", "res_edit_credits:" + sid}, {"💰 Valor Mensal", "res_edit_price:" + sid}}, {{"📱 WhatsApp", "res_edit_wa:" + sid}, {"🚫 Bloquear/Ativar", "res_toggle_active:" + sid}}, {{"♻️ Renovar", "res_renew_start:" + sid}, {"🗑️ Remover", "res_delete_confirm:" + sid}}}
	if (actor.IsAdmin || actor.Role == model.RoleAdmin) && r.Level == 0 && r.ParentTelegramID == 0 {
		rows = append(rows, []Button{{"👥 SubRevenda", "res_toggle_sub:" + sid}})
	}
	if actor.TelegramID != 0 && actor.TelegramID != r.TelegramID {
		rows = append(rows, []Button{{"🔔 Avisos de renovação", "res_renew_notice:" + sid}})
	}
	rows = append(rows, []Button{{"⬅️ Voltar", "menu_resellers"}})
	return kb(rows...)
}

func resellerRenewalNoticeKeyboard(id int64) InlineKeyboardMarkup {
	sid := strconv.FormatInt(id, 10)
	return kb(
		[]Button{{"✅ Receber", "res_set_renew_notice:" + sid + ":1"}, {"❌ Não receber", "res_set_renew_notice:" + sid + ":0"}},
		[]Button{{"⬅️ Voltar", "res_view:" + sid}},
	)
}
func resellerRenewKeyboard(id int64) InlineKeyboardMarkup {
	sid := strconv.FormatInt(id, 10)
	return kb([]Button{{"1 mês", "res_renew_days:" + sid + ":30"}, {"2 meses", "res_renew_days:" + sid + ":60"}, {"3 meses", "res_renew_days:" + sid + ":90"}}, []Button{{"⬅️ Voltar", "res_view:" + sid}})
}
func resellerDeleteKeyboard(id int64) InlineKeyboardMarkup {
	sid := strconv.FormatInt(id, 10)
	return kb([]Button{{"✅ Confirmar", "res_do_delete:" + sid}, {"❌ Cancelar", "res_view:" + sid}})
}
func resellerPanelText(r model.Reseller, reason string) string {
	kind := "Revenda"
	if r.Level == 1 || r.ParentTelegramID != 0 {
		kind = "SubRevenda"
	}
	return fmt.Sprintf("👥 %s - %s\n━━━━━━━━━━━━━━\n🆔 ID: %d\n📳 Limite: %d\n📆 Tempo: %s\n💰 Valor: R$ %.2f\n━━━━━━━━━━━━━━\n🌐 Xray: %s\n👥 SubRevenda: %s\n📱 WhatsApp: %s", h(kind), h(r.Name), r.TelegramID, r.Credits, h(daysLeftLong(r.ExpiresAt)), r.MonthlyPrice, h(yesNoText(r.AllowXray)), h(yesNoText(r.AllowSubReseller)), h(nonEmptyText(r.WhatsAppPhone, "-")))
}
func resellerCreatedText(r model.Reseller, sub bool) string {
	title := "✅ Revenda criada"
	if sub {
		title = "✅ SubRevenda criada"
	}
	kind := "Revenda"
	if sub || r.Level == 1 || r.ParentTelegramID != 0 {
		kind = "SubRevenda"
	}
	return fmt.Sprintf("%s\n━━━━━━━━━━━━━━\n👑 Nome: <code>%s</code>\n🆔 ID Telegram: <code>%d</code>\n📱 WhatsApp: <code>%s</code>\n📳 Limite: %d\n━━━━━━━━━━━━━━\n💰 Valor: <code>%s</code>\n📆 Expira: <code>%s</code>\n━━━━━━━━━━━━━━\n👥 Tipo: <code>%s</code>", htmlTitle(title), h(r.Name), r.TelegramID, h(nonEmptyText(r.WhatsAppPhone, "-")), r.Credits, h(moneyBR(r.MonthlyPrice)), h(daysLeft(r.ExpiresAt)), h(kind))
}
func resellerCardPlain(r model.Reseller, sub bool) string {
	title := "✅ Revenda criada"
	if sub {
		title = "✅ SubRevenda criada"
	}
	kind := "Revenda"
	if sub || r.Level == 1 || r.ParentTelegramID != 0 {
		kind = "SubRevenda"
	}
	return fmt.Sprintf("%s\n━━━━━━━━━━━━━━\n👑 Nome: %s\n🆔 ID Telegram: %d\n📱 WhatsApp: %s\n📳 Limite: %d\n━━━━━━━━━━━━━━\n💰 Valor: %s\n📆 Expira: %s\n━━━━━━━━━━━━━━\n👥 Tipo: %s", title, r.Name, r.TelegramID, nonEmptyText(r.WhatsAppPhone, "-"), r.Credits, moneyBR(r.MonthlyPrice), daysLeft(r.ExpiresAt), kind)
}
func resellerUsedCredits(accs []model.Account) map[int64]int {
	out := map[int64]int{}
	for _, a := range accs {
		if a.DeletedAt != nil || strings.EqualFold(a.Status, "deleted") || !a.CreditCounted || a.OwnerTelegramID == 0 {
			continue
		}
		out[a.OwnerTelegramID]++
	}
	return out
}

func resellerMiniLine(r model.Reseller) string {
	kind := "Revenda"
	if r.Level == 1 || r.ParentTelegramID != 0 {
		kind = "Sub"
	}
	return fmt.Sprintf("%s | %s | 📳: %d | 📆: %s", r.Name, kind, r.Credits, daysLeft(r.ExpiresAt))
}
func boolText(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
func yesNoText(v bool) string {
	if v {
		return "Sim"
	}
	return "Não"
}
func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

type serverStatusInfo struct{ Host, Uptime, CPU, RAM, Disk string }

func readServerStatus(preferredHost string) (serverStatusInfo, error) {
	var out serverStatusInfo
	out.Host = firstNonEmpty(detectPublicIP(), strings.TrimSpace(preferredHost), detectLocalIP(), "-")
	out.Uptime = readUptimeText()
	out.CPU = readCPUPercentText()
	out.RAM = readRAMPercentText()
	out.Disk = readDiskText()
	return out, nil
}

func readUptimeText() string {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "indisponível"
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "indisponível"
	}
	secsF, _ := strconv.ParseFloat(fields[0], 64)
	secs := int64(secsF)
	d := secs / 86400
	secs %= 86400
	h := secs / 3600
	secs %= 3600
	m := secs / 60
	if d > 0 {
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func readCPUPercentText() string {
	a, ok := readCPUStat()
	if !ok {
		return "indisponível"
	}
	time.Sleep(220 * time.Millisecond)
	b, ok := readCPUStat()
	if !ok {
		return "indisponível"
	}
	total := b.total - a.total
	idle := b.idle - a.idle
	if total <= 0 {
		return "0%"
	}
	used := float64(total-idle) * 100 / float64(total)
	return fmt.Sprintf("%.0f%%", used)
}

type cpuStat struct{ total, idle uint64 }

func readCPUStat() (cpuStat, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuStat{}, false
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return cpuStat{}, false
	}
	vals := make([]uint64, 0, len(f)-1)
	for _, x := range f[1:] {
		v, _ := strconv.ParseUint(x, 10, 64)
		vals = append(vals, v)
	}
	var total uint64
	for _, v := range vals {
		total += v
	}
	idle := vals[3]
	if len(vals) > 4 {
		idle += vals[4]
	}
	return cpuStat{total: total, idle: idle}, true
}

func readRAMPercentText() string {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "indisponível"
	}
	vals := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			key := strings.TrimSuffix(f[0], ":")
			v, _ := strconv.ParseUint(f[1], 10, 64)
			vals[key] = v
		}
	}
	total := vals["MemTotal"]
	avail := vals["MemAvailable"]
	if total == 0 {
		return "indisponível"
	}
	used := float64(total-avail) * 100 / float64(total)
	return fmt.Sprintf("%.0f%%", used)
}

func readDiskText() string {
	var fs syscall.Statfs_t
	if err := syscall.Statfs("/", &fs); err != nil {
		return "indisponível"
	}
	total := fs.Blocks * uint64(fs.Bsize)
	free := fs.Bavail * uint64(fs.Bsize)
	used := total - free
	pct := 0.0
	if total > 0 {
		pct = float64(used) * 100 / float64(total)
	}
	return fmt.Sprintf("%s/%s (%.0f%%)", humanGB(used), humanGB(total), pct)
}

func humanGB(n uint64) string {
	gb := float64(n) / (1024 * 1024 * 1024)
	if gb < 10 {
		return fmt.Sprintf("%.1fGb", gb)
	}
	return fmt.Sprintf("%.0fGb", gb)
}

func detectPublicIP() string {
	ctx, cancel := context.WithTimeout(context.Background(), 1600*time.Millisecond)
	defer cancel()
	endpoints := []string{"https://api.ipify.org", "https://ifconfig.me/ip", "https://icanhazip.com"}
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	for _, endpoint := range endpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 96))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if isPublicIPv4(ip) {
			return ip
		}
	}
	return ""
}

func isPublicIPv4(ip string) bool {
	parts := strings.Split(strings.TrimSpace(ip), ".")
	if len(parts) != 4 {
		return false
	}
	nums := make([]int, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
		nums[i] = n
	}
	if nums[0] == 10 || nums[0] == 127 || nums[0] == 0 {
		return false
	}
	if nums[0] == 172 && nums[1] >= 16 && nums[1] <= 31 {
		return false
	}
	if nums[0] == 192 && nums[1] == 168 {
		return false
	}
	if nums[0] == 169 && nums[1] == 254 {
		return false
	}
	return true
}

func detectLocalIP() string {
	cctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	out, err := execCommandOutput(cctx, "hostname", "-I")
	if err == nil {
		for _, f := range strings.Fields(out) {
			if strings.Contains(f, ".") && !strings.HasPrefix(f, "127.") {
				return f
			}
		}
	}
	return ""
}

func execCommandOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

func paymentActorIsAdmin(actor model.Actor) bool {
	return actor.IsAdmin || actor.Role == model.RoleAdmin
}

func paymentLimitConfigAllowed(actor model.Actor) bool {
	return actor.IsAdmin || actor.Role == model.RoleAdmin
}

func paymentsManageAllowed(ctx context.Context, st *store.DB, actor model.Actor) bool {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return true
	}
	if actor.Role == model.RoleReseller || actor.Role == model.RoleSubReseller {
		r, _ := st.FindReseller(ctx, actor.TelegramID)
		return r != nil && r.Active && r.DeletedAt == nil
	}
	return false
}
func paymentOwnerID(actor model.Actor) int64 {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return 0
	}
	return actor.TelegramID
}
func paymentOwnerLabel(ctx context.Context, st *store.DB, ownerID int64) string {
	if ownerID == 0 {
		return "Admin"
	}
	if r, _ := st.FindReseller(ctx, ownerID); r != nil && r.Name != "" {
		return r.Name
	}
	return fmt.Sprintf("%d", ownerID)
}
func paymentBankName(bank string) string {
	switch normalizePaymentBank(bank) {
	case "asaas":
		return "Asaas"
	case "infinitepay":
		return "InfinitePay"
	default:
		return "Mercado Pago"
	}
}
func normalizePaymentBank(bank string) string {
	s := strings.ToLower(strings.TrimSpace(bank))
	s = strings.ReplaceAll(s, "-", "_")
	switch s {
	case "mp", "mercadopago", "mercado_pago", "mercado pago":
		return "mercado_pago"
	case "asaas":
		return "asaas"
	case "infinite", "infinitepay", "infinite_pay":
		return "infinitepay"
	}
	return ""
}
func enabledText(v bool) string {
	if v {
		return "ativado"
	}
	return "desativado"
}
func paymentStatusBadge(v bool) string {
	if v {
		return "✅ ativado"
	}
	return "❌ desativado"
}
func paymentWebhookURL(ctx context.Context, st *store.DB, fallback string, ownerID int64) string {
	if v, _ := st.GetSetting(ctx, "payments_webhook_url"); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return "https://api.primecel.shop/pix"
}

func normalizePaymentWebhookURL(raw string) (string, string) {
	v := strings.TrimSpace(strings.ToLower(raw))
	v = strings.Trim(v, " .")
	if v == "" {
		v = "api.primecel.shop/pix"
	}
	v = strings.TrimPrefix(strings.TrimPrefix(v, "https://"), "http://")
	v = strings.Trim(v, "/ ")
	if !strings.Contains(v, "/") {
		v = strings.TrimRight(v, "/") + "/pix"
	}
	domain := v
	if i := strings.Index(domain, "/"); i >= 0 {
		domain = domain[:i]
	}
	return "https://" + v, domain
}

func paymentWebhookStatusURL(url string) string {
	u := strings.TrimRight(strings.TrimSpace(url), "/")
	if u == "" {
		return ""
	}
	if strings.HasSuffix(u, "/pix") {
		return strings.TrimSuffix(u, "/pix") + "/pix/status"
	}
	if strings.HasSuffix(u, "/pix/webhook") {
		return strings.TrimSuffix(u, "/pix/webhook") + "/pix/status"
	}
	if strings.HasSuffix(u, "/webhook") {
		return strings.TrimSuffix(u, "/webhook") + "/webhook/status"
	}
	return u + "/status"
}
func paymentAdminText(ctx context.Context, st *store.DB, ownerID int64) string {
	cfg, _ := st.FindPaymentOwnerConfig(ctx, ownerID)
	bankID := "mercado_pago"
	enabled := false
	if cfg != nil {
		bankID = firstNonEmpty(cfg.Bank, bankID)
		enabled = cfg.Enabled
	}
	renewalBadge := "🔴 OFF"
	if PaymentRenewalNoticeEnabled(ctx, st, ownerID) {
		renewalBadge = "🟢 ON"
	}
	lines := []string{
		"💳 <b>Pagamentos</b>",
		"━━━━━━━━STATUS━━━━━━━━",
		fmt.Sprintf("👤 Dono: <b>%s</b>", h(paymentOwnerLabel(ctx, st, ownerID))),
		fmt.Sprintf("📌 Status: <code>%s</code>", h(paymentStatusBadge(enabled))),
		fmt.Sprintf("🏦 Banco Pix: <b>%s</b>", h(paymentBankName(bankID))),
		fmt.Sprintf("🔔 Receber aviso: <b>%s</b>", h(renewalBadge)),
	}
	if ownerID == 0 {
		lines = append(lines, fmt.Sprintf("🌐 WebHook: <code>%s</code>", h(firstNonEmpty(paymentWebhookURL(ctx, st, "", ownerID), "não configurado"))))
	}
	return strings.Join(lines, "\n")
}
func parseMoney(s string) (float64, error) {
	return strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", "."), 64)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
