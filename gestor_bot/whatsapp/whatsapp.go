package whatsapp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/accounts"
	"primecel-gestor/gestor_bot/apps"
	"primecel-gestor/gestor_bot/checkuserdb"
	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/online"
	"primecel-gestor/gestor_bot/payments"
	"primecel-gestor/gestor_bot/resellers"
	"primecel-gestor/gestor_bot/store"
	remotesync "primecel-gestor/gestor_bot/sync"
)

type Services struct {
	Config    config.Config
	Store     *store.DB
	Accounts  *accounts.Service
	Resellers *resellers.Service
	Apps      *apps.Manager
	Online    *online.Manager
}

type Handler struct{ Services Services }

func NewHandler(s Services) *Handler { return &Handler{Services: s} }

func (h *Handler) autoSyncStateSnapshot() {
	if h == nil || h.Services.Store == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = remotesync.NewManager(h.Services.Config, h.Services.Store).SyncStateSnapshot(ctx)
	}()
}

func (h *Handler) autoSyncRemovedAccount(username string) {
	username = strings.TrimSpace(username)
	if h == nil || h.Services.Store == nil || username == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		mgr := remotesync.NewManager(h.Services.Config, h.Services.Store)
		_, _ = mgr.SyncRemove(ctx, username)
		_, _ = mgr.SyncStateSnapshot(ctx)
	}()
}

type Request struct {
	From      string `json:"from"`
	Text      string `json:"text"`
	MediaPath string `json:"media_path,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}
type Response struct {
	Messages     []any `json:"messages"`
	ResetSession bool  `json:"reset_session,omitempty"`
}
type TextMessage struct {
	To   string `json:"to,omitempty"`
	Text string `json:"text"`
}
type ImageMessage struct {
	To      string `json:"to,omitempty"`
	Image   string `json:"image"`
	Caption string `json:"caption,omitempty"`
}
type DocumentMessage struct {
	To       string `json:"to,omitempty"`
	Document string `json:"document"`
	FileName string `json:"fileName"`
	Mimetype string `json:"mimetype"`
	Caption  string `json:"caption"`
}

type session struct {
	State     string            `json:"state"`
	Data      map[string]string `json:"data"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type actorResolution struct {
	Actor    model.Actor
	Kind     string
	Reseller *model.Reseller
	Accounts []model.Account
}

func (h *Handler) Handle(ctx context.Context, from, text string) (Response, error) {
	return h.HandleRequest(ctx, Request{From: from, Text: text})
}

func (h *Handler) HandleRequest(ctx context.Context, req Request) (Response, error) {
	from, text := req.From, req.Text
	phone := digits(from)
	if phone == "" {
		return Response{}, errors.New("número de origem vazio")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "menu"
	}
	if isCancel(text) {
		_ = h.clearSession(ctx, phone)
		return formatWAResponse(Response{Messages: []any{h.mainMenuText(ctx, phone)}}), nil
	}
	sess, _ := h.getSession(ctx, phone)
	res, err := h.resolveActor(ctx, phone)
	if err != nil {
		return Response{}, err
	}
	if sess.State != "" {
		resp, handled, err := h.handleStateReq(ctx, phone, text, req, sess, res)
		if err != nil {
			return formatWAResponse(Response{Messages: []any{"❌ " + err.Error(), "Digite 0 para voltar ao menu."}}), nil
		}
		if handled {
			return formatWAResponse(resp), nil
		}
	}
	resp, err := h.handleMenu(ctx, phone, text, res)
	return formatWAResponse(resp), err
}

func (h *Handler) handleMenu(ctx context.Context, phone, text string, r actorResolution) (Response, error) {
	raw := strings.TrimSpace(text)
	t := norm(text)
	if ck := choiceKey(text); len(ck) == 1 && ck >= "0" && ck <= "9" {
		t = ck
	}
	if resp, ok, err := h.handleDirectShortcut(ctx, phone, raw, r); ok || err != nil {
		return resp, err
	}
	switch t {
	case "menu", "inicio", "início", "0", "0️⃣":
		_ = h.clearSession(ctx, phone)
		return Response{Messages: []any{h.menuFor(ctx, r)}}, nil
	case "1", "1️⃣", "criar", "criar conta", "➕ criar", "➕ criar conta":
		if r.Kind == "client" {
			return h.clientData(ctx, r)
		}
		if r.Kind == "unknown" {
			return Response{Messages: []any{h.menuFor(ctx, r)}}, nil
		}
		h.setSession(ctx, phone, session{State: "create_username", Data: map[string]string{"trial": "0"}})
		return Response{Messages: []any{"➕ Criar conta\n━━━━━━━━━━━━━━\nDigite o nome do usuário."}}, nil
	case "2", "2️⃣", "teste", "criar teste", "🧪 criar teste":
		if r.Kind == "client" {
			return h.clientMenuAction(ctx, phone, "password", r)
		}
		if r.Kind == "unknown" {
			return Response{Messages: []any{h.menuFor(ctx, r)}}, nil
		}
		h.setSession(ctx, phone, session{State: "create_username", Data: map[string]string{"trial": "1"}})
		return Response{Messages: []any{"🧪 Criar teste\n━━━━━━━━━━━━━━\nDigite o nome do usuário."}}, nil
	case "3", "3️⃣", "editar", "editar conta", "✏️ editar conta", "/editar":
		if r.Kind == "client" {
			return h.clientClearDevices(ctx, r)
		}
		if r.Kind == "unknown" {
			return Response{Messages: []any{h.menuFor(ctx, r)}}, nil
		}
		h.setSession(ctx, phone, session{State: "edit_username", Data: map[string]string{}})
		return Response{Messages: []any{"✏️ Editar conta\n━━━━━━━━━━━━━━\nDigite o usuário da conta."}}, nil
	case "4", "4️⃣", "listar", "listar contas", "📋 listar contas", "/listar":
		if r.Kind == "client" {
			return h.clientRenewPrompt(ctx, phone, r)
		}
		if r.Kind == "unknown" {
			return Response{Messages: []any{h.menuFor(ctx, r)}}, nil
		}
		return h.listAccounts(ctx, r)
	case "5", "5️⃣", "online", "onlines", "listar onlines", "listar online", "🟢 listar onlines", "/online", "/onlines":
		if r.Kind == "client" {
			return h.clientSupport(ctx, r)
		}
		if r.Kind == "unknown" {
			return Response{Messages: []any{h.menuFor(ctx, r)}}, nil
		}
		return h.listOnlines(ctx, r)
	case "6", "6️⃣", "expirados", "🚫 expirados", "/expirados":
		if r.Kind == "unknown" || r.Kind == "client" {
			return Response{Messages: []any{h.menuFor(ctx, r)}}, nil
		}
		return h.listExpired(ctx, r)
	case "7", "7️⃣", "limpar expirados", "🗑️ limpar expirados":
		if r.Kind == "unknown" || r.Kind == "client" {
			return Response{Messages: []any{h.menuFor(ctx, r)}}, nil
		}
		return h.clearExpiredAccounts(ctx, r)
	case "8", "8️⃣", "limpar", "limpar aparelhos", "🧹 limpar aparelhos", "📱 limpar aparelhos", "/limpar":
		if r.Kind == "client" {
			return h.clientClearDevices(ctx, r)
		}
		if r.Kind == "unknown" {
			return Response{Messages: []any{h.menuFor(ctx, r)}}, nil
		}
		h.setSession(ctx, phone, session{State: "clear_username", Data: map[string]string{}})
		return Response{Messages: []any{"📱 Limpar Aparelhos\n━━━━━━━━━━━━━━\nDigite o usuário da conta ou TODOS."}}, nil
	default:
		return Response{Messages: []any{h.menuFor(ctx, r)}}, nil
	}
}

func (h *Handler) handleDirectShortcut(ctx context.Context, phone, raw string, r actorResolution) (Response, bool, error) {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if r.Kind == "client" || r.Kind == "unknown" {
		return Response{}, false, nil
	}
	cmds := []string{"/criar", "criar", "/teste", "teste", "/editar", "/limpar"}
	for _, cmd := range cmds {
		if lower == cmd || strings.HasPrefix(lower, cmd+" ") {
			arg := strings.TrimSpace(raw[len(cmd):])
			switch cmd {
			case "/criar", "criar":
				if arg == "" {
					return Response{}, false, nil
				}
				username := accounts.NormalizeUsername(arg)
				if err := accounts.ValidateUsername(username); err != nil {
					return Response{}, true, err
				}
				pass := randomDigits(5)
				h.setSession(ctx, phone, session{State: "create_ask_whatsapp", Data: map[string]string{"trial": "0", "username": username, "password": pass}})
				return Response{Messages: []any{waAskWhatsAppText("➕ Criar conta", username, pass)}}, true, nil
			case "/teste", "teste":
				if arg == "" {
					return Response{}, false, nil
				}
				username := accounts.NormalizeUsername(arg)
				if err := accounts.ValidateUsername(username); err != nil {
					return Response{}, true, err
				}
				pass := randomDigits(5)
				h.setSession(ctx, phone, session{State: "create_ask_whatsapp", Data: map[string]string{"trial": "1", "username": username, "password": pass}})
				return Response{Messages: []any{waAskWhatsAppText("🧪 Criar teste", username, pass)}}, true, nil
			case "/editar":
				if arg == "" {
					h.setSession(ctx, phone, session{State: "edit_username", Data: map[string]string{}})
					return Response{Messages: []any{"✏️ Editar conta\n━━━━━━━━━━━━━━\nDigite o usuário da conta."}}, true, nil
				}
				acc, err := h.Services.Store.FindAccount(ctx, arg)
				if err != nil || acc == nil {
					return Response{}, true, errors.New("conta não encontrada")
				}
				if !canSeeAccount(r, *acc) {
					return Response{}, true, errors.New("sem permissão para essa conta")
				}
				h.setSession(ctx, phone, session{State: "edit_action", Data: map[string]string{"username": acc.Username}})
				return Response{Messages: []any{accountPanelWithActions(*acc)}}, true, nil
			case "/limpar":
				if arg == "" {
					h.setSession(ctx, phone, session{State: "clear_username", Data: map[string]string{}})
					return Response{Messages: []any{"📱 Limpar Aparelhos\n━━━━━━━━━━━━━━\nDigite o usuário da conta ou TODOS."}}, true, nil
				}
				if strings.EqualFold(arg, "todos") {
					if err := h.clearDevicesScope(ctx, r); err != nil {
						return Response{}, true, err
					}
					return Response{Messages: []any{"✅ Aparelhos limpos."}}, true, nil
				}
				acc, err := h.Services.Store.FindAccount(ctx, arg)
				if err != nil || acc == nil {
					return Response{}, true, errors.New("conta não encontrada")
				}
				if !canSeeAccount(r, *acc) {
					return Response{}, true, errors.New("sem permissão para essa conta")
				}
				if err := h.clearDeviceUsers(ctx, []string{acc.Username}, false); err != nil {
					return Response{}, true, err
				}
				return Response{Messages: []any{"✅ Aparelhos limpos: " + acc.Username}}, true, nil
			}
		}
	}
	return Response{}, false, nil
}

func (h *Handler) handleStateReq(ctx context.Context, phone, text string, req Request, s session, r actorResolution) (Response, bool, error) {
	data := s.Data
	if data == nil {
		data = map[string]string{}
	}
	switch s.State {
	case "create_username":
		username := accounts.NormalizeUsername(text)
		if err := accounts.ValidateUsername(username); err != nil {
			return Response{}, true, err
		}
		data["username"] = username
		if strings.TrimSpace(data["password"]) == "" {
			data["password"] = randomDigits(5)
		}
		title := "➕ Criar conta"
		if data["trial"] == "1" {
			title = "🧪 Criar teste"
		}
		h.setSession(ctx, phone, session{State: "create_ask_whatsapp", Data: data})
		return Response{Messages: []any{waAskWhatsAppText(title, data["username"], data["password"])}}, true, nil
	case "create_ask_whatsapp":
		choice := waYesNoInput(text)
		if choice == "yes" {
			h.setSession(ctx, phone, session{State: "create_client_whatsapp", Data: data})
			return Response{Messages: []any{waClientWhatsAppPrompt()}}, true, nil
		}
		if choice == "no" {
			data["client_whatsapp"] = ""
			h.setSession(ctx, phone, session{State: "create_monthly", Data: data})
			return Response{Messages: []any{waMonthlyValuePrompt()}}, true, nil
		}
		return Response{Messages: []any{"⚠️ Responda Sim ou Não.\n\n1️⃣ Sim\n2️⃣ Não"}}, true, nil
	case "create_client_whatsapp":
		if !strings.EqualFold(strings.TrimSpace(text), "pular") && strings.TrimSpace(text) != "0" {
			phoneDigits := digits(text)
			if len(phoneDigits) < 10 {
				return Response{}, true, errors.New("WhatsApp inválido")
			}
			data["client_whatsapp"] = phoneDigits
		}
		h.setSession(ctx, phone, session{State: "create_monthly", Data: data})
		return Response{Messages: []any{waMonthlyValuePrompt()}}, true, nil
	case "create_monthly":
		monthly, err := parseMoneyWA(text)
		if err != nil || monthly < 0 {
			return Response{}, true, errors.New("valor inválido")
		}
		data["monthly_value"] = fmt.Sprintf("%.2f", monthly)
		if data["trial"] == "1" {
			h.setSession(ctx, phone, session{State: "create_trial_hours", Data: data})
			return Response{Messages: []any{waAccountPreChoiceText("🧪 Criar teste", data, monthly, "Escolha a duração:\n\n1️⃣ 2h\n2️⃣ 4h\n3️⃣ 8h")}}, true, nil
		}
		h.setSession(ctx, phone, session{State: "create_days", Data: data})
		return Response{Messages: []any{waAccountPreChoiceText("➕ Criar conta", data, monthly, "Escolha a validade:\n\n1️⃣ 1 mês\n2️⃣ 2 meses\n3️⃣ 3 meses")}}, true, nil
	case "create_days":
		days := mapChoice(text, map[string]string{"1": "30", "2": "60", "3": "90", "30": "30", "60": "60", "90": "90"})
		if days == "" {
			return Response{}, true, errors.New("opção inválida")
		}
		data["days"] = days
		// WhatsApp segue regra direta: se Xray Geral estiver ON e a Revenda/SubRevenda tiver permissão, cria com Xray sem perguntar.
		return h.finishCreate(ctx, phone, data, r, h.actorAllowsXray(r))
	case "create_trial_hours":
		hrs := mapChoice(text, map[string]string{"1": "2", "2": "4", "3": "8", "2h": "2", "4h": "4", "8h": "8"})
		if hrs == "" {
			return Response{}, true, errors.New("opção inválida")
		}
		data["hours"] = hrs
		// WhatsApp segue regra direta: se Xray Geral estiver ON e a Revenda/SubRevenda tiver permissão, cria com Xray sem perguntar.
		return h.finishCreate(ctx, phone, data, r, h.actorAllowsXray(r))
	case "create_xray":
		// Compatibilidade para sessões antigas abertas antes da atualização. Novas criações não passam por esta pergunta.
		yes := isYes(text)
		return h.finishCreate(ctx, phone, data, r, yes)
	case "edit_username":
		acc, err := h.Services.Store.FindAccount(ctx, text)
		if err != nil || acc == nil {
			return Response{}, true, errors.New("conta não encontrada")
		}
		if !canSeeAccount(r, *acc) {
			return Response{}, true, errors.New("sem permissão para essa conta")
		}
		data["username"] = acc.Username
		h.setSession(ctx, phone, session{State: "edit_action", Data: data})
		return Response{Messages: []any{accountPanelWithActions(*acc)}}, true, nil
	case "edit_action":
		switch choiceKey(text) {
		case "1":
			acc, err := h.Services.Store.FindAccount(ctx, data["username"])
			if err != nil || acc == nil {
				return Response{}, true, errors.New("conta não encontrada")
			}
			return Response{Messages: []any{accountCopyText(*acc)}}, true, nil
		case "2":
			h.setSession(ctx, phone, session{State: "edit_password", Data: data})
			return Response{Messages: []any{"✏️ " + data["username"] + "\n━━━━━━━━━━━━━━\nDigite a nova senha."}}, true, nil
		case "3":
			h.setSession(ctx, phone, session{State: "edit_renew_days", Data: data})
			return Response{Messages: []any{"♻️ Renovar conta\n━━━━━━━━━━━━━━\n👤 Usuário: " + data["username"] + "\n\nEscolha a renovação:\n\n1️⃣ 1 mês\n2️⃣ 2 meses\n3️⃣ 3 meses"}}, true, nil
		case "4":
			h.setSession(ctx, phone, session{State: "edit_limit", Data: data})
			return Response{Messages: []any{"✏️ " + data["username"] + "\n━━━━━━━━━━━━━━\nDigite o novo limite de aparelhos/conexões."}}, true, nil
		case "5":
			h.setSession(ctx, phone, session{State: "edit_remove_confirm", Data: data})
			return Response{Messages: []any{"🗑️ Remover conta\n━━━━━━━━━━━━━━\n👤 Usuário: " + data["username"] + "\n\nConfirma remover esta conta?\n\n1️⃣ Confirmar\n2️⃣ Cancelar"}}, true, nil
		default:
			return Response{}, true, errors.New("opção inválida")
		}
	case "edit_password":
		acc, err := h.Services.Accounts.ChangePassword(ctx, r.Actor, data["username"], text)
		if err != nil {
			return Response{}, true, err
		}
		h.autoSyncStateSnapshot()
		_ = h.clearSession(ctx, phone)
		return Response{Messages: []any{accountSuccessTextWA("🔐 Senha alterada", *acc)}}, true, nil
	case "edit_renew_days", "renew_days":
		days := mapChoice(text, map[string]string{"1": "30", "2": "60", "3": "90", "30": "30", "60": "60", "90": "90"})
		if days == "" {
			return Response{}, true, errors.New("opção inválida")
		}
		n, _ := strconv.Atoi(days)
		acc, err := h.Services.Accounts.Renew(ctx, r.Actor, data["username"], n)
		if err != nil {
			return Response{}, true, err
		}
		h.autoSyncStateSnapshot()
		_ = h.clearSession(ctx, phone)
		return Response{Messages: []any{accountSuccessTextWA("✅ Conta renovada", *acc)}}, true, nil
	case "edit_limit":
		n, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || n <= 0 {
			return Response{}, true, errors.New("limite inválido")
		}
		acc, err := h.Services.Accounts.ChangeLimit(ctx, r.Actor, data["username"], n)
		if err != nil {
			return Response{}, true, err
		}
		h.autoSyncStateSnapshot()
		_ = h.clearSession(ctx, phone)
		return Response{Messages: []any{accountSuccessTextWA("📳 Limite alterado", *acc)}}, true, nil
	case "edit_remove_confirm":
		if !isYes(text) {
			_ = h.clearSession(ctx, phone)
			return Response{Messages: []any{"Operação cancelada."}}, true, nil
		}
		if err := h.Services.Accounts.Remove(ctx, r.Actor, data["username"]); err != nil {
			return Response{}, true, err
		}
		h.autoSyncRemovedAccount(data["username"])
		_ = h.clearSession(ctx, phone)
		return Response{Messages: []any{"✅ Conta removida\n━━━━━━━━━━━━━━\n👤 Usuário: " + data["username"]}}, true, nil
	case "renew_username":
		acc, err := h.Services.Store.FindAccount(ctx, text)
		if err != nil || acc == nil {
			return Response{}, true, errors.New("conta não encontrada")
		}
		if !canSeeAccount(r, *acc) {
			return Response{}, true, errors.New("sem permissão para essa conta")
		}
		data["username"] = acc.Username
		h.setSession(ctx, phone, session{State: "renew_days", Data: data})
		return Response{Messages: []any{"♻️ Renovar conta\n━━━━━━━━━━━━━━\n👤 Usuário: " + data["username"] + "\n\nEscolha a renovação:\n\n1️⃣ 1 mês\n2️⃣ 2 meses\n3️⃣ 3 meses"}}, true, nil
	case "clear_username":
		target := strings.TrimSpace(text)
		if strings.EqualFold(target, "todos") {
			if err := h.clearDevicesScope(ctx, r); err != nil {
				return Response{}, true, err
			}
			_ = h.clearSession(ctx, phone)
			return Response{Messages: []any{"✅ Aparelhos limpos."}}, true, nil
		}
		acc, err := h.Services.Store.FindAccount(ctx, target)
		if err != nil || acc == nil {
			return Response{}, true, errors.New("conta não encontrada")
		}
		if !canSeeAccount(r, *acc) {
			return Response{}, true, errors.New("sem permissão para essa conta")
		}
		if err := h.clearDeviceUsers(ctx, []string{acc.Username}, false); err != nil {
			return Response{}, true, err
		}
		_ = h.clearSession(ctx, phone)
		return Response{Messages: []any{"✅ Aparelhos limpos: " + acc.Username}}, true, nil
	case "client_renew_months":
		return h.handleClientRenewMonths(ctx, phone, text, data, r)
	case "client_renew_manual_proof":
		return h.handleClientRenewManualProof(ctx, phone, req, data)
	case "renew_manual_confirm":
		return h.handleRenewManualConfirm(ctx, phone, text, data, r)
	case "client_temp_release":
		if isYes(text) {
			acc, err := h.Services.Accounts.TemporaryRelease(ctx, r.Actor, data["username"], 15*time.Minute)
			if err != nil {
				return Response{}, true, err
			}
			_ = h.clearSession(ctx, phone)
			return Response{Messages: []any{"✅ Liberação temporária aplicada.\n\n" + accountPanel(*acc)}}, true, nil
		}
		_ = h.clearSession(ctx, phone)
		return Response{Messages: []any{"Certo. Fale com o suporte para renovar."}}, true, nil
	}
	return Response{}, false, nil
}

func (h *Handler) finishCreate(ctx context.Context, phone string, data map[string]string, r actorResolution, xray bool) (Response, bool, error) {
	pass := strings.TrimSpace(data["password"])
	if pass == "" {
		pass = randomDigits(5)
	}
	uuid := newUUIDLike()
	days, _ := strconv.Atoi(data["days"])
	hours, _ := strconv.Atoi(data["hours"])
	monthlyValue, _ := strconv.ParseFloat(data["monthly_value"], 64)
	acc, err := h.Services.Accounts.Create(ctx, r.Actor, accounts.CreateDraft{Username: data["username"], Password: pass, Days: days, TrialHours: hours, Limit: 1, UUID: uuid, XrayEnabled: xray, IsTrial: data["trial"] == "1", ClientWhatsApp: data["client_whatsapp"], MonthlyValue: monthlyValue})
	if err != nil {
		return Response{}, true, err
	}
	h.autoSyncStateSnapshot()
	_ = h.clearSession(ctx, phone)
	title := "✅ Conta criada"
	if acc.IsTrial {
		title = "✅ Teste criado"
	}
	return Response{Messages: []any{createdAccountPanel(title, *acc)}}, true, nil
}

func (h *Handler) resolveActor(ctx context.Context, phone string) (actorResolution, error) {
	for _, a := range h.Services.Config.WhatsAppAdminNumbers {
		if digits(a) == phone {
			return actorResolution{Kind: "admin", Actor: model.Actor{Role: model.RoleAdmin, IsAdmin: true, Name: h.Services.Config.AdminDisplayName}}, nil
		}
	}
	resellersList, _ := h.Services.Store.ListResellers(ctx)
	for _, r := range resellersList {
		if digits(r.WhatsAppPhone) == phone {
			role := model.RoleReseller
			if r.Level == 1 || r.ParentTelegramID != 0 {
				role = model.RoleSubReseller
			}
			return actorResolution{Kind: "reseller", Reseller: &r, Actor: model.Actor{TelegramID: r.TelegramID, Name: r.Name, Role: role, ParentID: r.ParentTelegramID, IsAdmin: false}}, nil
		}
	}
	accountsList, _ := h.Services.Store.ListAccounts(ctx, false)
	var owned []model.Account
	for _, a := range accountsList {
		if digits(a.ClientWhatsApp) == phone {
			owned = append(owned, a)
		}
	}
	if len(owned) > 0 {
		return actorResolution{Kind: "client", Accounts: owned, Actor: model.Actor{Role: model.RoleAdmin, Name: "Cliente"}}, nil
	}
	return actorResolution{Kind: "unknown", Actor: model.Actor{Role: model.RoleAdmin, IsAdmin: false, Name: "Visitante"}}, nil
}

func (h *Handler) menuFor(ctx context.Context, r actorResolution) string {
	switch r.Kind {
	case "admin", "reseller":
		return h.accountsMenuText(ctx, r)
	case "client":
		return h.clientMenuText(ctx, r)
	default:
		return "Olá. Seu número ainda não está vinculado a uma revenda ou conta. Entre em contato com o suporte."
	}
}

func (h *Handler) mainMenuText(ctx context.Context, phone string) string {
	r, _ := h.resolveActor(ctx, phone)
	return h.menuFor(ctx, r)
}

func (h *Handler) actorAllowsXray(r actorResolution) bool {
	if !h.xrayCreateEnabled(context.Background()) {
		return false
	}
	if r.Kind == "admin" {
		return true
	}
	if r.Reseller != nil {
		return r.Reseller.AllowXray
	}
	return false
}

func (h *Handler) xrayCreateEnabled(ctx context.Context) bool {
	if h.Services.Store != nil {
		if v, _ := h.Services.Store.GetSetting(ctx, "xray_create_enabled"); strings.TrimSpace(v) != "" {
			return settingBool(v)
		}
	}
	return h.Services.Config.XrayCreateEnabled
}

func settingBool(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true" || v == "yes" || v == "sim" || v == "on"
}
func canSeeAccount(r actorResolution, a model.Account) bool {
	if r.Kind == "admin" {
		return true
	}
	if r.Kind == "reseller" {
		return a.OwnerTelegramID == r.Actor.TelegramID
	}
	if r.Kind == "client" {
		for _, ca := range r.Accounts {
			if strings.EqualFold(ca.Username, a.Username) {
				return true
			}
		}
	}
	return false
}

func (h *Handler) accountsMenuText(ctx context.Context, r actorResolution) string {
	if r.Kind == "reseller" {
		return h.resellerAccountsMenuText(ctx, r)
	}
	active, expired, onlineCount, _ := h.menuAccountStats(ctx, r)
	name := firstNonEmpty(r.Actor.Name, h.Services.Config.AdminDisplayName, "Admin")
	return fmt.Sprintf("⚡ PRIMECEL - %s\n━━━━━━━━━━━━━━━━━━\n👤 Contas: %d | Expirados: %d\n🟢 Online: %d\n━━━━━━━━━━━━━━━━━━\n1️⃣ Criar Conta\n2️⃣ Criar Teste\n3️⃣ Editar Conta\n4️⃣ Listar Contas\n5️⃣ Listar Onlines\n6️⃣ Listar Expirados\n7️⃣ Limpar Expirados\n8️⃣ Limpar Aparelhos\n9️⃣ Remover Todos", name, active, expired, onlineCount)
}

func (h *Handler) resellerAccountsMenuText(ctx context.Context, r actorResolution) string {
	name := firstNonEmpty(r.Actor.Name, "Revenda")
	active, expired, onlineCount, subCount := h.menuAccountStats(ctx, r)
	limit := 0
	if r.Reseller != nil {
		limit = r.Reseller.Credits
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "⚡ PRIMECEL - %s\n", name)
	sb.WriteString("━━━━━━━━━━━━━━━━━━\n")
	if r.Actor.Role == model.RoleReseller {
		fmt.Fprintf(&sb, "👥 Revendas: %d\n", subCount)
	}
	fmt.Fprintf(&sb, "📳 Limite: %d\n", limit)
	fmt.Fprintf(&sb, "👤 Contas: %d | Expirados: %d\n", active, expired)
	fmt.Fprintf(&sb, "🟢 Online: %d\n", onlineCount)
	sb.WriteString("━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString("1️⃣ Criar Conta\n")
	sb.WriteString("2️⃣ Criar Teste\n")
	sb.WriteString("3️⃣ Editar Conta\n")
	sb.WriteString("4️⃣ Listar Contas\n")
	sb.WriteString("5️⃣ Listar Onlines\n")
	sb.WriteString("6️⃣ Listar Expirados\n")
	sb.WriteString("7️⃣ Limpar Expirados\n")
	sb.WriteString("8️⃣ Limpar Aparelhos")
	return sb.String()
}

func (h *Handler) menuAccountStats(ctx context.Context, r actorResolution) (active int, expired int, onlineCount int, subCount int) {
	owners := h.menuOwnerIDs(ctx, r)
	now := time.Now().UTC()
	list, _ := h.Services.Store.ListAccounts(ctx, false)
	for _, a := range list {
		if a.DeletedAt != nil || a.Status == "deleted" || !ownerVisibleForMenu(r, owners, a.OwnerTelegramID) {
			continue
		}
		if a.ExpiresAt.After(now) {
			active++
		} else {
			expired++
		}
	}
	if r.Kind == "reseller" && r.Actor.Role == model.RoleReseller {
		rs, _ := h.Services.Store.ListResellers(ctx)
		for _, rr := range rs {
			if rr.DeletedAt == nil && rr.ParentTelegramID == r.Actor.TelegramID {
				subCount++
			}
		}
	}
	if h.Services.Online != nil {
		if sum, err := h.Services.Online.Summary(ctx); err == nil {
			for _, it := range sum.Users {
				if ownerVisibleForMenu(r, owners, it.OwnerID) {
					onlineCount++
				}
			}
		}
	}
	return active, expired, onlineCount, subCount
}

func (h *Handler) menuOwnerIDs(ctx context.Context, r actorResolution) map[int64]bool {
	if r.Kind == "admin" || r.Actor.IsAdmin || r.Actor.Role == model.RoleAdmin {
		return nil
	}
	owners := map[int64]bool{r.Actor.TelegramID: true}
	if r.Kind == "reseller" && r.Actor.Role == model.RoleReseller {
		rs, _ := h.Services.Store.ListResellers(ctx)
		for _, rr := range rs {
			if rr.DeletedAt == nil && rr.ParentTelegramID == r.Actor.TelegramID {
				owners[rr.TelegramID] = true
			}
		}
	}
	return owners
}

func ownerVisibleForMenu(r actorResolution, owners map[int64]bool, ownerID int64) bool {
	if r.Kind == "admin" || r.Actor.IsAdmin || r.Actor.Role == model.RoleAdmin {
		return true
	}
	return owners[ownerID]
}

func (h *Handler) listAccounts(ctx context.Context, r actorResolution) (Response, error) {
	list, _ := h.Services.Store.ListAccounts(ctx, false)
	now := time.Now().UTC()
	var rows []model.Account
	for _, a := range list {
		if canSeeAccount(r, a) && a.DeletedAt == nil && a.Status != "deleted" && a.ExpiresAt.After(now) {
			rows = append(rows, a)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return strings.ToLower(rows[i].Username) < strings.ToLower(rows[j].Username) })
	var lines []string
	for i, a := range rows {
		if i >= 10 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s | 📳: %d | 📆: %s", a.Username, nonZeroInt(a.LimitConnections, 1), left(a.ExpiresAt)))
	}
	if len(lines) == 0 {
		lines = []string{"Nenhuma conta ativa."}
	}
	return Response{Messages: []any{"📋 Contas [" + strconv.Itoa(len(rows)) + "]\n━━━━━━━━━━━━━━\n" + strings.Join(lines, "\n") + "\n━━━━━━━━━━━━━━"}}, nil
}

func (h *Handler) listOnlines(ctx context.Context, r actorResolution) (Response, error) {
	sum, err := h.Services.Online.Summary(ctx)
	if err != nil {
		return Response{}, err
	}
	var rows []online.Item
	for _, it := range sum.Users {
		if r.Kind == "admin" || (r.Kind == "reseller" && it.OwnerID == r.Actor.TelegramID) {
			rows = append(rows, it)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return strings.ToLower(rows[i].Username) < strings.ToLower(rows[j].Username) })
	var lines []string
	for i, it := range rows {
		if i >= 10 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s | 🟢: %d/%d", it.Username, it.Connections, nonZeroInt(it.Limit, 1)))
	}
	if len(lines) == 0 {
		lines = []string{"Nenhuma conta online."}
	}
	return Response{Messages: []any{"🟢 Contas Online [" + strconv.Itoa(len(rows)) + "]\n━━━━━━━━━━━━━━\n" + strings.Join(lines, "\n") + "\n━━━━━━━━━━━━━━"}}, nil
}

func (h *Handler) listExpired(ctx context.Context, r actorResolution) (Response, error) {
	list, _ := h.Services.Store.ListAccounts(ctx, false)
	now := time.Now().UTC()
	var rows []model.Account
	for _, a := range list {
		if canSeeAccount(r, a) && a.DeletedAt == nil && a.Status != "deleted" && !a.ExpiresAt.After(now) {
			rows = append(rows, a)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ExpiresAt.Before(rows[j].ExpiresAt) })
	var lines []string
	for i, a := range rows {
		if i >= 10 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s | 📆: %s", a.Username, expiredForWA(a.ExpiresAt)))
	}
	if len(lines) == 0 {
		lines = []string{"Nenhuma conta expirada."}
	}
	return Response{Messages: []any{"🚫 Contas Expiradas [" + strconv.Itoa(len(rows)) + "]\n━━━━━━━━━━━━━━\n" + strings.Join(lines, "\n") + "\n━━━━━━━━━━━━━━"}}, nil
}

func (h *Handler) clearExpiredAccounts(ctx context.Context, r actorResolution) (Response, error) {
	list, _ := h.Services.Store.ListAccounts(ctx, false)
	now := time.Now().UTC()
	removed := 0
	for _, a := range list {
		if !canSeeAccount(r, a) || a.DeletedAt != nil || a.Status == "deleted" || a.ExpiresAt.After(now) {
			continue
		}
		if err := h.Services.Accounts.Remove(ctx, r.Actor, a.Username); err == nil {
			removed++
		}
	}
	return Response{Messages: []any{fmt.Sprintf("🗑️ Limpar Expirados\n━━━━━━━━━━━━━━\nContas removidas: %d", removed)}}, nil
}

func (h *Handler) sendApps(ctx context.Context) (Response, error) {
	list, err := h.Services.Apps.List(ctx)
	if err != nil {
		return Response{}, err
	}
	if len(list) == 0 {
		return Response{Messages: []any{"📲 Nenhum aplicativo cadastrado."}}, nil
	}
	var msgs []any
	for _, app := range list {
		if app.Path != "" {
			msgs = append(msgs, DocumentMessage{Document: app.Path, FileName: nonEmpty(app.FileName, app.Name+".apk"), Mimetype: nonEmpty(app.MimeType, "application/vnd.android.package-archive"), Caption: "📲 " + app.Name + " | Versão " + app.Version})
		} else {
			msgs = append(msgs, "📲 "+app.Name+" | Versão "+app.Version+"\nArquivo local ausente.")
		}
	}
	return Response{Messages: msgs}, nil
}

func (h *Handler) clientMenuText(ctx context.Context, r actorResolution) string {
	if len(r.Accounts) == 0 {
		return "👤 Painel do Cliente\n━━━━━━━━━━━━━━\nNenhuma conta vinculada."
	}
	a := r.Accounts[0]
	devs, _ := h.Services.Store.CountDevices(ctx, a.Username)
	return fmt.Sprintf("👤 Minha Conta\n━━━━━━━━━━━━━━━━━━\n👤 Usuário: %s\n📆 Validade: %s\n⏳ Expira: %s\n📳 Limite: %d\n📱 Aparelhos: %d/%d\n━━━━━━━━━━━━━━━━━━\n\n1️⃣ Ver Dados\n2️⃣ Mudar Senha\n3️⃣ Limpar Aparelhos\n4️⃣ Renovar\n5️⃣ Suporte", a.Username, brDateWA(a.ExpiresAt), daysLeftWA(a.ExpiresAt), nonZeroInt(a.LimitConnections, 1), devs, nonZeroInt(a.LimitConnections, 1))
}

func (h *Handler) clientData(ctx context.Context, r actorResolution) (Response, error) {
	if len(r.Accounts) == 0 {
		return Response{Messages: []any{"Nenhuma conta vinculada."}}, nil
	}
	return Response{Messages: []any{accountCopyText(r.Accounts[0])}}, nil
}

func (h *Handler) clientMenuAction(ctx context.Context, phone, action string, r actorResolution) (Response, error) {
	if len(r.Accounts) == 0 {
		return Response{Messages: []any{"Nenhuma conta vinculada."}}, nil
	}
	data := map[string]string{"username": r.Accounts[0].Username}
	h.setSession(ctx, phone, session{State: "edit_password", Data: data})
	return Response{Messages: []any{"🔐 Mudar Senha\n━━━━━━━━━━━━━━\nDigite a nova senha."}}, nil
}

func (h *Handler) clientRenewPrompt(ctx context.Context, phone string, r actorResolution) (Response, error) {
	if len(r.Accounts) == 0 {
		return Response{Messages: []any{"Nenhuma conta vinculada."}}, nil
	}
	a := r.Accounts[0]
	monthly := h.accountMonthlyAmount(ctx, a)
	h.setSession(ctx, phone, session{State: "client_renew_months", Data: map[string]string{"username": a.Username}})
	return Response{Messages: []any{fmt.Sprintf("♻️ Renovar Conta\n━━━━━━━━━━━━━━\n👤 Usuário: %s\n💰 Valor mensal: %s\n\nEscolha o tempo:\n\n1️⃣ 1 mês — %s\n2️⃣ 2 meses — %s\n3️⃣ 3 meses — %s", a.Username, moneyWA(monthly), moneyWA(monthly), moneyWA(monthly*2), moneyWA(monthly*3))}}, nil
}

func (h *Handler) handleClientRenewMonths(ctx context.Context, phone, text string, data map[string]string, r actorResolution) (Response, bool, error) {
	monthsStr := mapChoice(text, map[string]string{"1": "1", "2": "2", "3": "3"})
	if monthsStr == "" {
		return Response{}, true, errors.New("opção inválida")
	}
	months, _ := strconv.Atoi(monthsStr)
	acc, err := h.Services.Store.FindAccount(ctx, data["username"])
	if err != nil || acc == nil {
		return Response{}, true, errors.New("conta não encontrada")
	}
	monthly := h.accountMonthlyAmount(ctx, *acc)
	amount := monthly * float64(months)
	sellerPhone := h.accountSellerPhone(ctx, *acc)
	if sellerPhone == "" {
		return Response{}, true, errors.New("vendedor sem WhatsApp configurado")
	}
	ownerID := acc.OwnerTelegramID
	mode := "manual"
	if h.hasAutoPayment(ctx, ownerID) {
		mode = "auto"
	}
	id := "WR" + time.Now().UTC().Format("20060102150405") + randomDigits(4)
	days := months * 30
	now := time.Now().UTC().Format(time.RFC3339)
	if err := h.Services.Store.Exec(ctx, `INSERT INTO whatsapp_renewal_requests(id,username,client_phone,seller_phone,owner_id,months,days,amount,mode,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, acc.Username, phone, sellerPhone, ownerID, months, days, amount, mode, "pending", now, now); err != nil {
		return Response{}, true, err
	}
	if mode == "auto" {
		return h.createClientAutoPix(ctx, phone, id, *acc, months, days, amount, ownerID)
	}
	_ = h.clearSession(ctx, phone)
	h.setSession(ctx, phone, session{State: "client_renew_manual_proof", Data: map[string]string{"request_id": id, "username": acc.Username, "months": monthsStr, "amount": fmt.Sprintf("%.2f", amount), "seller_phone": sellerPhone}})
	msg := fmt.Sprintf("♻️ Renovar Conta\n━━━━━━━━━━━━━━\n👤 Usuário: %s\n⏳ Tempo: %d mês(es)\n💰 Valor: %s\n━━━━━━━━━━━━━━\n✅ Após o pagamento, envie o comprovante em imagem aqui.", acc.Username, months, moneyWA(amount))
	return Response{Messages: []any{msg}}, true, nil
}

func (h *Handler) handleClientRenewManualProof(ctx context.Context, phone string, req Request, data map[string]string) (Response, bool, error) {
	if strings.TrimSpace(req.MediaPath) == "" || !strings.Contains(strings.ToLower(req.MediaType), "image") {
		return Response{}, true, errors.New("envie o comprovante em imagem")
	}
	reqID := data["request_id"]
	username := data["username"]
	months, _ := strconv.Atoi(data["months"])
	amount, _ := strconv.ParseFloat(data["amount"], 64)
	sellerPhone := digits(data["seller_phone"])
	if sellerPhone == "" {
		return Response{}, true, errors.New("vendedor sem WhatsApp configurado")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_ = h.Services.Store.Exec(ctx, `UPDATE whatsapp_renewal_requests SET proof_path=?, status='proof_sent', updated_at=? WHERE id=?`, req.MediaPath, now, reqID)
	_ = h.setSession(ctx, sellerPhone, session{State: "renew_manual_confirm", Data: map[string]string{"request_id": reqID, "username": username, "months": strconv.Itoa(months), "client_phone": phone}})
	caption := fmt.Sprintf("♻️ Solicitação de Renovação\n━━━━━━━━━━━━━━\n👤 Usuário: %s\n⏳ Tempo: %d mês(es)\n💰 Valor pago: %s\n━━━━━━━━━━━━━━\n1️⃣ Confirmar e liberar\n2️⃣ Recusar", username, months, moneyWA(amount))
	_ = h.clearSession(ctx, phone)
	return Response{Messages: []any{
		"✅ Comprovante enviado com sucesso.\n\nAguarde a confirmação do vendedor.",
		ImageMessage{To: sellerPhone, Image: req.MediaPath, Caption: caption},
	}}, true, nil
}

func (h *Handler) handleRenewManualConfirm(ctx context.Context, phone, text string, data map[string]string, r actorResolution) (Response, bool, error) {
	choice := choiceKey(text)
	if choice != "1" && choice != "2" {
		return Response{}, true, errors.New("opção inválida")
	}
	reqID := data["request_id"]
	username := data["username"]
	months, _ := strconv.Atoi(data["months"])
	clientPhone := digits(data["client_phone"])
	if choice == "2" {
		now := time.Now().UTC().Format(time.RFC3339)
		_ = h.Services.Store.Exec(ctx, `UPDATE whatsapp_renewal_requests SET status='rejected', updated_at=? WHERE id=?`, now, reqID)
		_ = h.clearSession(ctx, phone)
		return Response{Messages: []any{
			"❌ Solicitação recusada.",
			TextMessage{To: clientPhone, Text: "❌ Sua solicitação de renovação foi recusada.\n\nEntre em contato com o suporte."},
		}}, true, nil
	}
	days := months * 30
	acc, err := h.Services.Accounts.Renew(ctx, r.Actor, username, days)
	if err != nil {
		return Response{}, true, err
	}
	h.autoSyncStateSnapshot()
	now := time.Now().UTC().Format(time.RFC3339)
	_ = h.Services.Store.Exec(ctx, `UPDATE whatsapp_renewal_requests SET status='approved', applied_at=?, updated_at=? WHERE id=?`, now, now, reqID)
	_ = h.clearSession(ctx, phone)
	return Response{Messages: []any{
		accountSuccessTextWA("✅ Conta renovada", *acc),
		TextMessage{To: clientPhone, Text: fmt.Sprintf("✅ Conta renovada com sucesso.\n\n👤 Usuário: %s\n📆 Nova validade: %s", acc.Username, brDateWA(acc.ExpiresAt))},
	}}, true, nil
}

func (h *Handler) createClientAutoPix(ctx context.Context, phone, reqID string, acc model.Account, months, days int, amount float64, ownerID int64) (Response, bool, error) {
	cfg, _ := h.Services.Store.FindPaymentOwnerConfig(ctx, ownerID)
	if cfg == nil || !cfg.Enabled || cfg.Bank == "" {
		return Response{}, true, errors.New("pagamento automático não configurado")
	}
	mgr := payments.NewManager(h.Services.Store)
	order, err := mgr.CreateOrder(ctx, payments.OrderInput{OwnerID: ownerID, TargetResellerID: 0, Kind: payments.KindAccountRenew, Months: months, Days: days, Amount: amount, Bank: cfg.Bank, Description: fmt.Sprintf("Renovação %d mês(es) - %s", months, acc.Username)})
	if err != nil {
		return Response{}, true, err
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(order.PayloadJSON), &payload)
	if payload == nil {
		payload = map[string]any{}
	}
	payload["username"] = acc.Username
	payload["client_phone"] = phone
	payload["renewal_request_id"] = reqID
	b, _ := json.Marshal(payload)
	order.PayloadJSON = string(b)
	_ = h.Services.Store.UpdatePaymentOrder(ctx, *order)
	now := time.Now().UTC().Format(time.RFC3339)
	_ = h.Services.Store.Exec(ctx, `UPDATE whatsapp_renewal_requests SET status='pix_sent', order_id=?, updated_at=? WHERE id=?`, order.OrderID, now, reqID)
	_ = h.clearSession(ctx, phone)
	return Response{Messages: []any{clientPixText(*order, acc.Username, months, amount)}}, true, nil
}

func (h *Handler) accountMonthlyAmount(ctx context.Context, a model.Account) float64 {
	if a.MonthlyValue > 0 {
		return a.MonthlyValue
	}
	if a.OwnerTelegramID != 0 {
		if r, _ := h.Services.Store.FindReseller(ctx, a.OwnerTelegramID); r != nil && r.MonthlyPrice > 0 {
			return r.MonthlyPrice
		}
	}
	return 0
}

func (h *Handler) accountSellerPhone(ctx context.Context, a model.Account) string {
	if a.OwnerTelegramID != 0 {
		if r, _ := h.Services.Store.FindReseller(ctx, a.OwnerTelegramID); r != nil && digits(r.WhatsAppPhone) != "" {
			return digits(r.WhatsAppPhone)
		}
	}
	for _, n := range h.Services.Config.WhatsAppAdminNumbers {
		if d := digits(n); d != "" {
			return d
		}
	}
	return ""
}

func (h *Handler) hasAutoPayment(ctx context.Context, ownerID int64) bool {
	cfg, _ := h.Services.Store.FindPaymentOwnerConfig(ctx, ownerID)
	return cfg != nil && cfg.Enabled && strings.TrimSpace(cfg.Bank) != ""
}

func (h *Handler) clientClearDevices(ctx context.Context, r actorResolution) (Response, error) {
	usernames := make([]string, 0, len(r.Accounts))
	for _, a := range r.Accounts {
		usernames = append(usernames, a.Username)
	}
	if err := h.clearDeviceUsers(ctx, usernames, true); err != nil {
		return Response{}, err
	}
	return Response{Messages: []any{"✅ Aparelhos limpos."}}, nil
}
func (h *Handler) clientSupport(ctx context.Context, r actorResolution) (Response, error) {
	support := ""
	if len(r.Accounts) > 0 && r.Accounts[0].OwnerTelegramID != 0 {
		if rs, _ := h.Services.Store.FindReseller(ctx, r.Accounts[0].OwnerTelegramID); rs != nil {
			support = rs.WhatsAppPhone
		}
	}
	if support == "" && len(h.Services.Config.WhatsAppAdminNumbers) > 0 {
		support = h.Services.Config.WhatsAppAdminNumbers[0]
	}
	if support == "" {
		support = "suporte não configurado"
	}
	return Response{Messages: []any{"📞 Suporte: " + support}}, nil
}
func (h *Handler) clearDevicesScope(ctx context.Context, r actorResolution) error {
	if r.Actor.Role == model.RoleAdmin || r.Actor.IsAdmin {
		return h.clearAllDevices(ctx)
	}
	list, _ := h.Services.Store.ListAccounts(ctx, false)
	usernames := make([]string, 0, len(list))
	for _, a := range list {
		if canSeeAccount(r, a) {
			usernames = append(usernames, a.Username)
		}
	}
	return h.clearDeviceUsers(ctx, usernames, true)
}

func (h *Handler) clearAllDevices(ctx context.Context) error {
	if err := h.Services.Store.Exec(ctx, `DELETE FROM devices`); err != nil {
		return err
	}
	if err := checkuserdb.ClearAll(ctx, h.Services.Config.CheckUserDBPath); err != nil {
		return err
	}
	remoteCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err := remotesync.NewManager(h.Services.Config, h.Services.Store).SyncDeviceScope(remoteCtx)
	return err
}

func (h *Handler) clearDeviceUsers(ctx context.Context, usernames []string, batch bool) error {
	usernames = uniqueWADeviceUsernames(usernames)
	if len(usernames) == 0 {
		return nil
	}
	for _, username := range usernames {
		if err := h.Services.Store.ClearDevicesForUser(ctx, username, false); err != nil {
			return err
		}
	}
	if err := checkuserdb.ClearUsers(ctx, h.Services.Config.CheckUserDBPath, usernames); err != nil {
		return err
	}
	remoteCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	mgr := remotesync.NewManager(h.Services.Config, h.Services.Store)
	if len(usernames) == 1 && !batch {
		_, err := mgr.SyncDeviceUser(remoteCtx, usernames[0])
		return err
	}
	_, err := mgr.SyncDeviceUsers(remoteCtx, usernames)
	return err
}

func uniqueWADeviceUsernames(values []string) []string {
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

func (h *Handler) getSession(ctx context.Context, phone string) (session, error) {
	rows, err := h.Services.Store.Query(ctx, `SELECT state,data_json,updated_at FROM whatsapp_sessions WHERE phone=? LIMIT 1`, phone)
	if err != nil || len(rows) == 0 {
		return session{Data: map[string]string{}}, err
	}
	var data map[string]string
	_ = json.Unmarshal([]byte(rows[0]["data_json"]), &data)
	return session{State: rows[0]["state"], Data: data, UpdatedAt: parseTime(rows[0]["updated_at"])}, nil
}
func (h *Handler) setSession(ctx context.Context, phone string, s session) error {
	if s.Data == nil {
		s.Data = map[string]string{}
	}
	b, _ := json.Marshal(s.Data)
	now := time.Now().UTC().Format(time.RFC3339)
	return h.Services.Store.Exec(ctx, `INSERT INTO whatsapp_sessions(phone,state,data_json,updated_at) VALUES(?,?,?,?) ON CONFLICT(phone) DO UPDATE SET state=excluded.state,data_json=excluded.data_json,updated_at=excluded.updated_at`, phone, s.State, string(b), now)
}
func (h *Handler) clearSession(ctx context.Context, phone string) error {
	return h.Services.Store.Exec(ctx, `DELETE FROM whatsapp_sessions WHERE phone=?`, phone)
}

func accountPanel(a model.Account) string {
	return accountCardWA("✏️ Editar Conta", a)
}

func accountPanelWithActions(a model.Account) string {
	return accountPanel(a) + "\n\n1️⃣ Copiar dados\n2️⃣ Mudar senha\n3️⃣ Renovar\n4️⃣ Alterar limite\n5️⃣ Remover\n0️⃣ Voltar"
}

func accountCopyText(a model.Account) string {
	return accountCardWA("📋 Dados da Conta", a)
}

func accountSuccessTextWA(title string, a model.Account) string {
	if a.IsTrial || strings.Contains(strings.ToLower(title), "teste") {
		return accountCardWA("✅ Teste criado", a)
	}
	return accountCardWA(title, a)
}

func createdAccountPanel(title string, a model.Account) string {
	return accountSuccessTextWA(title, a)
}

func accountHasDisplayUUIDWA(a model.Account) bool {
	uuid := strings.TrimSpace(a.UUID)
	if !a.XrayEnabled || uuid == "" || uuid == "." || uuid == "-" {
		return false
	}
	if strings.EqualFold(uuid, "sem Xray") || strings.EqualFold(uuid, "sem xray") || strings.EqualFold(uuid, "none") || strings.EqualFold(uuid, "null") {
		return false
	}
	return true
}

func accountUUIDBlockWA(a model.Account) string {
	if !accountHasDisplayUUIDWA(a) {
		return ""
	}
	return "\n🔑 UUID:\n" + strings.TrimSpace(a.UUID)
}

func accountCardWA(title string, a model.Account) string {
	owner := nonEmpty(a.OwnerName, "Admin")
	return fmt.Sprintf(`%s
━━━━━━━━━━━━━━
👤 Usuário: %s
🔒 Senha: %s%s
📳 Limite: %d
━━━━━━━━━━━━━━
📱 WhatsApp: %s
💰 Valor: %s
📆 Expira: %s
━━━━━━━━━━━━━━
👑 Vendedor: %s`, title, a.Username, a.Password, accountUUIDBlockWA(a), nonZeroInt(a.LimitConnections, 1), nonEmpty(a.ClientWhatsApp, "-"), moneyWA(a.MonthlyValue), daysLeftWA(a.ExpiresAt), owner)
}

func formatWAResponse(resp Response) Response {
	for i, msg := range resp.Messages {
		switch v := msg.(type) {
		case string:
			resp.Messages[i] = formatWAText(v)
		case TextMessage:
			v.Text = formatWAText(v.Text)
			resp.Messages[i] = v
		case ImageMessage:
			v.Caption = formatWAText(v.Caption)
			resp.Messages[i] = v
		case DocumentMessage:
			v.Caption = formatWAText(v.Caption)
			resp.Messages[i] = v
		}
	}
	return resp
}

func formatWAText(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		prefix := strings.TrimSpace(line[:idx])
		cleanPrefix := strings.Trim(prefix, "*")
		if !waLabelPrefixOK(cleanPrefix) {
			continue
		}
		rest := line[idx+1:]
		if waCopyField(cleanPrefix) {
			trimmed := strings.TrimSpace(rest)
			if trimmed != "" && !strings.HasPrefix(trimmed, "`") {
				rest = " `" + strings.Trim(trimmed, "`") + "`"
			} else if trimmed == "" && i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				if next != "" && !strings.HasPrefix(next, "`") && !strings.Contains(next, "━━━━━━━━") {
					lines[i+1] = "`" + strings.Trim(next, "`") + "`"
				}
			}
		}
		lines[i] = "*" + cleanPrefix + ":*" + rest
	}
	return strings.Join(lines, "\n")
}
func waLabelPrefixOK(prefix string) bool {
	p := strings.TrimSpace(prefix)
	if p == "" || len([]rune(p)) > 48 || strings.Contains(p, "http") || strings.Contains(p, "/") || strings.Contains(p, "━━━━━━━━") {
		return false
	}
	return true
}
func waCopyField(prefix string) bool {
	p := strings.ToLower(strings.TrimSpace(prefix))
	return strings.Contains(p, "usuário") || strings.Contains(p, "usuario") || strings.Contains(p, "senha") || strings.Contains(p, "uuid")
}

func clientPixText(o model.PaymentOrder, username string, months int, amount float64) string {
	var blocks []string
	if strings.TrimSpace(o.PixCopyPaste) != "" {
		blocks = append(blocks, "Pix copia e cola:\n"+o.PixCopyPaste)
	}
	if strings.TrimSpace(o.PaymentURL) != "" {
		blocks = append(blocks, "Link de pagamento:\n"+o.PaymentURL)
	}
	if len(blocks) == 0 {
		blocks = append(blocks, "Pix/link ainda não retornado pelo banco. Consulte o suporte em alguns instantes.")
	}
	return fmt.Sprintf("💳 Pedido Pix\n━━━━━━━━━━━━━━\n👤 Usuário: %s\n⏳ Tempo: %d mês(es)\n💰 Valor: %s\n🧾 Pedido: %s\n━━━━━━━━PIX━━━━━━━━\n%s\n━━━━━━━━AVISO━━━━━━━━\n✅ Após a aprovação, sua conta será renovada automaticamente.", username, months, moneyWA(amount), o.OrderID, strings.Join(blocks, "\n\n"))
}

func parseMoneyWA(s string) (float64, error) {
	return strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", "."), 64)
}
func moneyWA(v float64) string { return "R$ " + strings.ReplaceAll(fmt.Sprintf("%.2f", v), ".", ",") }
func daysLeftWA(t time.Time) string {
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
func brDateWA(t time.Time) string {
	if t.IsZero() {
		return "sem validade"
	}
	return t.Local().Format("02/01/2006")
}
func nonZeroInt(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}
func expiredForWA(t time.Time) string {
	if t.IsZero() {
		return "expirado"
	}
	d := time.Since(t)
	if d < 24*time.Hour {
		return "hoje"
	}
	return strconv.Itoa(int(d.Hours()/24)) + " dias atrás"
}
func digits(s string) string                         { re := regexp.MustCompile(`\D+`); return re.ReplaceAllString(s, "") }
func norm(s string) string                           { return strings.ToLower(strings.TrimSpace(s)) }
func isCancel(s string) bool                         { s = choiceKey(s); return s == "0" || s == "cancelar" || s == "voltar" }
func isYes(s string) bool                            { s = choiceKey(s); return s == "1" || s == "sim" || s == "s" || s == "yes" }
func mapChoice(s string, m map[string]string) string { return m[choiceKey(s)] }
func waYesNoInput(s string) string {
	t := choiceKey(s)
	switch t {
	case "1", "sim", "s", "yes", "y":
		return "yes"
	case "2", "0", "nao", "não", "n", "no", "pular":
		return "no"
	default:
		return ""
	}
}
func waAskWhatsAppText(title, username, pass string) string {
	return fmt.Sprintf("%s\n━━━━━━━━━━━━━━\n👤 Usuário: %s\n🔒 Senha: %s\n━━━━━━━━━━━━━━\n📱 Deseja adicionar WhatsApp ao cliente?\n\n1️⃣ Sim\n2️⃣ Não", title, username, pass)
}
func waClientWhatsAppPrompt() string {
	return "📞 Digite o WhatsApp do cliente:\n━━━━━━━━━━━━━━\nEx: 5585912345678 ou 0 para pular"
}
func waMonthlyValuePrompt() string {
	return "💰 Digite o valor mensal:\n━━━━━━━━━━━━━━\nEx: 20 ou 0 para sem valor"
}
func waAccountPreChoiceText(title string, data map[string]string, value float64, next string) string {
	lines := []string{title, "━━━━━━━━━━━━━━", "👤 Usuário: " + data["username"], "🔒 Senha: " + data["password"]}
	if strings.TrimSpace(data["client_whatsapp"]) != "" {
		lines = append(lines, "📱 WhatsApp: "+data["client_whatsapp"])
	}
	lines = append(lines, "💰 Valor: "+moneyWA(value), "━━━━━━━━━━━━━━", next)
	return strings.Join(lines, "\n")
}
func choiceKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "️", "")
	s = strings.ReplaceAll(s, "⃣", "")
	keycap := map[string]string{"0️⃣": "0", "1️⃣": "1", "2️⃣": "2", "3️⃣": "3", "4️⃣": "4", "5️⃣": "5", "6️⃣": "6", "7️⃣": "7", "8️⃣": "8", "9️⃣": "9"}
	if v, ok := keycap[s]; ok {
		return v
	}
	fields := strings.Fields(s)
	if len(fields) > 0 {
		f := fields[0]
		if v, ok := keycap[f]; ok {
			return v
		}
		f = strings.Trim(f, ".-) ")
		if f != "" {
			return f
		}
	}
	return s
}
func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
func left(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Until(t)
	if d < 0 {
		return "expirado"
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%02dh:%02d", int(d.Hours()), int(d.Minutes())%60)
	}
	return strconv.Itoa(int(d.Hours()/24)) + " dias"
}
func parseTime(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
func randomDigits(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		x, _ := rand.Int(rand.Reader, big.NewInt(10))
		b.WriteString(strconv.Itoa(int(x.Int64())))
	}
	return b.String()
}
func newUUIDLike() string {
	parts := []int{8, 4, 4, 4, 12}
	var out []string
	for _, p := range parts {
		var b strings.Builder
		for i := 0; i < p; i++ {
			x, _ := rand.Int(rand.Reader, big.NewInt(16))
			b.WriteString(strconv.FormatInt(x.Int64(), 16))
		}
		out = append(out, b.String())
	}
	return strings.Join(out, "-")
}
