package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
)

const (
	KindRenew        = "renew"
	KindLimit        = "limit"
	KindRenewLimit   = "renew_limit"
	KindAccountRenew = "account_renew"

	BankMercadoPago = "mercado_pago"
	BankAsaas       = "asaas"
	BankInfinitePay = "infinitepay"
)

type Manager struct {
	Store *store.DB
	// AfterApply é chamado após um pagamento aprovado ser aplicado no SQLite.
	// Usado pelo serviço HTTP para refletir renovação de conta no Linux/Xray/VPS.
	AfterApply func(context.Context, model.PaymentOrder) error
}

func NewManager(st *store.DB) *Manager { return &Manager{Store: st} }
func (m *Manager) SetAfterApply(fn func(context.Context, model.PaymentOrder) error) *Manager {
	m.AfterApply = fn
	return m
}

type OwnerConfigInput struct {
	OwnerID  int64
	Bank     string
	Token    string
	Enabled  bool
	DataJSON string
}

func (m *Manager) ConfigureOwner(ctx context.Context, in OwnerConfigInput) (*model.PaymentOwnerConfig, error) {
	bank := normalizeBank(in.Bank)
	if bank == "" {
		return nil, errors.New("banco inválido")
	}
	if strings.TrimSpace(in.DataJSON) == "" {
		in.DataJSON = "{}"
	}
	if !json.Valid([]byte(in.DataJSON)) {
		return nil, errors.New("data_json inválido")
	}
	cfg := model.PaymentOwnerConfig{OwnerID: in.OwnerID, Bank: bank, Enabled: in.Enabled, Token: strings.TrimSpace(in.Token), DataJSON: in.DataJSON, UpdatedAt: time.Now().UTC()}
	if err := m.Store.UpsertPaymentOwnerConfig(ctx, cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type PackageInput struct {
	ID      int64
	OwnerID int64
	Kind    string
	Name    string
	Months  int
	Days    int
	Credits int
	Amount  float64
	Active  bool
}

func (m *Manager) UpsertPackage(ctx context.Context, in PackageInput) (*model.PaymentPackage, error) {
	kind := normalizeKind(in.Kind)
	if kind == "" {
		return nil, errors.New("tipo de pacote inválido")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, errors.New("nome do pacote vazio")
	}
	if in.Amount < 0 {
		return nil, errors.New("valor inválido")
	}
	if in.Months < 0 || in.Days < 0 || in.Credits < 0 {
		return nil, errors.New("meses/dias/limites inválidos")
	}
	if in.Days == 0 && in.Months > 0 {
		in.Days = in.Months * 30
	}
	p := model.PaymentPackage{ID: in.ID, OwnerID: in.OwnerID, Kind: kind, Name: name, Months: in.Months, Days: in.Days, Credits: in.Credits, Amount: in.Amount, Active: in.Active}
	return m.Store.UpsertPaymentPackage(ctx, p)
}

type OrderInput struct {
	OwnerID          int64
	TargetResellerID int64
	Kind             string
	Months           int
	Days             int
	Credits          int
	Amount           float64
	Bank             string
	Description      string
}

func (m *Manager) CreateOrder(ctx context.Context, in OrderInput) (*model.PaymentOrder, error) {
	kind := normalizeKind(in.Kind)
	if kind == "" {
		return nil, errors.New("tipo de pedido inválido")
	}
	bank := normalizeBank(in.Bank)
	if bank == "" {
		if cfg, _ := m.Store.FindPaymentOwnerConfig(ctx, in.OwnerID); cfg != nil {
			bank = cfg.Bank
		}
	}
	if bank == "" {
		return nil, errors.New("banco não configurado")
	}
	if in.TargetResellerID <= 0 && kind != KindAccountRenew {
		return nil, errors.New("revenda alvo inválida")
	}
	if in.Amount < 0 {
		return nil, errors.New("valor inválido")
	}
	if in.Days == 0 && in.Months > 0 {
		in.Days = in.Months * 30
	}
	if (kind == KindRenew || kind == KindRenewLimit || kind == KindAccountRenew) && in.Days <= 0 {
		return nil, errors.New("pedido de renovação sem dias")
	}
	if (kind == KindLimit || kind == KindRenewLimit) && in.Credits <= 0 {
		return nil, errors.New("pedido de limite sem quantidade de limites")
	}
	id := "PC" + time.Now().UTC().Format("20060102150405") + randHex(4)
	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		desc = kind
	}
	payload := map[string]any{"description": desc, "created_by": "primecel-gestor"}
	b, _ := json.Marshal(payload)
	o := model.PaymentOrder{OrderID: id, OwnerID: in.OwnerID, TargetResellerID: in.TargetResellerID, Kind: kind, Months: in.Months, Days: in.Days, Credits: in.Credits, Amount: in.Amount, Bank: bank, ExternalID: id, Status: "pending", PayloadJSON: string(b), CreatedAt: time.Now().UTC()}
	charge, err := m.CreateBankCharge(ctx, &o, desc)
	if err != nil {
		return nil, err
	}
	o.ExternalID = firstNonEmpty(charge.ExternalID, o.ExternalID)
	o.PixCopyPaste = charge.PixCopyPaste
	o.PaymentURL = charge.PaymentURL
	if charge.RawJSON != "" {
		o.PayloadJSON = charge.RawJSON
	}
	if err := m.Store.InsertPaymentOrder(ctx, o); err != nil {
		return nil, err
	}
	_ = m.Store.AddPaymentEvent(ctx, o.OrderID, o.OwnerID, "created", string(b))
	return &o, nil
}

type ChargeResult struct {
	ExternalID   string `json:"external_id"`
	PixCopyPaste string `json:"pix_copy_paste"`
	PaymentURL   string `json:"payment_url"`
	RawJSON      string `json:"raw_json"`
}

type ownerPaymentData struct {
	PayerEmail        string `json:"payer_email"`
	PayerFirstName    string `json:"payer_first_name"`
	PayerLastName     string `json:"payer_last_name"`
	AsaasCustomerID   string `json:"asaas_customer_id"`
	AsaasCustomerCPF  string `json:"asaas_customer_cpf_cnpj"`
	InfinitePayTag    string `json:"infinitepay_tag"`
	InfinitePayHandle string `json:"infinitepay_handle"`
	WebhookURL        string `json:"webhook_url"`
	RedirectURL       string `json:"redirect_url"`
}

func (m *Manager) CreateBankCharge(ctx context.Context, o *model.PaymentOrder, description string) (ChargeResult, error) {
	cfg, _ := m.Store.FindPaymentOwnerConfig(ctx, o.OwnerID)
	if cfg == nil || !cfg.Enabled {
		pix, url := buildPaymentPlaceholder(*o)
		return ChargeResult{ExternalID: o.OrderID, PixCopyPaste: pix, PaymentURL: url, RawJSON: string(mustJSON(map[string]any{"provider": "test_mode", "order_id": o.OrderID, "bank": o.Bank}))}, nil
	}
	var data ownerPaymentData
	_ = json.Unmarshal([]byte(cfg.DataJSON), &data)
	needsToken := o.Bank != BankInfinitePay
	if needsToken && (strings.TrimSpace(cfg.Token) == "" || strings.EqualFold(strings.TrimSpace(cfg.Token), "TEST")) {
		pix, url := buildPaymentPlaceholder(*o)
		return ChargeResult{ExternalID: o.OrderID, PixCopyPaste: pix, PaymentURL: url, RawJSON: string(mustJSON(map[string]any{"provider": "test_mode", "order_id": o.OrderID, "bank": o.Bank}))}, nil
	}
	switch o.Bank {
	case BankMercadoPago:
		return createMercadoPagoCharge(ctx, *cfg, data, *o, description)
	case BankAsaas:
		return createAsaasCharge(ctx, *cfg, data, *o, description)
	case BankInfinitePay:
		return m.createInfinitePayCharge(ctx, *cfg, data, *o, description)
	default:
		return ChargeResult{}, errors.New("banco não suportado")
	}
}

func createMercadoPagoCharge(ctx context.Context, cfg model.PaymentOwnerConfig, data ownerPaymentData, o model.PaymentOrder, description string) (ChargeResult, error) {
	payerEmail := firstNonEmpty(data.PayerEmail, "cliente+"+strings.ToLower(o.OrderID)+"@primecel.local")
	body := map[string]any{
		"transaction_amount": round2(o.Amount),
		"description":        description,
		"payment_method_id":  "pix",
		"external_reference": o.OrderID,
		"payer":              map[string]any{"email": payerEmail, "first_name": firstNonEmpty(data.PayerFirstName, "PrimeCel"), "last_name": firstNonEmpty(data.PayerLastName, "Cliente")},
	}
	raw, err := doJSON(ctx, http.MethodPost, "https://api.mercadopago.com/v1/payments", cfg.Token, map[string]string{"X-Idempotency-Key": o.OrderID}, body)
	if err != nil {
		return ChargeResult{}, err
	}
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	pix := findString(obj, "qr_code")
	url := findString(obj, "ticket_url")
	ext := fmt.Sprint(obj["id"])
	if pix == "" && url == "" {
		return ChargeResult{}, fmt.Errorf("mercado pago sem pix na resposta: %s", string(raw))
	}
	return ChargeResult{ExternalID: firstNonEmpty(ext, o.OrderID), PixCopyPaste: pix, PaymentURL: url, RawJSON: string(raw)}, nil
}

func createAsaasCharge(ctx context.Context, cfg model.PaymentOwnerConfig, data ownerPaymentData, o model.PaymentOrder, description string) (ChargeResult, error) {
	asaasHeaders := map[string]string{"access_token": strings.TrimSpace(cfg.Token), "User-Agent": "primecel-gestor"}
	customerID := strings.TrimSpace(data.AsaasCustomerID)
	if customerID == "" {
		if strings.TrimSpace(data.AsaasCustomerCPF) == "" {
			return ChargeResult{}, errors.New("Asaas exige asaas_customer_id ou asaas_customer_cpf_cnpj em data_json")
		}
		customerBody := map[string]any{"name": "PrimeCel " + o.OrderID, "cpfCnpj": data.AsaasCustomerCPF}
		if data.PayerEmail != "" {
			customerBody["email"] = data.PayerEmail
		}
		raw, err := doJSON(ctx, http.MethodPost, "https://api.asaas.com/v3/customers", "", asaasHeaders, customerBody)
		if err != nil {
			return ChargeResult{}, err
		}
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		customerID = strings.TrimSpace(fmt.Sprint(obj["id"]))
		if customerID == "" {
			return ChargeResult{}, fmt.Errorf("Asaas não retornou customer id: %s", string(raw))
		}
	}
	due := time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02")
	payBody := map[string]any{"customer": customerID, "billingType": "PIX", "value": round2(o.Amount), "dueDate": due, "description": description, "externalReference": o.OrderID}
	raw, err := doJSON(ctx, http.MethodPost, "https://api.asaas.com/v3/payments", "", asaasHeaders, payBody)
	if err != nil {
		return ChargeResult{}, err
	}
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	pid := strings.TrimSpace(fmt.Sprint(obj["id"]))
	if pid == "" {
		return ChargeResult{}, fmt.Errorf("Asaas não retornou payment id: %s", string(raw))
	}
	qrRaw, err := doJSON(ctx, http.MethodGet, "https://api.asaas.com/v3/payments/"+pid+"/pixQrCode", "", asaasHeaders, nil)
	if err != nil {
		return ChargeResult{}, err
	}
	var qr map[string]any
	_ = json.Unmarshal(qrRaw, &qr)
	pix := firstNonEmpty(findString(qr, "payload"), findString(qr, "pixCopyPaste"), findString(qr, "qrCode"))
	url := firstNonEmpty(fmt.Sprint(obj["invoiceUrl"]), fmt.Sprint(obj["bankSlipUrl"]))
	merged := map[string]any{"payment": obj, "pix_qr_code": qr}
	mergedRaw, _ := json.Marshal(merged)
	if pix == "" && url == "" {
		return ChargeResult{}, fmt.Errorf("Asaas sem pix na resposta: %s", string(qrRaw))
	}
	return ChargeResult{ExternalID: pid, PixCopyPaste: pix, PaymentURL: url, RawJSON: string(mergedRaw)}, nil
}

func (m *Manager) createInfinitePayCharge(ctx context.Context, cfg model.PaymentOwnerConfig, data ownerPaymentData, o model.PaymentOrder, description string) (ChargeResult, error) {
	handle := firstNonEmpty(data.InfinitePayHandle, data.InfinitePayTag, cfg.Token)
	handle = strings.TrimPrefix(strings.TrimSpace(handle), "$")
	if handle == "" {
		return ChargeResult{}, errors.New("InfinitePay exige InfiniteTag/Handle em data_json")
	}
	amountCents := int(round2(o.Amount) * 100)
	itemDescription := strings.TrimSpace(description)
	if itemDescription == "" {
		itemDescription = "Renovação Primecel"
	}
	body := map[string]any{
		"handle":    handle,
		"order_nsu": o.OrderID,
		"items": []map[string]any{{
			"description": itemDescription,
			"name":        itemDescription,
			"quantity":    1,
			"price":       amountCents,
		}},
	}
	webhookURL := strings.TrimSpace(data.WebhookURL)
	if webhookURL == "" && m.Store != nil {
		webhookURL, _ = m.Store.GetSetting(ctx, "payments_webhook_url")
	}
	if webhookURL == "" {
		webhookURL = "https://api.primecel.shop/pix"
	}
	if webhookURL != "" {
		body["webhook_url"] = webhookURL
	}
	if strings.TrimSpace(data.RedirectURL) != "" {
		body["redirect_url"] = strings.TrimSpace(data.RedirectURL)
	}
	raw, err := doJSON(ctx, http.MethodPost, "https://api.checkout.infinitepay.io/links", "", nil, body)
	if err != nil {
		return ChargeResult{}, err
	}
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	url := firstNonEmpty(findString(obj, "url"), findString(obj, "checkout_url"), findString(obj, "payment_url"), findString(obj, "link"))
	id := firstNonEmpty(findString(obj, "id"), findString(obj, "order_nsu"), o.OrderID)
	if url == "" {
		return ChargeResult{}, fmt.Errorf("InfinitePay sem link na resposta: %s", string(raw))
	}
	return ChargeResult{ExternalID: id, PaymentURL: url, RawJSON: string(raw)}, nil
}

func doJSON(ctx context.Context, method, url, token string, headers map[string]string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("access_token", token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: status %d: %s", method, url, resp.StatusCode, string(out))
	}
	return out, nil
}

func round2(v float64) float64 { f, _ := strconv.ParseFloat(fmt.Sprintf("%.2f", v), 64); return f }
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" && strings.TrimSpace(v) != "<nil>" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (m *Manager) MarkPaid(ctx context.Context, orderID string, rawPayload []byte) (*model.PaymentOrder, error) {
	// Comando manual do admin/CLI: usado para conciliação. O WebHook usa MarkPaidConfirmed com confirmação bancária.
	return m.MarkPaidConfirmed(ctx, orderID, rawPayload, true)
}

func (m *Manager) MarkPaidConfirmed(ctx context.Context, orderID string, rawPayload []byte, alreadyConfirmed bool) (*model.PaymentOrder, error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, errors.New("order_id vazio")
	}
	var out *model.PaymentOrder
	err := m.Store.WithTx(ctx, func(tx *store.DB) error {
		o, err := tx.FindPaymentOrder(ctx, orderID)
		if err != nil {
			return err
		}
		if o == nil {
			return errors.New("pedido não encontrado")
		}
		if o.AppliedAt != nil {
			out = o
			return nil
		}
		if !alreadyConfirmed && !payloadLooksPaid(rawPayload) {
			return errors.New("pagamento ainda não confirmado")
		}
		now := time.Now().UTC()
		o.Status = "approved"
		o.PaidAt = &now
		if len(rawPayload) > 0 {
			if o.Kind == KindAccountRenew {
				o.PayloadJSON = mergeAccountRenewPayload(o.PayloadJSON, rawPayload)
			} else if json.Valid(rawPayload) {
				o.PayloadJSON = string(rawPayload)
			} else {
				o.PayloadJSON = string(mustJSON(map[string]string{"raw": string(rawPayload)}))
			}
		}
		if err := applyOrder(ctx, tx, o, now); err != nil {
			return err
		}
		o.AppliedAt = &now
		if err := tx.UpdatePaymentOrder(ctx, *o); err != nil {
			return err
		}
		if err := tx.AddPaymentEvent(ctx, o.OrderID, o.OwnerID, "approved_applied", o.PayloadJSON); err != nil {
			return err
		}
		out = o
		return nil
	})
	if err == nil && out != nil && m.AfterApply != nil {
		if hookErr := m.AfterApply(ctx, *out); hookErr != nil {
			_ = m.Store.AddPaymentEvent(ctx, out.OrderID, out.OwnerID, "post_apply_error", string(mustJSON(map[string]string{"error": hookErr.Error()})))
		}
	}
	return out, err
}

func mergeAccountRenewPayload(existing string, raw []byte) string {
	merged := map[string]any{}
	if json.Valid([]byte(existing)) {
		_ = json.Unmarshal([]byte(existing), &merged)
	}
	if len(raw) == 0 {
		b, _ := json.Marshal(merged)
		return string(b)
	}
	var incoming map[string]any
	if json.Unmarshal(raw, &incoming) == nil {
		for k, v := range incoming {
			// Não deixa o retorno do banco apagar os dados internos necessários para aplicar a renovação.
			if v == nil && (k == "username" || k == "plan_id" || k == "plan_name") {
				continue
			}
			merged[k] = v
		}
		merged["payment_payload"] = incoming
	} else {
		merged["payment_raw"] = string(raw)
	}
	b, _ := json.Marshal(merged)
	return string(b)
}

func applyOrder(ctx context.Context, tx *store.DB, o *model.PaymentOrder, now time.Time) error {
	if o.Kind == KindAccountRenew {
		var data map[string]any
		_ = json.Unmarshal([]byte(o.PayloadJSON), &data)
		username := strings.TrimSpace(fmt.Sprint(data["username"]))
		if username == "" || username == "<nil>" {
			return errors.New("pedido sem usuário da conta")
		}
		if expected := anyFloat(data["expected_amount"]); expected >= 0 && round2(o.Amount) != round2(expected) {
			return errors.New("valor do pedido não confere com o plano")
		}
		if round2(o.Amount) <= 0 {
			freeOK, _ := data["free_renewal_validated"].(bool)
			months := anyInt(data["months"])
			monthly := anyFloat(data["account_monthly_value"])
			if !freeOK || months != 1 || round2(monthly) > 0 {
				return errors.New("renovação grátis não autorizada")
			}
		}
		acc, err := tx.FindAccount(ctx, username)
		if err != nil || acc == nil {
			return errors.New("conta não encontrada")
		}
		base := now
		if acc.ExpiresAt.After(base) {
			base = acc.ExpiresAt
		}
		days := o.Days
		if days == 0 && o.Months > 0 {
			days = o.Months * 30
		}
		if days <= 0 {
			return errors.New("pedido sem dias de renovação")
		}
		acc.ExpiresAt = base.AddDate(0, 0, days)
		acc.ExpiryDate = acc.ExpiresAt.AddDate(0, 0, -1).Format("2006-01-02")
		// Renovou teste pelo site/API: passa a ser conta normal.
		acc.IsTrial = false
		acc.Status = "active"
		acc.UpdatedAt = now
		if err := tx.UpsertAccount(ctx, *acc); err != nil {
			return err
		}
		_ = tx.UpsertDeviceUser(ctx, acc.Username, acc.UUID, acc.LimitConnections)

		payload := map[string]any{}
		_ = json.Unmarshal([]byte(o.PayloadJSON), &payload)
		payload["new_expires_at"] = acc.ExpiresAt.UTC().Format(time.RFC3339)
		payload["new_expiry_date"] = acc.ExpiryDate
		payload["renewed_username"] = acc.Username
		payload["renewed_by_payment"] = true
		b, _ := json.Marshal(payload)
		return tx.AddAccountEvent(ctx, acc.Username, "renew_payment", string(b), o.OwnerID)
	}
	r, err := tx.FindReseller(ctx, o.TargetResellerID)
	if err != nil || r == nil {
		return errors.New("revenda alvo não encontrada")
	}
	if o.Kind == KindRenew || o.Kind == KindRenewLimit {
		base := now
		if r.ExpiresAt.After(base) {
			base = r.ExpiresAt
		}
		days := o.Days
		if days == 0 && o.Months > 0 {
			days = o.Months * 30
		}
		if days <= 0 {
			return errors.New("pedido sem dias de renovação")
		}
		r.ExpiresAt = base.AddDate(0, 0, days)
		r.Active = true
		r.PendingMonthlyDifference = 0
		r.PendingMonthlyPrice = 0
	}
	if o.Kind == KindLimit || o.Kind == KindRenewLimit {
		if o.Credits <= 0 {
			return errors.New("pedido sem limites")
		}
		r.Credits += o.Credits
		if err := tx.AddCreditMovement(ctx, r.TelegramID, o.Credits, "payment_credit", "", 0, o.OwnerID); err != nil {
			return err
		}
	}
	r.UpdatedAt = now
	return tx.UpsertReseller(ctx, *r)
}

func (m *Manager) ExtractOrderID(payload []byte) string {
	var obj any
	if json.Unmarshal(payload, &obj) == nil {
		if id := findString(obj, "order_id", "external_reference", "externalReference", "order_nsu", "reference", "idempotency_key"); id != "" {
			return id
		}
	}
	re := regexp.MustCompile(`PC[0-9]{14}[0-9a-fA-F]+`)
	return re.FindString(string(payload))
}

func (m *Manager) ProcessWebhook(r *http.Request, body []byte) (map[string]any, int) {
	ctx := r.Context()
	orderID := strings.TrimSpace(r.URL.Query().Get("order_id"))
	if orderID == "" {
		orderID = m.ExtractOrderID(body)
	}
	eventID := webhookEventID(r, body, orderID)
	remoteIP := clientIP(r)
	headersJSON := headersSummaryJSON(r)
	bodyJSON := compactBodyJSON(body)
	baseEvent := model.PaymentWebhookEvent{EventID: eventID, OrderID: orderID, RemoteIP: remoteIP, HeadersJSON: headersJSON, BodyJSON: bodyJSON, CreatedAt: time.Now().UTC()}
	if orderID == "" {
		baseEvent.Result = "ignored"
		baseEvent.ErrorText = "order_id não encontrado"
		_, _ = m.Store.AddPaymentWebhookEvent(ctx, baseEvent)
		return map[string]any{"ok": false, "error": baseEvent.ErrorText}, http.StatusBadRequest
	}
	o, err := m.Store.FindPaymentOrder(ctx, orderID)
	if err != nil || o == nil {
		baseEvent.Result = "ignored"
		baseEvent.ErrorText = "pedido não encontrado"
		_, _ = m.Store.AddPaymentWebhookEvent(ctx, baseEvent)
		return map[string]any{"ok": false, "order_id": orderID, "error": baseEvent.ErrorText}, http.StatusBadRequest
	}
	baseEvent.OwnerID, baseEvent.Bank, baseEvent.ExternalID = o.OwnerID, o.Bank, o.ExternalID
	if eventID != "" {
		inserted, _ := m.Store.AddPaymentWebhookEvent(ctx, model.PaymentWebhookEvent{EventID: eventID, OrderID: orderID, OwnerID: o.OwnerID, Bank: o.Bank, ExternalID: o.ExternalID, Status: "received", RemoteIP: remoteIP, Result: "received", HeadersJSON: headersJSON, BodyJSON: bodyJSON, CreatedAt: time.Now().UTC()})
		if !inserted {
			return map[string]any{"ok": true, "duplicate": true, "order_id": o.OrderID, "status": o.Status, "applied_at": o.AppliedAt}, http.StatusOK
		}
	}
	cfg, _ := m.Store.FindPaymentOwnerConfig(ctx, o.OwnerID)
	if err := validateWebhookAuth(*o, cfg, r, body); err != nil {
		baseEvent.Status = "auth_failed"
		baseEvent.Result = "blocked"
		baseEvent.ErrorText = err.Error()
		_, _ = m.Store.AddPaymentWebhookEvent(ctx, baseEvent)
		return map[string]any{"ok": false, "order_id": orderID, "error": err.Error()}, http.StatusUnauthorized
	}
	confirmed, status, rawConfirm, err := m.ConfirmPayment(ctx, *o, cfg, body)
	baseEvent.Status = status
	if err != nil {
		baseEvent.Result = "failed"
		baseEvent.ErrorText = err.Error()
		_, _ = m.Store.AddPaymentWebhookEvent(ctx, baseEvent)
		return map[string]any{"ok": false, "order_id": orderID, "status": status, "error": err.Error()}, http.StatusBadRequest
	}
	if !confirmed {
		baseEvent.Result = "ignored"
		baseEvent.ErrorText = "pagamento ainda não aprovado"
		_, _ = m.Store.AddPaymentWebhookEvent(ctx, baseEvent)
		return map[string]any{"ok": true, "order_id": orderID, "status": status, "applied": false}, http.StatusOK
	}
	raw := rawConfirm
	if len(raw) == 0 {
		raw = body
	}
	paid, err := m.MarkPaidConfirmed(ctx, orderID, raw, true)
	if err != nil {
		baseEvent.Result = "failed"
		baseEvent.ErrorText = err.Error()
		_, _ = m.Store.AddPaymentWebhookEvent(ctx, baseEvent)
		return map[string]any{"ok": false, "order_id": orderID, "status": status, "error": err.Error()}, http.StatusBadRequest
	}
	baseEvent.Result = "approved_applied"
	_, _ = m.Store.AddPaymentWebhookEvent(ctx, baseEvent)
	return map[string]any{"ok": true, "order_id": paid.OrderID, "status": paid.Status, "applied_at": paid.AppliedAt}, http.StatusOK
}

func (m *Manager) WebhookHandler() http.Handler {
	mux := http.NewServeMux()
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		resp, code := m.ProcessWebhook(r, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	}
	apiPlans := func(w http.ResponseWriter, r *http.Request) {
		m.servePublicPlans(w, r)
	}
	mux.HandleFunc("/webhook", h)
	mux.HandleFunc("/pix", h)
	mux.HandleFunc("/pix/webhook", h)
	mux.HandleFunc("/api/plans", apiPlans)
	mux.HandleFunc("/api/planos", apiPlans)
	mux.HandleFunc("/api/packages", apiPlans)
	mux.HandleFunc("/api/pacotes", apiPlans)
	mux.HandleFunc("/api/renovacao/login", m.serveRenewalLogin)
	mux.HandleFunc("/api/renovacao/pix", m.serveRenewalPix)
	mux.HandleFunc("/api/renovacao/pix/status", m.serveRenewalPixStatus)
	mux.HandleFunc("/api/renovacao/session", m.serveRenewalSession)
	mux.HandleFunc("/api/renewal/login", m.serveRenewalLogin)
	mux.HandleFunc("/api/renewal/pix", m.serveRenewalPix)
	mux.HandleFunc("/api/renewal/pix/status", m.serveRenewalPixStatus)
	mux.HandleFunc("/api/renewal/session", m.serveRenewalSession)
	mux.HandleFunc("/plans", apiPlans)
	status := func(w http.ResponseWriter, r *http.Request) {
		setPublicAPIHeaders(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "primecel-webhook", "time": time.Now().UTC().Format(time.RFC3339), "api_plans": "/api/plans"})
	}
	mux.HandleFunc("/webhook/status", status)
	mux.HandleFunc("/pix/status", status)
	return mux
}

type renewalLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type renewalPixRequest struct {
	PlanID string `json:"planId"`
}

type renewalSession struct {
	Token     string
	Username  string
	ExpiresAt time.Time
}

type renewalPlan struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Days        int     `json:"days"`
	Months      int     `json:"months"`
	Price       float64 `json:"price"`
	Amount      float64 `json:"amount"`
	Description string  `json:"description"`
	Popular     bool    `json:"popular,omitempty"`
	PackageID   int64   `json:"package_id,omitempty"`
}

func (m *Manager) serveRenewalLogin(w http.ResponseWriter, r *http.Request) {
	setPublicAPIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "message": "method not allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var in renewalLoginRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "json inválido"})
		return
	}
	username := strings.TrimSpace(in.Username)
	password := strings.TrimSpace(in.Password)
	if username == "" || password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "informe usuário e senha"})
		return
	}
	acc, err := m.Store.FindAccount(ctx, username)
	if err != nil || acc == nil || acc.DeletedAt != nil || acc.Status == "deleted" || acc.Password != password {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "usuário ou senha inválidos"})
		return
	}
	ownerID, sellerName := m.renewalOwnerForAccount(ctx, *acc)
	cfg, _ := m.Store.FindPaymentOwnerConfig(ctx, ownerID)
	paymentConfigured := cfg != nil && cfg.Enabled && strings.TrimSpace(cfg.Bank) != ""
	plans, _ := m.renewalPlansForAccount(ctx, *acc, ownerID)
	if len(plans) == 0 {
		paymentConfigured = false
	}
	token, err := m.createRenewalSession(ctx, acc.Username)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"token": token,
		"account": map[string]any{
			"username":      acc.Username,
			"displayName":   acc.Username,
			"expiresAt":     acc.ExpiresAt.UTC().Format(time.RFC3339),
			"expiryDate":    acc.ExpiryDate,
			"status":        renewalAccountStatus(*acc),
			"limit":         acc.LimitConnections,
			"monthlyValue":  round2(acc.MonthlyValue),
			"monthly_value": round2(acc.MonthlyValue),
		},
		"seller": map[string]any{
			"id":                ownerID,
			"name":              sellerName,
			"paymentConfigured": paymentConfigured,
		},
		"plans": plans,
	})
}

func (m *Manager) serveRenewalSession(w http.ResponseWriter, r *http.Request) {
	setPublicAPIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "message": "method not allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	sess, ok := m.renewalSessionFromRequest(ctx, r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "sessão expirada"})
		return
	}
	data, err := m.renewalSessionPayload(ctx, sess.Username, sess.Token, sess.ExpiresAt)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (m *Manager) renewalSessionPayload(ctx context.Context, username, token string, sessionExpiresAt time.Time) (map[string]any, error) {
	acc, err := m.Store.FindAccount(ctx, username)
	if err != nil || acc == nil || acc.DeletedAt != nil || acc.Status == "deleted" {
		return nil, errors.New("conta não encontrada")
	}
	ownerID, sellerName := m.renewalOwnerForAccount(ctx, *acc)
	cfg, _ := m.Store.FindPaymentOwnerConfig(ctx, ownerID)
	paymentConfigured := cfg != nil && cfg.Enabled && strings.TrimSpace(cfg.Bank) != ""
	plans, _ := m.renewalPlansForAccount(ctx, *acc, ownerID)
	if len(plans) == 0 {
		paymentConfigured = false
	}
	return map[string]any{
		"ok":               true,
		"token":            token,
		"sessionExpiresAt": sessionExpiresAt.UTC().Format(time.RFC3339),
		"account": map[string]any{
			"username":      acc.Username,
			"displayName":   acc.Username,
			"expiresAt":     acc.ExpiresAt.UTC().Format(time.RFC3339),
			"expiryDate":    acc.ExpiryDate,
			"status":        renewalAccountStatus(*acc),
			"limit":         acc.LimitConnections,
			"monthlyValue":  round2(acc.MonthlyValue),
			"monthly_value": round2(acc.MonthlyValue),
		},
		"seller": map[string]any{
			"id":                ownerID,
			"name":              sellerName,
			"paymentConfigured": paymentConfigured,
		},
		"plans": plans,
	}, nil
}

func (m *Manager) serveRenewalPix(w http.ResponseWriter, r *http.Request) {
	setPublicAPIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "message": "method not allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	sess, ok := m.renewalSessionFromRequest(ctx, r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "sessão inválida"})
		return
	}
	var in renewalPixRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "json inválido"})
		return
	}
	acc, err := m.Store.FindAccount(ctx, sess.Username)
	if err != nil || acc == nil || acc.DeletedAt != nil || acc.Status == "deleted" {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "conta não encontrada"})
		return
	}
	ownerID, _ := m.renewalOwnerForAccount(ctx, *acc)
	cfg, _ := m.Store.FindPaymentOwnerConfig(ctx, ownerID)
	if cfg == nil || !cfg.Enabled || strings.TrimSpace(cfg.Bank) == "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "message": "pagamento não configurado para esta revenda"})
		return
	}
	plan, err := m.resolveRenewalPlan(ctx, *acc, ownerID, strings.TrimSpace(in.PlanID))
	if err != nil {
		_ = m.addRenewalFraudEvent(ctx, ownerID, acc.Username, "invalid_plan", r, map[string]any{"plan_id": strings.TrimSpace(in.PlanID), "error": err.Error()})
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if fraudReason := m.validateRenewalPlanIntegrity(ctx, *acc, ownerID, plan); fraudReason != "" {
		_ = m.addRenewalFraudEvent(ctx, ownerID, acc.Username, fraudReason, r, map[string]any{"plan_id": plan.ID, "months": plan.Months, "days": plan.Days, "price": plan.Price})
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "plano inválido para esta conta"})
		return
	}
	if recent, err := m.hasRecentRenewalOrder(ctx, acc.Username, plan, 90*time.Second); err == nil && recent {
		_ = m.addRenewalFraudEvent(ctx, ownerID, acc.Username, "duplicate_payment_attempt", r, map[string]any{"plan_id": plan.ID, "window_seconds": 90})
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "message": "já existe uma tentativa recente para este plano. aguarde alguns segundos"})
		return
	}
	if plan.Price <= 0 {
		order, err := m.createFreeRenewalOrder(ctx, ownerID, cfg.Bank, *acc, plan)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		updated, _ := m.Store.FindAccount(ctx, acc.Username)
		accountPayload := map[string]any{"username": acc.Username, "displayName": acc.Username, "status": "Ativa", "monthlyValue": round2(acc.MonthlyValue), "monthly_value": round2(acc.MonthlyValue)}
		if updated != nil {
			accountPayload["expiresAt"] = updated.ExpiresAt.UTC().Format(time.RFC3339)
			accountPayload["expiryDate"] = updated.ExpiryDate
			accountPayload["status"] = renewalAccountStatus(*updated)
			accountPayload["limit"] = updated.LimitConnections
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"paymentId":   order.OrderID,
			"orderId":     order.OrderID,
			"copyPaste":   "",
			"paymentUrl":  "",
			"status":      "approved",
			"applied":     order.AppliedAt != nil,
			"amount":      0,
			"bank":        order.Bank,
			"plan":        plan,
			"account":     accountPayload,
			"generatedAt": time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	order, err := m.CreateOrder(ctx, OrderInput{OwnerID: ownerID, TargetResellerID: 0, Kind: KindAccountRenew, Months: plan.Months, Days: plan.Days, Amount: plan.Price, Bank: cfg.Bank, Description: fmt.Sprintf("Renovação %s - %s", plan.Name, acc.Username)})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(order.PayloadJSON), &payload)
	if payload == nil {
		payload = map[string]any{}
	}
	payload["username"] = acc.Username
	payload["source"] = "renewal_api"
	payload["plan_id"] = plan.ID
	payload["plan_name"] = plan.Name
	payload["days"] = plan.Days
	payload["months"] = plan.Months
	payload["expected_amount"] = plan.Price
	payload["account_monthly_value"] = round2(acc.MonthlyValue)
	payload["client_ip"] = clientIP(r)
	payload["user_agent"] = strings.TrimSpace(r.UserAgent())
	payload["anti_fraud"] = true
	b, _ := json.Marshal(payload)
	order.PayloadJSON = string(b)
	_ = m.Store.UpdatePaymentOrder(ctx, *order)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"paymentId":   order.OrderID,
		"orderId":     order.OrderID,
		"copyPaste":   order.PixCopyPaste,
		"paymentUrl":  order.PaymentURL,
		"status":      order.Status,
		"amount":      order.Amount,
		"bank":        order.Bank,
		"plan":        plan,
		"account":     acc.Username,
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
	})
}

func (m *Manager) createFreeRenewalOrder(ctx context.Context, ownerID int64, bank string, acc model.Account, plan renewalPlan) (*model.PaymentOrder, error) {
	id := "PC" + time.Now().UTC().Format("20060102150405") + randHex(4)
	desc := fmt.Sprintf("Renovação grátis %s - %s", plan.Name, acc.Username)
	payload := map[string]any{
		"description":            desc,
		"created_by":             "primecel-gestor",
		"username":               acc.Username,
		"source":                 "renewal_api",
		"plan_id":                plan.ID,
		"plan_name":              plan.Name,
		"days":                   plan.Days,
		"months":                 plan.Months,
		"expected_amount":        0,
		"account_monthly_value":  round2(acc.MonthlyValue),
		"free_renewal":           true,
		"free_renewal_validated": plan.Months == 1 && round2(acc.MonthlyValue) <= 0,
		"anti_fraud":             true,
	}
	b, _ := json.Marshal(payload)
	now := time.Now().UTC()
	order := model.PaymentOrder{OrderID: id, OwnerID: ownerID, TargetResellerID: 0, Kind: KindAccountRenew, Months: plan.Months, Days: plan.Days, Amount: 0, Bank: bank, ExternalID: id, Status: "pending", PayloadJSON: string(b), CreatedAt: now}
	if err := m.Store.InsertPaymentOrder(ctx, order); err != nil {
		return nil, err
	}
	_ = m.Store.AddPaymentEvent(ctx, order.OrderID, order.OwnerID, "created_free", string(b))
	paid, err := m.MarkPaidConfirmed(ctx, order.OrderID, []byte(`{"status":"approved","source":"free_renewal","free":true}`), true)
	if err != nil {
		return nil, err
	}
	return paid, nil
}

func (m *Manager) serveRenewalPixStatus(w http.ResponseWriter, r *http.Request) {
	setPublicAPIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "message": "method not allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	sess, ok := m.renewalSessionFromRequest(ctx, r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "sessão inválida"})
		return
	}
	paymentID := strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("paymentId"), r.URL.Query().Get("order_id"), r.URL.Query().Get("orderId"), r.URL.Query().Get("order_nsu")))
	transactionNSU := strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("transaction_nsu"), r.URL.Query().Get("transactionNsu")))
	slug := strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("slug"), r.URL.Query().Get("invoice_slug"), r.URL.Query().Get("invoiceSlug")))
	if paymentID == "" && r.Method == http.MethodPost {
		var body map[string]any
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		paymentID = strings.TrimSpace(fmt.Sprint(body["paymentId"]))
		if paymentID == "" || paymentID == "<nil>" {
			paymentID = strings.TrimSpace(fmt.Sprint(body["order_id"]))
		}
		if paymentID == "" || paymentID == "<nil>" {
			paymentID = strings.TrimSpace(fmt.Sprint(body["order_nsu"]))
		}
		transactionNSU = firstNonEmpty(transactionNSU, strings.TrimSpace(fmt.Sprint(body["transaction_nsu"])), strings.TrimSpace(fmt.Sprint(body["transactionNsu"])))
		slug = firstNonEmpty(slug, strings.TrimSpace(fmt.Sprint(body["slug"])), strings.TrimSpace(fmt.Sprint(body["invoice_slug"])))
	}
	if paymentID == "" || paymentID == "<nil>" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "paymentId obrigatório"})
		return
	}
	o, err := m.Store.FindPaymentOrder(ctx, paymentID)
	if err != nil || o == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "pagamento não encontrado"})
		return
	}
	if !m.orderBelongsToSession(*o, sess.Username) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "message": "pagamento não pertence a esta sessão"})
		return
	}
	if o.AppliedAt == nil {
		cfg, _ := m.Store.FindPaymentOwnerConfig(ctx, o.OwnerID)
		statusPayload := []byte(`{"source":"renewal_api_status"}`)
		if o.Bank == BankInfinitePay && transactionNSU != "" && slug != "" {
			statusPayload = []byte(fmt.Sprintf(`{"source":"renewal_api_status","order_nsu":%q,"transaction_nsu":%q,"slug":%q}`, o.OrderID, transactionNSU, slug))
		}
		confirmed, status, raw, err := m.ConfirmPayment(ctx, *o, cfg, statusPayload)
		if err == nil && confirmed {
			if len(raw) == 0 {
				raw = []byte(fmt.Sprintf(`{"status":%q,"source":"renewal_api_status"}`, status))
			}
			if paid, paidErr := m.MarkPaidConfirmed(ctx, o.OrderID, raw, true); paidErr == nil && paid != nil {
				o = paid
			}
		}
	}
	status := o.Status
	if o.AppliedAt != nil {
		status = "approved"
	}
	response := map[string]any{
		"ok":        true,
		"paymentId": o.OrderID,
		"orderId":   o.OrderID,
		"status":    status,
		"applied":   o.AppliedAt != nil,
		"paidAt":    timePtrRFC3339(o.PaidAt),
		"appliedAt": timePtrRFC3339(o.AppliedAt),
	}
	if o.AppliedAt != nil {
		if acc, accErr := m.Store.FindAccount(ctx, sess.Username); accErr == nil && acc != nil {
			response["account"] = map[string]any{
				"username":      acc.Username,
				"displayName":   acc.Username,
				"expiresAt":     acc.ExpiresAt.UTC().Format(time.RFC3339),
				"expiryDate":    acc.ExpiryDate,
				"status":        renewalAccountStatus(*acc),
				"limit":         acc.LimitConnections,
				"monthlyValue":  round2(acc.MonthlyValue),
				"monthly_value": round2(acc.MonthlyValue),
			}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (m *Manager) validateRenewalPlanIntegrity(ctx context.Context, acc model.Account, ownerID int64, plan renewalPlan) string {
	months := plan.Months
	if months <= 0 && plan.Days > 0 && plan.Days%30 == 0 {
		months = plan.Days / 30
	}
	if months != 1 && months != 2 && months != 3 {
		return "invalid_months"
	}
	if plan.Days <= 0 {
		return "invalid_days"
	}
	if months == 1 {
		expected := round2(acc.MonthlyValue)
		if expected < 0 {
			expected = 0
		}
		if round2(plan.Price) != expected {
			return "monthly_price_mismatch"
		}
		return ""
	}
	packages, err := m.Store.ListPaymentPackages(ctx, ownerID, true)
	if err != nil {
		return "package_lookup_failed"
	}
	for _, p := range packages {
		if p.Kind != KindRenew {
			continue
		}
		days := p.Days
		if days == 0 && p.Months > 0 {
			days = p.Months * 30
		}
		pm := p.Months
		if pm <= 0 && days%30 == 0 {
			pm = days / 30
		}
		if pm == months && p.ID == plan.PackageID && round2(p.Amount) == round2(plan.Price) && days == plan.Days {
			return ""
		}
	}
	return "package_price_mismatch"
}

func (m *Manager) hasRecentRenewalOrder(ctx context.Context, username string, plan renewalPlan, window time.Duration) (bool, error) {
	orders, err := m.Store.ListPaymentOrders(ctx, -1, "")
	if err != nil {
		return false, err
	}
	cutoff := time.Now().UTC().Add(-window)
	for _, o := range orders {
		if o.Kind != KindAccountRenew || o.AppliedAt != nil || o.CreatedAt.Before(cutoff) {
			continue
		}
		if o.Status == "approved" || o.Status == "paid" || o.Status == "cancelled" || o.Status == "expired" {
			continue
		}
		var payload map[string]any
		_ = json.Unmarshal([]byte(o.PayloadJSON), &payload)
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(payload["username"])), username) {
			continue
		}
		if strings.TrimSpace(fmt.Sprint(payload["plan_id"])) == plan.ID || (anyInt(payload["months"]) == plan.Months && anyInt(payload["days"]) == plan.Days) {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) addRenewalFraudEvent(ctx context.Context, ownerID int64, username, reason string, r *http.Request, extra map[string]any) error {
	if extra == nil {
		extra = map[string]any{}
	}
	extra["username"] = username
	extra["reason"] = reason
	extra["client_ip"] = clientIP(r)
	extra["user_agent"] = strings.TrimSpace(r.UserAgent())
	extra["path"] = r.URL.Path
	b, _ := json.Marshal(extra)
	return m.Store.AddPaymentEvent(ctx, "renewal-antifraud", ownerID, "anti_fraud_"+reason, string(b))
}

func anyFloat(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(v), ",", "."), 64)
		return f
	default:
		return -1
	}
}

func anyInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(v))
		return i
	default:
		return 0
	}
}

func (m *Manager) renewalOwnerForAccount(ctx context.Context, a model.Account) (int64, string) {
	ownerID := a.OwnerTelegramID
	ownerType := strings.ToLower(strings.TrimSpace(a.OwnerType))
	if ownerID == 0 || ownerType == "admin" {
		name := strings.TrimSpace(a.OwnerName)
		if name == "" {
			name = "Admin"
		}
		return 0, name
	}
	if r, _ := m.Store.FindReseller(ctx, ownerID); r != nil {
		return ownerID, firstNonEmpty(r.Name, a.OwnerName, fmt.Sprint(ownerID))
	}
	return ownerID, firstNonEmpty(a.OwnerName, fmt.Sprint(ownerID))
}

func (m *Manager) renewalPlansForAccount(ctx context.Context, a model.Account, ownerID int64) ([]renewalPlan, error) {
	packages, err := m.Store.ListPaymentPackages(ctx, ownerID, true)
	if err != nil {
		return nil, err
	}
	out := make([]renewalPlan, 0, 3)
	seen := map[string]int{}
	addPlan := func(plan renewalPlan) {
		if plan.Days <= 0 || plan.Price < 0 {
			return
		}
		key := ""
		if plan.Months > 0 {
			key = fmt.Sprintf("m:%d", plan.Months)
		} else {
			key = fmt.Sprintf("d:%d", plan.Days)
		}
		if idx, ok := seen[key]; ok {
			// Duplicatas de mesmo período podem surgir quando o vendedor salva o mesmo plano mais de uma vez.
			// Mantém apenas um card; se houver valores diferentes, usa o menor valor para evitar cobrar mais caro.
			if plan.Price < out[idx].Price || (plan.Price == out[idx].Price && plan.PackageID > out[idx].PackageID) {
				out[idx] = plan
			}
			return
		}
		seen[key] = len(out)
		out = append(out, plan)
	}

	// Regra da renovação web:
	// 1 mês sempre usa o valor salvo na própria conta, inclusive R$ 0,00.
	// 2 e 3 meses usam somente os valores configurados nos pacotes de renovação do vendedor/admin.
	monthly := round2(a.MonthlyValue)
	if monthly < 0 {
		monthly = 0
	}
	addPlan(renewalPlan{ID: "m:1", Name: renewalPlanName(1, 30, ""), Days: 30, Months: 1, Price: monthly, Amount: monthly, Description: renewalPlanDescription(1, 30)})

	for _, p := range packages {
		if p.Kind != KindRenew {
			continue
		}
		days := p.Days
		if days == 0 && p.Months > 0 {
			days = p.Months * 30
		}
		if days <= 0 || p.Amount < 0 {
			continue
		}
		months := p.Months
		if months <= 0 && days%30 == 0 {
			months = days / 30
		}
		if months == 1 {
			// O plano de 1 mês é sempre o valor da conta; pacote de 1 mês não sobrescreve.
			continue
		}
		if months != 2 && months != 3 {
			continue
		}
		addPlan(renewalPlan{ID: fmt.Sprintf("pkg:%d", p.ID), Name: renewalPlanName(months, days, p.Name), Days: days, Months: months, Price: round2(p.Amount), Amount: round2(p.Amount), Description: renewalPlanDescription(months, days), Popular: months == 2, PackageID: p.ID})
	}
	sort.SliceStable(out, func(i, j int) bool {
		mi, mj := out[i].Months, out[j].Months
		if mi <= 0 {
			mi = (out[i].Days + 29) / 30
		}
		if mj <= 0 {
			mj = (out[j].Days + 29) / 30
		}
		if mi != mj {
			return mi < mj
		}
		if out[i].Days != out[j].Days {
			return out[i].Days < out[j].Days
		}
		return out[i].Price < out[j].Price
	})
	popularSet := false
	for i := range out {
		out[i].Popular = false
		if !popularSet && out[i].Months == 2 {
			out[i].Popular = true
			popularSet = true
		}
	}
	return out, nil
}

func (m *Manager) resolveRenewalPlan(ctx context.Context, a model.Account, ownerID int64, planID string) (renewalPlan, error) {
	plans, err := m.renewalPlansForAccount(ctx, a, ownerID)
	if err != nil {
		return renewalPlan{}, err
	}
	for _, p := range plans {
		if p.ID == planID || fmt.Sprint(p.PackageID) == planID {
			return p, nil
		}
	}
	return renewalPlan{}, errors.New("plano inválido")
}

func (m *Manager) renewalMonthlyAmount(ctx context.Context, a model.Account) float64 {
	if a.MonthlyValue > 0 {
		return a.MonthlyValue
	}
	if a.OwnerTelegramID != 0 {
		if r, _ := m.Store.FindReseller(ctx, a.OwnerTelegramID); r != nil && r.MonthlyPrice > 0 {
			return r.MonthlyPrice
		}
	}
	return 0
}

func (m *Manager) createRenewalSession(ctx context.Context, username string) (string, error) {
	token := "ren_" + randHex(24)
	now := time.Now().UTC()
	expires := now.Add(7 * 24 * time.Hour)
	_ = m.Store.Exec(ctx, `DELETE FROM renewal_sessions WHERE expires_at<=?`, now.Format(time.RFC3339))
	if err := m.Store.Exec(ctx, `INSERT INTO renewal_sessions(token,username,expires_at,created_at) VALUES(?,?,?,?)`, token, username, expires.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		return "", err
	}
	return token, nil
}

func (m *Manager) renewalSessionFromRequest(ctx context.Context, r *http.Request) (renewalSession, bool) {
	token := strings.TrimSpace(r.Header.Get("Authorization"))
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer"))
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if token == "" {
		return renewalSession{}, false
	}
	rows, err := m.Store.Query(ctx, `SELECT token,username,expires_at FROM renewal_sessions WHERE token=? LIMIT 1`, token)
	if err != nil || len(rows) == 0 {
		return renewalSession{}, false
	}
	expires := parseRenewalTime(rows[0]["expires_at"])
	if expires.IsZero() || !expires.After(time.Now().UTC()) {
		_ = m.Store.Exec(ctx, `DELETE FROM renewal_sessions WHERE token=?`, token)
		return renewalSession{}, false
	}
	return renewalSession{Token: rows[0]["token"], Username: rows[0]["username"], ExpiresAt: expires}, true
}

func (m *Manager) orderBelongsToSession(o model.PaymentOrder, username string) bool {
	var data map[string]any
	_ = json.Unmarshal([]byte(o.PayloadJSON), &data)
	return strings.EqualFold(strings.TrimSpace(fmt.Sprint(data["username"])), username)
}

func renewalAccountStatus(a model.Account) string {
	if strings.EqualFold(a.Status, "blocked") {
		return "Bloqueada"
	}
	if !a.ExpiresAt.After(time.Now().UTC()) {
		return "Expirada"
	}
	return "Ativa"
}

func renewalPlanDescription(months, days int) string {
	if months > 0 {
		return fmt.Sprintf("Renovação por %d mês%s", months, pluralS(months))
	}
	return fmt.Sprintf("Renovação por %d dias", days)
}

func renewalPlanName(months, days int, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if months == 1 {
		return "Mensal"
	}
	if months > 1 {
		return fmt.Sprintf("%d meses", months)
	}
	if fallback != "" {
		return fallback
	}
	return fmt.Sprintf("%d dias", days)
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

func parseRenewalTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
		return t
	}
	return time.Time{}
}

func timePtrRFC3339(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (m *Manager) servePublicPlans(w http.ResponseWriter, r *http.Request) {
	setPublicAPIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ownerID := int64(0)
	if raw := strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("owner_id"), r.URL.Query().Get("owner"))); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "owner_id inválido"})
			return
		}
		ownerID = v
	}
	cfg, _ := m.Store.FindPaymentOwnerConfig(ctx, ownerID)
	packages, err := m.Store.ListPaymentPackages(ctx, ownerID, true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	monthPlans := make([]map[string]any, 0, 3)
	limitPackages := make([]map[string]any, 0)
	for _, wantMonth := range []int{1, 2, 3} {
		for _, p := range packages {
			if p.Kind == KindRenew && p.Months == wantMonth {
				monthPlans = append(monthPlans, publicPackageMap(p, "month"))
			}
		}
	}
	for _, p := range packages {
		if p.Kind == KindRenew && (p.Months == 1 || p.Months == 2 || p.Months == 3) {
			continue
		}
		if p.Kind == KindRenew {
			monthPlans = append(monthPlans, publicPackageMap(p, "month"))
			continue
		}
		if p.Kind == KindLimit || p.Kind == KindRenewLimit {
			limitPackages = append(limitPackages, publicPackageMap(p, "package"))
		}
	}
	bank := ""
	enabled := false
	updatedAt := ""
	if cfg != nil {
		bank = cfg.Bank
		enabled = cfg.Enabled
		if !cfg.UpdatedAt.IsZero() {
			updatedAt = cfg.UpdatedAt.UTC().Format(time.RFC3339)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"service":          "primecel-payments-api",
		"owner_id":         ownerID,
		"currency":         "BRL",
		"payments_enabled": enabled,
		"bank":             bank,
		"bank_name":        publicBankName(bank),
		"months":           monthPlans,
		"plans":            monthPlans,
		"packages":         limitPackages,
		"limits":           limitPackages,
		"updated_at":       updatedAt,
		"generated_at":     time.Now().UTC().Format(time.RFC3339),
	})
}

func setPublicAPIHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Cache-Control", "no-store")
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func publicPackageMap(p model.PaymentPackage, category string) map[string]any {
	label := strings.TrimSpace(p.Name)
	if label == "" && p.Months > 0 {
		label = fmt.Sprintf("%d mês(es)", p.Months)
	}
	return map[string]any{
		"id":               p.ID,
		"owner_id":         p.OwnerID,
		"category":         category,
		"kind":             p.Kind,
		"name":             p.Name,
		"label":            label,
		"months":           p.Months,
		"days":             p.Days,
		"limits":           p.Credits,
		"credits":          p.Credits,
		"amount":           round2(p.Amount),
		"amount_formatted": formatBRL(p.Amount),
		"currency":         "BRL",
		"active":           p.Active,
		"created_at":       p.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":       p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func publicBankName(bank string) string {
	switch normalizeBank(bank) {
	case BankMercadoPago:
		return "Mercado Pago"
	case BankAsaas:
		return "Asaas"
	case BankInfinitePay:
		return "InfinitePay"
	default:
		return ""
	}
}

func formatBRL(v float64) string {
	s := fmt.Sprintf("%.2f", round2(v))
	s = strings.ReplaceAll(s, ".", ",")
	return "R$ " + s
}

func (m *Manager) ConfirmPayment(ctx context.Context, o model.PaymentOrder, cfg *model.PaymentOwnerConfig, payload []byte) (bool, string, []byte, error) {
	if o.AppliedAt != nil {
		return true, "already_applied", payload, nil
	}
	if cfg == nil {
		status := paymentStatusFromPayload(payload)
		if status == "" || status == "test" || status == "pending" {
			return true, firstNonEmpty(status, "test_mode"), payload, nil
		}
		return statusIsApproved(status), status, payload, nil
	}
	needsToken := o.Bank != BankInfinitePay
	if needsToken && (strings.TrimSpace(cfg.Token) == "" || strings.EqualFold(strings.TrimSpace(cfg.Token), "TEST")) {
		status := paymentStatusFromPayload(payload)
		if status == "" || status == "test" || status == "pending" {
			return true, firstNonEmpty(status, "test_mode"), payload, nil
		}
		return statusIsApproved(status), status, payload, nil
	}
	switch o.Bank {
	case BankMercadoPago:
		id := firstNonEmpty(o.ExternalID, findStringFromBytes(payload, "data.id", "id", "payment_id"))
		if id == "" || strings.HasPrefix(id, "PC") {
			status := paymentStatusFromPayload(payload)
			return statusIsApproved(status), firstNonEmpty(status, "unknown"), payload, nil
		}
		raw, err := doJSON(ctx, http.MethodGet, "https://api.mercadopago.com/v1/payments/"+id, cfg.Token, nil, nil)
		if err != nil {
			return false, "confirm_error", raw, err
		}
		status := paymentStatusFromPayload(raw)
		return statusIsApproved(status), status, raw, nil
	case BankAsaas:
		id := firstNonEmpty(o.ExternalID, findStringFromBytes(payload, "payment.id", "id", "payment_id"))
		if id == "" || strings.HasPrefix(id, "PC") {
			status := paymentStatusFromPayload(payload)
			return statusIsApproved(status), firstNonEmpty(status, "unknown"), payload, nil
		}
		raw, err := doJSON(ctx, http.MethodGet, "https://api.asaas.com/v3/payments/"+id, "", map[string]string{"access_token": strings.TrimSpace(cfg.Token), "User-Agent": "primecel-gestor"}, nil)
		if err != nil {
			return false, "confirm_error", raw, err
		}
		status := paymentStatusFromPayload(raw)
		return statusIsApproved(status), status, raw, nil
	case BankInfinitePay:
		confirmed, status, raw, err := m.confirmInfinitePay(ctx, o, cfg, payload)
		return confirmed, status, raw, err
	default:
		status := paymentStatusFromPayload(payload)
		return statusIsApproved(status), firstNonEmpty(status, "unknown"), payload, nil
	}
}

func (m *Manager) confirmInfinitePay(ctx context.Context, o model.PaymentOrder, cfg *model.PaymentOwnerConfig, payload []byte) (bool, string, []byte, error) {
	if infinitePayPayloadLooksPaid(payload, o) {
		return true, "paid", payload, nil
	}

	transactionNSU := findStringFromBytes(payload, "transaction_nsu", "transactionNsu")
	slug := firstNonEmpty(findStringFromBytes(payload, "slug"), findStringFromBytes(payload, "invoice_slug", "invoiceSlug"))
	if transactionNSU == "" || slug == "" || cfg == nil {
		status := paymentStatusFromPayload(payload)
		return statusIsApproved(status), firstNonEmpty(status, "pending"), payload, nil
	}

	var data ownerPaymentData
	_ = json.Unmarshal([]byte(cfg.DataJSON), &data)
	handle := strings.TrimPrefix(strings.TrimSpace(firstNonEmpty(data.InfinitePayHandle, data.InfinitePayTag, cfg.Token)), "$")
	if handle == "" {
		return false, "missing_handle", payload, errors.New("InfinitePay sem handle para confirmar pagamento")
	}

	body := map[string]any{
		"handle":          handle,
		"order_nsu":       o.OrderID,
		"transaction_nsu": transactionNSU,
		"slug":            slug,
	}
	raw, err := doJSON(ctx, http.MethodPost, "https://api.checkout.infinitepay.io/payment_check", "", nil, body)
	if err != nil {
		return false, "confirm_error", raw, err
	}
	if infinitePayPayloadLooksPaid(raw, o) {
		return true, "paid", raw, nil
	}
	status := paymentStatusFromPayload(raw)
	return statusIsApproved(status), firstNonEmpty(status, "pending"), raw, nil
}

func infinitePayPayloadLooksPaid(payload []byte, o model.PaymentOrder) bool {
	var obj any
	if json.Unmarshal(payload, &obj) != nil {
		return false
	}
	orderNSU := firstNonEmpty(findString(obj, "order_nsu", "orderNsu", "order_id", "external_reference", "reference"), o.OrderID)
	if orderNSU != "" && o.OrderID != "" && orderNSU != o.OrderID {
		return false
	}
	if strings.EqualFold(findString(obj, "success"), "true") && strings.EqualFold(findString(obj, "paid"), "true") {
		return true
	}
	if strings.EqualFold(findString(obj, "paid"), "true") {
		return true
	}
	if findString(obj, "transaction_nsu", "transactionNsu") != "" && (findString(obj, "paid_amount", "paidAmount") != "" || findString(obj, "amount") != "") {
		return true
	}
	status := paymentStatusFromPayload(payload)
	return statusIsApproved(status)
}

func validateWebhookAuth(o model.PaymentOrder, cfg *model.PaymentOwnerConfig, r *http.Request, body []byte) error {
	if cfg == nil {
		return nil
	}
	var data map[string]any
	_ = json.Unmarshal([]byte(cfg.DataJSON), &data)
	required := strings.EqualFold(fmt.Sprint(data["webhook_require_auth"]), "true") || fmt.Sprint(data["webhook_require_auth"]) == "1"
	secret := firstNonEmpty(fmt.Sprint(data["webhook_secret"]), fmt.Sprint(data["asaas_webhook_token"]), fmt.Sprint(data["mp_webhook_secret"]), fmt.Sprint(data["infinitepay_webhook_secret"]))
	if secret == "<nil>" {
		secret = ""
	}
	switch o.Bank {
	case BankAsaas:
		if secret == "" {
			if required {
				return errors.New("token de webhook Asaas não configurado")
			}
			return nil
		}
		got := firstNonEmpty(r.Header.Get("asaas-access-token"), r.Header.Get("access_token"), r.Header.Get("x-webhook-token"), r.Header.Get("x-asaas-token"))
		if !constantEqual(got, secret) {
			return errors.New("webhook Asaas com token inválido")
		}
	case BankMercadoPago:
		if secret == "" {
			if required {
				return errors.New("segredo de webhook Mercado Pago não configurado")
			}
			return nil
		}
		if !verifyMercadoPagoSignature(r, body, secret) {
			return errors.New("assinatura Mercado Pago inválida")
		}
	case BankInfinitePay:
		if secret == "" {
			if required {
				return errors.New("segredo de webhook InfinitePay não configurado")
			}
			return nil
		}
		got := firstNonEmpty(r.Header.Get("x-infinitepay-signature"), r.Header.Get("x-webhook-signature"), r.Header.Get("x-signature"))
		if !verifyHMACHeader(got, body, secret) && !constantEqual(got, secret) {
			return errors.New("assinatura InfinitePay inválida")
		}
	}
	return nil
}

func verifyMercadoPagoSignature(r *http.Request, body []byte, secret string) bool {
	sig := r.Header.Get("x-signature")
	if sig == "" {
		return false
	}
	ts := signaturePart(sig, "ts")
	v1 := signaturePart(sig, "v1")
	if ts == "" || v1 == "" {
		return verifyHMACHeader(sig, body, secret)
	}
	requestID := r.Header.Get("x-request-id")
	dataID := firstNonEmpty(r.URL.Query().Get("data.id"), r.URL.Query().Get("id"), findStringFromBytes(body, "data.id", "id"))
	manifest := fmt.Sprintf("id:%s;request-id:%s;ts:%s;", dataID, requestID, ts)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(manifest))
	expected := hex.EncodeToString(mac.Sum(nil))
	return constantEqual(v1, expected)
}

func signaturePart(sig, key string) string {
	for _, part := range strings.Split(sig, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, key+"=") {
			return strings.TrimPrefix(part, key+"=")
		}
	}
	return ""
}

func verifyHMACHeader(header string, body []byte, secret string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	header = strings.TrimPrefix(header, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return constantEqual(header, expected)
}

func constantEqual(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return hmac.Equal([]byte(a), []byte(b))
}

func webhookEventID(r *http.Request, body []byte, orderID string) string {
	if v := firstNonEmpty(r.Header.Get("x-request-id"), r.Header.Get("x-event-id"), r.Header.Get("x-webhook-id"), r.URL.Query().Get("event_id")); v != "" {
		return v
	}
	if v := findStringFromBytes(body, "event_id", "eventId", "notification_id", "id"); v != "" && !strings.EqualFold(v, orderID) {
		return v
	}
	mac := sha256.Sum256(append([]byte(orderID+":"), body...))
	return "body-" + hex.EncodeToString(mac[:12])
}

func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func headersSummaryJSON(r *http.Request) string {
	allowed := []string{"user-agent", "x-request-id", "x-event-id", "x-webhook-id", "x-signature", "asaas-access-token", "x-real-ip", "x-forwarded-for"}
	m := map[string]string{}
	for _, k := range allowed {
		if v := r.Header.Get(k); v != "" {
			m[k] = maskSecret(v)
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "***" + s[len(s)-4:]
}

func compactBodyJSON(body []byte) string {
	if len(body) == 0 {
		return "{}"
	}
	var obj any
	if json.Unmarshal(body, &obj) == nil {
		b, _ := json.Marshal(obj)
		if len(b) > 8000 {
			b = b[:8000]
		}
		return string(b)
	}
	s := string(body)
	if len(s) > 8000 {
		s = s[:8000]
	}
	b, _ := json.Marshal(map[string]string{"raw": s})
	return string(b)
}

func paymentStatusFromPayload(payload []byte) string {
	var obj any
	if json.Unmarshal(payload, &obj) != nil {
		return ""
	}
	if strings.EqualFold(findString(obj, "paid"), "true") {
		return "paid"
	}
	if strings.EqualFold(findString(obj, "success"), "true") && strings.EqualFold(findString(obj, "paid"), "true") {
		return "paid"
	}
	return strings.ToLower(firstNonEmpty(findString(obj, "status", "payment.status", "event", "type"), findString(obj, "payment_status", "status_detail")))
}

func payloadLooksPaid(payload []byte) bool {
	return statusIsApproved(paymentStatusFromPayload(payload))
}

func statusIsApproved(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "approved", "accredited", "paid", "confirmed", "received", "received_in_cash", "payment_received", "payment_confirmed", "completed", "settled", "concluded", "approved_applied":
		return true
	default:
		return strings.Contains(status, "approved") || strings.Contains(status, "paid") || strings.Contains(status, "confirmed") || strings.Contains(status, "received")
	}
}

func findStringFromBytes(payload []byte, keys ...string) string {
	var obj any
	if json.Unmarshal(payload, &obj) != nil {
		return ""
	}
	// allow dotted leaf names by also searching the last segment
	all := append([]string{}, keys...)
	for _, k := range keys {
		if strings.Contains(k, ".") {
			parts := strings.Split(k, ".")
			all = append(all, parts[len(parts)-1])
		}
	}
	return findString(obj, all...)
}

func normalizeBank(s string) string {
	s = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, "-", "_")))
	switch s {
	case "mercadopago", "mercado_pago", "mp":
		return BankMercadoPago
	case "asaas":
		return BankAsaas
	case "infinitepay", "infinite_pay":
		return BankInfinitePay
	default:
		return ""
	}
}
func normalizeKind(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case KindRenew, KindLimit, KindRenewLimit, KindAccountRenew:
		return s
	default:
		return ""
	}
}
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%04d", time.Now().UnixNano()%10000)
	}
	return hex.EncodeToString(b)
}
func buildPaymentPlaceholder(o model.PaymentOrder) (string, string) {
	switch o.Bank {
	case BankInfinitePay:
		return "", "https://checkout.infinitepay.io/" + o.OrderID
	case BankAsaas:
		return "PIX-ASAAS-" + o.OrderID, ""
	default:
		return "PIX-MERCADOPAGO-" + o.OrderID, ""
	}
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func findString(v any, keys ...string) string {
	want := map[string]bool{}
	for _, k := range keys {
		want[strings.ToLower(k)] = true
	}
	var walk func(any) string
	walk = func(x any) string {
		switch t := x.(type) {
		case map[string]any:
			for k, v := range t {
				if want[strings.ToLower(k)] {
					if s := fmt.Sprint(v); strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
				if s := walk(v); s != "" {
					return s
				}
			}
		case []any:
			for _, v := range t {
				if s := walk(v); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return walk(v)
}
