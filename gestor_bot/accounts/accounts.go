package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/mirrors"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/resellers"
	"primecel-gestor/gestor_bot/store"
	"primecel-gestor/gestor_bot/system"
	"primecel-gestor/gestor_bot/xray"
)

type ServiceDeps struct {
	Store     *store.DB
	System    system.Manager
	Mirrors   *mirrors.Writer
	Resellers *resellers.Service
	Xray      *xray.Manager
	XrayOpts  xray.ApplyOptions
	Config    config.Config
}
type Service struct {
	st       *store.DB
	sys      system.Manager
	mirrors  *mirrors.Writer
	res      *resellers.Service
	xray     *xray.Manager
	xrayOpts xray.ApplyOptions
	cfg      config.Config
}

func NewService(d ServiceDeps) *Service {
	return &Service{st: d.Store, sys: d.System, mirrors: d.Mirrors, res: d.Resellers, xray: d.Xray, xrayOpts: d.XrayOpts, cfg: d.Config}
}

type CreateDraft struct {
	Username, Password, UUID, ClientWhatsApp string
	MonthlyValue                             float64
	Days, Limit, TrialHours                  int
	IsTrial                                  bool
	// XrayEnabled define se o UUID deve ficar ativo/aplicado no Xray.
	// UUID vazio = sem Xray; UUID preenchido com XrayEnabled=false = Xray oculto/inativo.
	XrayEnabled bool
}

var validUser = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]{3,11}$`)

func NormalizeUsername(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}
func ValidateUsername(s string) error {
	if !validUser.MatchString(s) {
		return errors.New("usuário inválido")
	}
	return nil
}
func ValidatePassword(s string) error {
	if s == "" {
		return errors.New("senha vazia")
	}
	if strings.ContainsAny(s, " \n\r:") {
		return errors.New("senha inválida")
	}
	return nil
}

func (s *Service) ResolveAvailableUsername(ctx context.Context, raw string) (string, error) {
	base := NormalizeUsername(raw)
	if err := ValidateUsername(base); err != nil {
		return "", err
	}
	if old, _ := s.st.FindAccount(ctx, base); old != nil {
		return "", errors.New("usuário já existe")
	}
	return base, nil
}

func (s *Service) Create(ctx context.Context, actor model.Actor, d CreateDraft) (*model.Account, error) {
	username, err := s.ResolveAvailableUsername(ctx, d.Username)
	if err != nil {
		return nil, err
	}
	d.Username = username
	if err := ValidatePassword(d.Password); err != nil {
		return nil, err
	}
	if d.Days <= 0 && (!d.IsTrial || d.TrialHours <= 0) {
		return nil, errors.New("validade inválida")
	}
	if d.Limit <= 0 {
		d.Limit = 1
	}
	wantsXray := d.UUID != ""
	activeXray := wantsXray && d.XrayEnabled
	validateDays := d.Days
	if d.IsTrial && validateDays <= 0 {
		validateDays = 1
	}
	if s.res != nil {
		if err := s.res.ValidateCanCreateAccount(ctx, actor, validateDays, d.Limit, activeXray); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	expiresAt := now.AddDate(0, 0, d.Days)
	if d.IsTrial && d.TrialHours > 0 {
		expiresAt = now.Add(time.Duration(d.TrialHours) * time.Hour)
	}
	expiryDate := expiresAt.Format("2006-01-02")
	if !d.IsTrial {
		expiresAt = time.Date(expiresAt.Year(), expiresAt.Month(), expiresAt.Day()+1, 0, 0, 0, 0, time.Local).UTC()
	}
	acc := model.Account{Username: d.Username, Password: d.Password, UUID: d.UUID, LimitConnections: d.Limit, ExpiresAt: expiresAt, ExpiryDate: expiryDate, OwnerTelegramID: actor.TelegramID, OwnerName: actor.Name, OwnerType: string(actor.Role), Status: "active", IsTrial: d.IsTrial, XrayEnabled: activeXray, CreditCounted: !actor.IsAdmin && actor.Role != model.RoleAdmin, ClientWhatsApp: digits(d.ClientWhatsApp), MonthlyValue: d.MonthlyValue, CreatedAt: now, UpdatedAt: now}
	spent := false
	if acc.CreditCounted && s.res != nil {
		if err := s.res.SpendForAccount(ctx, actor, acc.Username); err != nil {
			return nil, err
		}
		spent = true
	}
	if err := s.st.UpsertAccount(ctx, acc); err != nil {
		if spent {
			_ = s.res.RefundForAccount(ctx, actor, acc.Username)
		}
		return nil, err
	}
	if err := s.st.UpsertDeviceUser(ctx, acc.Username, acc.UUID, acc.LimitConnections); err != nil {
		return nil, err
	}
	if err := s.sys.ApplyAccount(ctx, acc); err != nil {
		if spent {
			_ = s.res.RefundForAccount(ctx, actor, acc.Username)
		}
		_ = s.st.MarkAccountDeleted(ctx, acc.Username)
		return nil, err
	}
	if acc.XrayEnabled && acc.UUID != "" && s.xray != nil {
		if _, err := s.xray.ApplyAccount(ctx, acc, s.xrayOpts); err != nil {
			if spent {
				_ = s.res.RefundForAccount(ctx, actor, acc.Username)
			}
			_ = s.sys.RemoveAccount(ctx, acc.Username)
			_ = s.st.MarkAccountDeleted(ctx, acc.Username)
			return nil, err
		}
	}
	s.addEvent(ctx, acc.Username, "create", acc, actor.TelegramID)
	_ = s.mirrors.UpsertAccount(ctx, acc)
	return &acc, nil
}
func (s *Service) Renew(ctx context.Context, actor model.Actor, username string, days int) (*model.Account, error) {
	if days <= 0 {
		return nil, errors.New("dias inválidos")
	}
	acc, err := s.st.FindAccount(ctx, username)
	if err != nil || acc == nil {
		return nil, errors.New("conta não encontrada")
	}
	base := time.Now().UTC()
	if acc.ExpiresAt.After(base) {
		base = acc.ExpiresAt
	}
	acc.ExpiresAt = base.AddDate(0, 0, days)
	acc.ExpiryDate = acc.ExpiresAt.AddDate(0, 0, -1).Format("2006-01-02")
	// Renovou teste: passa a ser conta normal.
	// A criação do teste continua vencendo por hora; somente a renovação converte.
	acc.IsTrial = false
	acc.Status = "active"
	acc.UpdatedAt = time.Now().UTC()
	if err := s.st.UpsertAccount(ctx, *acc); err != nil {
		return nil, err
	}
	if err := s.st.UpsertDeviceUser(ctx, acc.Username, acc.UUID, acc.LimitConnections); err != nil {
		return nil, err
	}
	if err := s.sys.ApplyAccount(ctx, *acc); err != nil {
		return nil, err
	}
	if acc.XrayEnabled && acc.UUID != "" && s.xray != nil {
		if _, err := s.xray.ApplyAccount(ctx, *acc, s.xrayOpts); err != nil {
			return nil, err
		}
	}
	s.addEvent(ctx, acc.Username, "renew", acc, actor.TelegramID)
	_ = s.mirrors.UpsertAccount(ctx, *acc)
	return acc, nil
}

func (s *Service) TemporaryRelease(ctx context.Context, actor model.Actor, username string, dur time.Duration) (*model.Account, error) {
	if dur <= 0 {
		dur = 15 * time.Minute
	}
	acc, err := s.st.FindAccount(ctx, username)
	if err != nil || acc == nil {
		return nil, errors.New("conta não encontrada")
	}
	now := time.Now().UTC()
	acc.ExpiresAt = now.Add(dur)
	acc.ExpiryDate = acc.ExpiresAt.Format("2006-01-02")
	acc.Status = "active"
	acc.UpdatedAt = now
	if err := s.st.UpsertAccount(ctx, *acc); err != nil {
		return nil, err
	}
	if err := s.st.UpsertDeviceUser(ctx, acc.Username, acc.UUID, acc.LimitConnections); err != nil {
		return nil, err
	}
	if err := s.sys.ApplyAccount(ctx, *acc); err != nil {
		return nil, err
	}
	if acc.XrayEnabled && acc.UUID != "" && s.xray != nil {
		if _, err := s.xray.ApplyAccount(ctx, *acc, s.xrayOpts); err != nil {
			return nil, err
		}
	}
	s.addEvent(ctx, acc.Username, "temporary_release", map[string]any{"username": acc.Username, "minutes": int(dur.Minutes()), "expires_at": acc.ExpiresAt}, actor.TelegramID)
	_ = s.mirrors.UpsertAccount(ctx, *acc)
	return acc, nil
}

func (s *Service) ChangePassword(ctx context.Context, actor model.Actor, username, newPass string) (*model.Account, error) {
	if err := ValidatePassword(newPass); err != nil {
		return nil, err
	}
	acc, err := s.st.FindAccount(ctx, username)
	if err != nil || acc == nil {
		return nil, errors.New("conta não encontrada")
	}
	acc.Password = newPass
	acc.UpdatedAt = time.Now().UTC()
	if err := s.st.UpsertAccount(ctx, *acc); err != nil {
		return nil, err
	}
	if err := s.sys.ChangePassword(ctx, acc.Username, newPass); err != nil {
		return nil, err
	}
	if err := s.sys.UpsertUsuariosDB(ctx, *acc); err != nil {
		return nil, err
	}
	s.addEvent(ctx, acc.Username, "password_update", acc, actor.TelegramID)
	_ = s.mirrors.UpsertAccount(ctx, *acc)
	return acc, nil
}
func (s *Service) ChangeLimit(ctx context.Context, actor model.Actor, username string, limit int) (*model.Account, error) {
	if limit <= 0 {
		return nil, errors.New("limite inválido")
	}
	acc, err := s.st.FindAccount(ctx, username)
	if err != nil || acc == nil {
		return nil, errors.New("conta não encontrada")
	}
	acc.LimitConnections = limit
	acc.UpdatedAt = time.Now().UTC()
	if err := s.st.UpsertAccount(ctx, *acc); err != nil {
		return nil, err
	}
	if err := s.st.UpsertDeviceUser(ctx, acc.Username, acc.UUID, acc.LimitConnections); err != nil {
		return nil, err
	}
	if err := s.sys.UpsertUsuariosDB(ctx, *acc); err != nil {
		return nil, err
	}
	s.addEvent(ctx, acc.Username, "limit_update", acc, actor.TelegramID)
	_ = s.mirrors.UpsertAccount(ctx, *acc)
	return acc, nil
}
func (s *Service) Remove(ctx context.Context, actor model.Actor, username string) error {
	acc, err := s.st.FindAccount(ctx, username)
	if err != nil || acc == nil {
		return errors.New("conta não encontrada")
	}
	if err := s.sys.RemoveAccount(ctx, acc.Username); err != nil {
		return err
	}
	if acc.XrayEnabled && acc.UUID != "" && s.xray != nil {
		if err := s.xray.RemoveAccount(ctx, acc.Username, acc.UUID, s.xrayOpts); err != nil {
			return err
		}
	}
	if err := s.st.ClearDevicesForUser(ctx, acc.Username, true); err != nil {
		return err
	}
	if err := s.st.MarkAccountDeleted(ctx, acc.Username); err != nil {
		return err
	}
	s.addEvent(ctx, acc.Username, "delete", map[string]any{"username": acc.Username}, actor.TelegramID)
	if acc.CreditCounted && s.res != nil {
		_ = s.res.RefundForAccount(ctx, actor, acc.Username)
	}
	_ = s.mirrors.RemoveAccount(ctx, acc.Username)
	return nil
}

// PruneRemoteToSnapshot força uma VPS secundária a refletir o snapshot do servidor principal.
// Remove sobras do SQLite local, usuários Linux, usuarios.db, CheckUser e Xray que não
// existem mais no painel principal. desiredUsers representa contas SSH/usuarios.db ativas;
// desiredXrayUsers representa contas que devem permanecer aplicadas no Xray.
func (s *Service) PruneRemoteToSnapshot(ctx context.Context, desiredUsers, desiredXrayUsers map[string]bool) (int, int, []string) {
	desiredUsers = normalizeDesiredMap(desiredUsers)
	desiredXrayUsers = normalizeDesiredMap(desiredXrayUsers)
	if desiredXrayUsers == nil {
		desiredXrayUsers = map[string]bool{}
	}
	var details []string
	removed, failed := 0, 0
	admin := model.Actor{Role: model.RoleAdmin, IsAdmin: true, Name: "Sync"}

	local, err := s.st.ListAccounts(ctx, false)
	if err != nil {
		details = append(details, "reconciliação sqlite: "+err.Error())
		failed++
	} else {
		for _, acc := range local {
			if acc.DeletedAt != nil || acc.Status == "deleted" {
				continue
			}
			key := accountKey(acc.Username)
			if key == "" || desiredUsers[key] {
				continue
			}
			if err := s.Remove(ctx, admin, acc.Username); err != nil {
				failed++
				details = append(details, acc.Username+": remover ausente no principal: "+err.Error())
				continue
			}
			removed++
		}
	}

	// Limpa sobras legadas que podem existir apenas no usuarios.db/Linux, sem registro no SQLite.
	if entries, err := system.ReadUsuariosDB(s.cfg.UsuariosDBPath); err == nil {
		seenLegacy := map[string]bool{}
		for _, e := range entries {
			key := accountKey(e.Username)
			if key == "" || seenLegacy[key] || desiredUsers[key] {
				continue
			}
			seenLegacy[key] = true
			if !syncManagedUsername(e.Username) {
				continue
			}
			if err := s.sys.RemoveAccount(ctx, e.Username); err != nil {
				failed++
				details = append(details, e.Username+": remover legado ausente: "+err.Error())
				continue
			}
			_ = s.st.ClearDevicesForUser(ctx, e.Username, true)
			_ = s.st.MarkAccountDeleted(ctx, e.Username)
			_ = s.mirrors.RemoveAccount(ctx, e.Username)
			removed++
		}
	}

	if s.xray != nil {
		xRemoved, err := s.xray.PruneClientsNotDesired(ctx, desiredXrayUsers, s.xrayOpts)
		if err != nil {
			failed++
			details = append(details, "xray prune: "+err.Error())
		} else if xRemoved > 0 {
			removed += xRemoved
		}
	}
	return removed, failed, details
}

func normalizeDesiredMap(in map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range in {
		if !v {
			continue
		}
		key := accountKey(k)
		if key != "" {
			out[key] = true
		}
	}
	return out
}

func accountKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func syncManagedUsername(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return validUser.MatchString(NormalizeUsername(s))
}

// ApplyRemote aplica um estado final recebido do agente/snapshot sem consumir limite.
// É usado nas VPS secundárias e em reconciliações idempotentes. A fonte principal
// continua sendo o SQLite; os espelhos e o sistema local são derivados dele.
func (s *Service) ApplyRemote(ctx context.Context, acc model.Account, actor model.Actor) (*model.Account, error) {
	acc.Username = NormalizeUsername(acc.Username)
	if err := ValidateUsername(acc.Username); err != nil {
		return nil, err
	}
	if err := ValidatePassword(acc.Password); err != nil {
		return nil, err
	}
	if acc.LimitConnections <= 0 {
		acc.LimitConnections = 1
	}
	if acc.ExpiresAt.IsZero() {
		if acc.ExpiryDate != "" {
			if t, err := time.Parse("2006-01-02", acc.ExpiryDate); err == nil {
				acc.ExpiresAt = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.Local).UTC()
			}
		}
		if acc.ExpiresAt.IsZero() {
			acc.ExpiresAt = time.Now().UTC().AddDate(0, 0, 30)
		}
	}
	if acc.ExpiryDate == "" {
		if acc.IsTrial {
			acc.ExpiryDate = acc.ExpiresAt.Format("2006-01-02")
		} else {
			acc.ExpiryDate = acc.ExpiresAt.AddDate(0, 0, -1).Format("2006-01-02")
		}
	}
	if acc.OwnerType == "" {
		acc.OwnerType = string(model.RoleAdmin)
	}
	if acc.Status == "" {
		acc.Status = "active"
	}
	if strings.TrimSpace(acc.UUID) == "" {
		acc.XrayEnabled = false
	}
	now := time.Now().UTC()
	if acc.CreatedAt.IsZero() {
		acc.CreatedAt = now
	}
	acc.UpdatedAt = now
	if err := s.st.UpsertAccount(ctx, acc); err != nil {
		return nil, err
	}
	if err := s.st.UpsertDeviceUser(ctx, acc.Username, acc.UUID, acc.LimitConnections); err != nil {
		return nil, err
	}
	if err := s.sys.ApplyAccount(ctx, acc); err != nil {
		return nil, err
	}
	if !s.cfg.PrincipalManagerOnly && !s.sys.UserExists(ctx, acc.Username) {
		return nil, fmt.Errorf("usuário não foi criado no sistema: %s", acc.Username)
	}
	if acc.XrayEnabled && s.xray != nil {
		if _, err := s.xray.ApplyAccount(ctx, acc, s.xrayOpts); err != nil {
			return nil, err
		}
	} else if !acc.XrayEnabled && strings.TrimSpace(acc.UUID) != "" && s.xray != nil {
		// UUID oculto/inativo permanece salvo no banco, mas não pode ficar aplicado no Xray.
		_ = s.xray.RemoveAccount(ctx, acc.Username, acc.UUID, s.xrayOpts)
	}
	s.addEvent(ctx, acc.Username, "remote_apply", acc, actor.TelegramID)
	_ = s.mirrors.UpsertAccount(ctx, acc)
	return &acc, nil
}

// ActivateAccountXrayIfEligible ativa/aplica um UUID oculto quando o Xray Geral está ON
// e o dono da conta tem permissão para usar Xray. UUID permanece salvo mesmo oculto.
func (s *Service) ActivateAccountXrayIfEligible(ctx context.Context, acc model.Account, xrayGeneral bool) (*model.Account, error) {
	if !xrayGeneral || acc.XrayEnabled || strings.TrimSpace(acc.UUID) == "" {
		return &acc, nil
	}
	if !s.ownerAllowsXray(ctx, acc) {
		return &acc, nil
	}
	acc.XrayEnabled = true
	acc.UpdatedAt = time.Now().UTC()
	if err := s.st.UpsertAccount(ctx, acc); err != nil {
		return nil, err
	}
	if err := s.st.UpsertDeviceUser(ctx, acc.Username, acc.UUID, acc.LimitConnections); err != nil {
		return nil, err
	}
	if s.xray != nil {
		if _, err := s.xray.ApplyAccount(ctx, acc, s.xrayOpts); err != nil {
			return nil, err
		}
	}
	s.addEvent(ctx, acc.Username, "xray_activate", acc, 0)
	_ = s.mirrors.UpsertAccount(ctx, acc)
	return &acc, nil
}

func (s *Service) ActivateEligibleHiddenXray(ctx context.Context, xrayGeneral bool) (int, error) {
	if !xrayGeneral {
		return 0, nil
	}
	accs, err := s.st.ListAccounts(ctx, false)
	if err != nil {
		return 0, err
	}
	activated := 0
	for _, acc := range accs {
		if acc.DeletedAt != nil || acc.Status == "deleted" || acc.XrayEnabled || strings.TrimSpace(acc.UUID) == "" {
			continue
		}
		updated, err := s.ActivateAccountXrayIfEligible(ctx, acc, true)
		if err != nil {
			return activated, err
		}
		if updated != nil && updated.XrayEnabled {
			activated++
		}
	}
	return activated, nil
}

// SuspendExpiredAccess bloqueia o acesso real de contas vencidas sem apagar o cadastro.
// A conta permanece no SQLite como suspensa/expirada para listagem e renovação posterior.
func (s *Service) SuspendExpiredAccess(ctx context.Context) ([]string, error) {
	if s == nil || s.st == nil || s.sys == nil {
		return nil, nil
	}
	accs, err := s.st.ListAccounts(ctx, false)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	suspended := []string{}
	errs := []string{}
	for _, acc := range accs {
		if acc.DeletedAt != nil || strings.EqualFold(acc.Status, "deleted") || !strings.EqualFold(acc.Status, "active") {
			continue
		}
		if acc.ExpiresAt.IsZero() || acc.ExpiresAt.After(now) {
			continue
		}
		if err := s.sys.RemoveAccount(ctx, acc.Username); err != nil {
			// Não some do painel se a remoção do acesso tiver erro parcial.
			// A conta continua marcada como suspensa/expirada para renovar depois.
			errs = append(errs, acc.Username+": "+err.Error())
		}
		if acc.XrayEnabled && acc.UUID != "" && s.xray != nil {
			if err := s.xray.RemoveAccount(ctx, acc.Username, acc.UUID, s.xrayOpts); err != nil {
				errs = append(errs, acc.Username+": xray: "+err.Error())
			}
		}
		_ = s.st.ClearDevicesForUser(ctx, acc.Username, true)
		acc.Status = "suspended"
		acc.UpdatedAt = now
		if err := s.st.UpsertAccount(ctx, acc); err != nil {
			errs = append(errs, acc.Username+": sqlite: "+err.Error())
			continue
		}
		s.addEvent(ctx, acc.Username, "auto_suspend_expired", map[string]any{"username": acc.Username, "expires_at": acc.ExpiresAt.UTC().Format(time.RFC3339)}, 0)
		suspended = append(suspended, acc.Username)
	}
	if len(suspended) > 0 && s.mirrors != nil {
		_ = s.mirrors.RefreshAll(ctx)
	}
	if len(errs) > 0 && len(suspended) == 0 {
		return suspended, errors.New(strings.Join(errs, "; "))
	}
	return suspended, nil
}

func (s *Service) ownerAllowsXray(ctx context.Context, acc model.Account) bool {
	if acc.OwnerType == "" || acc.OwnerType == string(model.RoleAdmin) || acc.OwnerTelegramID == 0 {
		return true
	}
	r, err := s.st.FindReseller(ctx, acc.OwnerTelegramID)
	if err != nil || r == nil {
		return false
	}
	return r.AllowXray
}

func (s *Service) addEvent(ctx context.Context, username, typ string, v any, actor int64) {
	b, _ := json.Marshal(v)
	_ = s.st.AddAccountEvent(ctx, username, typ, string(b), actor)
}
func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
