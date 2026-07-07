package resellers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/mirrors"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
)

type Service struct {
	st      *store.DB
	mirrors *mirrors.Writer
}

func NewService(st *store.DB, mw *mirrors.Writer) *Service { return &Service{st: st, mirrors: mw} }

func (s *Service) Find(ctx context.Context, id int64) (*model.Reseller, error) {
	return s.st.FindReseller(ctx, id)
}

type CreateDraft struct {
	TelegramID       int64
	Name             string
	WhatsAppPhone    string
	Password         string
	Credits          int
	ValidityDays     int
	MaxDays          int
	MaxLimit         int
	MonthlyPrice     float64
	AllowXray        bool
	AllowSubReseller bool
}

func (s *Service) Create(ctx context.Context, actor model.Actor, d CreateDraft) (*model.Reseller, error) {
	if d.TelegramID <= 0 {
		return nil, errors.New("ID Telegram inválido")
	}
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" {
		return nil, errors.New("nome vazio")
	}
	d.WhatsAppPhone = digits(d.WhatsAppPhone)
	if d.WhatsAppPhone == "" {
		return nil, errors.New("WhatsApp vazio")
	}
	if d.Credits < 0 {
		return nil, errors.New("limite inválido")
	}
	if d.ValidityDays <= 0 {
		return nil, errors.New("validade inválida")
	}
	if d.MaxDays <= 0 {
		d.MaxDays = 3650
	}
	if d.MaxLimit <= 0 {
		d.MaxLimit = 1
	}
	if d.Password != "" {
		if err := ValidatePassword(d.Password); err != nil {
			return nil, err
		}
	}
	if old, _ := s.st.FindReseller(ctx, d.TelegramID); old != nil {
		return nil, errors.New("revenda já existe com esse ID")
	}

	now := time.Now().UTC()
	r := model.Reseller{TelegramID: d.TelegramID, Name: d.Name, WhatsAppPhone: d.WhatsAppPhone, Password: d.Password, Credits: d.Credits, Active: true, MaxDays: d.MaxDays, MaxLimit: d.MaxLimit, AllowXray: d.AllowXray, AllowSubReseller: d.AllowSubReseller, ExpiresAt: now.AddDate(0, 0, d.ValidityDays), MonthlyPrice: d.MonthlyPrice, CreatedAt: now, UpdatedAt: now}

	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		r.Level = 0
		r.ParentTelegramID = 0
	} else {
		parent, err := s.st.FindReseller(ctx, actor.TelegramID)
		if err != nil || parent == nil {
			return nil, errors.New("revenda principal não encontrada")
		}
		if parent.Level == 1 || parent.ParentTelegramID != 0 {
			return nil, errors.New("SubRevenda não pode criar outra SubRevenda")
		}
		if br := s.BlockReason(ctx, parent); br != "" {
			return nil, errors.New(br)
		}
		if !parent.AllowSubReseller {
			return nil, errors.New("revenda sem permissão para criar SubRevenda")
		}
		if parent.Credits < d.Credits {
			return nil, errors.New("limite insuficiente para criar SubRevenda")
		}
		r.Level = 1
		r.ParentTelegramID = parent.TelegramID
		r.AllowXray = parent.AllowXray
		r.AllowSubReseller = false
		if err := s.st.WithTx(ctx, func(tx *store.DB) error {
			parent.Credits -= d.Credits
			if err := tx.UpsertReseller(ctx, *parent); err != nil {
				return err
			}
			if err := tx.UpsertReseller(ctx, r); err != nil {
				return err
			}
			return tx.AddCreditMovement(ctx, parent.TelegramID, -d.Credits, "create_subreseller", "", r.TelegramID, actor.TelegramID)
		}); err != nil {
			return nil, err
		}
		_ = s.refresh(ctx)
		return &r, nil
	}
	if err := s.st.UpsertReseller(ctx, r); err != nil {
		return nil, err
	}
	_ = s.refresh(ctx)
	return &r, nil
}

func (s *Service) Renew(ctx context.Context, actor model.Actor, targetID int64, days, newCredits int, allowXray *bool) (*model.Reseller, error) {
	if days <= 0 {
		return nil, errors.New("dias inválidos")
	}
	r, err := s.st.FindReseller(ctx, targetID)
	if err != nil || r == nil {
		return nil, errors.New("revenda não encontrada")
	}
	if err := s.ensureCanManage(ctx, actor, r); err != nil {
		return nil, err
	}
	if newCredits < 0 {
		return nil, errors.New("limite inválido")
	}
	base := time.Now().UTC()
	if r.ExpiresAt.After(base) {
		base = r.ExpiresAt
	}
	oldCredits := r.Credits
	r.ExpiresAt = base.AddDate(0, 0, days)
	r.Active = true
	if newCredits > 0 {
		r.Credits = newCredits
	}
	if allowXray != nil && (actor.IsAdmin || actor.Role == model.RoleAdmin) {
		r.AllowXray = *allowXray
	}
	r.UpdatedAt = time.Now().UTC()
	if !actor.IsAdmin && actor.Role != model.RoleAdmin && r.ParentTelegramID == actor.TelegramID {
		diff := r.Credits - oldCredits
		parent, _ := s.st.FindReseller(ctx, actor.TelegramID)
		if parent == nil {
			return nil, errors.New("revenda principal não encontrada")
		}
		if diff > 0 && parent.Credits < diff {
			return nil, errors.New("limite insuficiente para aumentar SubRevenda")
		}
		r.AllowXray = parent.AllowXray
		err := s.st.WithTx(ctx, func(tx *store.DB) error {
			if diff != 0 {
				parent.Credits -= diff
				if err := tx.UpsertReseller(ctx, *parent); err != nil {
					return err
				}
				reason := "increase_subreseller_limit"
				amount := -diff
				if diff < 0 {
					reason = "decrease_subreseller_limit"
					amount = -diff
				}
				if err := tx.AddCreditMovement(ctx, parent.TelegramID, amount, reason, "", r.TelegramID, actor.TelegramID); err != nil {
					return err
				}
			}
			return tx.UpsertReseller(ctx, *r)
		})
		if err != nil {
			return nil, err
		}
	} else if err := s.st.UpsertReseller(ctx, *r); err != nil {
		return nil, err
	}
	_ = s.refresh(ctx)
	return r, nil
}

func (s *Service) ChangeCredits(ctx context.Context, actor model.Actor, targetID int64, credits int) (*model.Reseller, error) {
	if credits < 0 {
		return nil, errors.New("limite inválido")
	}
	r, err := s.st.FindReseller(ctx, targetID)
	if err != nil || r == nil {
		return nil, errors.New("revenda não encontrada")
	}
	if err := s.ensureCanManage(ctx, actor, r); err != nil {
		return nil, err
	}
	old := r.Credits
	r.Credits = credits
	r.UpdatedAt = time.Now().UTC()
	if !actor.IsAdmin && actor.Role != model.RoleAdmin && r.ParentTelegramID == actor.TelegramID {
		diff := credits - old
		parent, _ := s.st.FindReseller(ctx, actor.TelegramID)
		if parent == nil {
			return nil, errors.New("revenda principal não encontrada")
		}
		if diff > 0 && parent.Credits < diff {
			return nil, errors.New("limite insuficiente")
		}
		if err := s.st.WithTx(ctx, func(tx *store.DB) error {
			parent.Credits -= diff
			if err := tx.UpsertReseller(ctx, *parent); err != nil {
				return err
			}
			if err := tx.UpsertReseller(ctx, *r); err != nil {
				return err
			}
			if diff != 0 {
				return tx.AddCreditMovement(ctx, parent.TelegramID, -diff, "edit_subreseller_limit", "", r.TelegramID, actor.TelegramID)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	} else if err := s.st.UpsertReseller(ctx, *r); err != nil {
		return nil, err
	}
	_ = s.refresh(ctx)
	return r, nil
}

func (s *Service) ChangeMonthlyPrice(ctx context.Context, actor model.Actor, targetID int64, price float64) (*model.Reseller, error) {
	if price < 0 {
		return nil, errors.New("valor inválido")
	}
	r, err := s.st.FindReseller(ctx, targetID)
	if err != nil || r == nil {
		return nil, errors.New("revenda não encontrada")
	}
	if err := s.ensureCanManage(ctx, actor, r); err != nil {
		return nil, err
	}
	r.MonthlyPrice = price
	r.UpdatedAt = time.Now().UTC()
	if err := s.st.UpsertReseller(ctx, *r); err != nil {
		return nil, err
	}
	_ = s.refresh(ctx)
	return r, nil
}

func (s *Service) ChangeWhatsApp(ctx context.Context, actor model.Actor, targetID int64, phone string) (*model.Reseller, error) {
	phone = digits(phone)
	if phone == "" {
		return nil, errors.New("WhatsApp vazio")
	}
	r, err := s.st.FindReseller(ctx, targetID)
	if err != nil || r == nil {
		return nil, errors.New("revenda não encontrada")
	}
	if err := s.ensureCanManage(ctx, actor, r); err != nil {
		return nil, err
	}
	r.WhatsAppPhone = phone
	r.UpdatedAt = time.Now().UTC()
	if err := s.st.UpsertReseller(ctx, *r); err != nil {
		return nil, err
	}
	_ = s.refresh(ctx)
	return r, nil
}

func (s *Service) SetActive(ctx context.Context, actor model.Actor, targetID int64, active bool) (*model.Reseller, error) {
	r, err := s.st.FindReseller(ctx, targetID)
	if err != nil || r == nil {
		return nil, errors.New("revenda não encontrada")
	}
	if err := s.ensureCanManage(ctx, actor, r); err != nil {
		return nil, err
	}
	r.Active = active
	r.UpdatedAt = time.Now().UTC()
	if err := s.st.UpsertReseller(ctx, *r); err != nil {
		return nil, err
	}
	_ = s.refresh(ctx)
	return r, nil
}

func (s *Service) SetAllowSubReseller(ctx context.Context, actor model.Actor, targetID int64, allow bool) (*model.Reseller, error) {
	if !actor.IsAdmin && actor.Role != model.RoleAdmin {
		return nil, errors.New("somente admin")
	}
	r, err := s.st.FindReseller(ctx, targetID)
	if err != nil || r == nil {
		return nil, errors.New("revenda não encontrada")
	}
	if r.Level == 1 || r.ParentTelegramID != 0 {
		return nil, errors.New("SubRevenda não pode criar SubRevenda")
	}
	r.AllowSubReseller = allow
	r.UpdatedAt = time.Now().UTC()
	if err := s.st.UpsertReseller(ctx, *r); err != nil {
		return nil, err
	}
	_ = s.refresh(ctx)
	return r, nil
}

func (s *Service) Delete(ctx context.Context, actor model.Actor, targetID int64) error {
	r, err := s.st.FindReseller(ctx, targetID)
	if err != nil || r == nil {
		return errors.New("revenda não encontrada")
	}
	if err := s.ensureCanManage(ctx, actor, r); err != nil {
		return err
	}
	if !actor.IsAdmin && actor.Role != model.RoleAdmin && r.ParentTelegramID == actor.TelegramID {
		parent, _ := s.st.FindReseller(ctx, actor.TelegramID)
		if parent == nil {
			return errors.New("revenda principal não encontrada")
		}
		if err := s.st.WithTx(ctx, func(tx *store.DB) error {
			parent.Credits += r.Credits
			if err := tx.UpsertReseller(ctx, *parent); err != nil {
				return err
			}
			if err := tx.MarkResellerDeleted(ctx, r.TelegramID); err != nil {
				return err
			}
			return tx.AddCreditMovement(ctx, parent.TelegramID, r.Credits, "delete_subreseller_refund", "", r.TelegramID, actor.TelegramID)
		}); err != nil {
			return err
		}
	} else if err := s.st.MarkResellerDeleted(ctx, r.TelegramID); err != nil {
		return err
	}
	_ = s.refresh(ctx)
	return nil
}

func (s *Service) FindByQuery(ctx context.Context, q string) (*model.Reseller, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	if id, err := strconv.ParseInt(q, 10, 64); err == nil {
		return s.st.FindReseller(ctx, id)
	}
	rs, err := s.st.ListResellers(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if strings.EqualFold(r.Name, q) {
			rr := r
			return &rr, nil
		}
	}
	return nil, nil
}

func (s *Service) ValidateCanCreateAccount(ctx context.Context, actor model.Actor, days, limit int, wantsXray bool) error {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return nil
	}
	r, err := s.st.FindReseller(ctx, actor.TelegramID)
	if err != nil || r == nil {
		return errors.New("revenda não encontrada")
	}
	if reason := s.BlockReason(ctx, r); reason != "" {
		return errors.New(reason)
	}
	if r.Credits <= 0 {
		return errors.New("revenda sem limite disponível")
	}
	if days > r.MaxDays {
		return fmt.Errorf("validade acima do permitido (%d dias)", r.MaxDays)
	}
	if limit > r.MaxLimit {
		return fmt.Errorf("limite acima do permitido (%d)", r.MaxLimit)
	}
	if wantsXray && !r.AllowXray {
		return errors.New("revenda sem permissão para Xray")
	}
	return nil
}
func (s *Service) SpendForAccount(ctx context.Context, actor model.Actor, username string) error {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return nil
	}
	return s.st.WithTx(ctx, func(tx *store.DB) error {
		r, err := tx.FindReseller(ctx, actor.TelegramID)
		if err != nil || r == nil {
			return errors.New("revenda não encontrada")
		}
		if r.Credits <= 0 {
			return errors.New("revenda sem limite disponível")
		}
		r.Credits--
		if err := tx.UpsertReseller(ctx, *r); err != nil {
			return err
		}
		return tx.AddCreditMovement(ctx, r.TelegramID, -1, "create_account", username, 0, actor.TelegramID)
	})
}
func (s *Service) RefundForAccount(ctx context.Context, actor model.Actor, username string) error {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return nil
	}
	return s.st.WithTx(ctx, func(tx *store.DB) error {
		r, err := tx.FindReseller(ctx, actor.TelegramID)
		if err != nil || r == nil {
			return errors.New("revenda não encontrada")
		}
		r.Credits++
		if err := tx.UpsertReseller(ctx, *r); err != nil {
			return err
		}
		return tx.AddCreditMovement(ctx, r.TelegramID, 1, "remove_account_refund", username, 0, actor.TelegramID)
	})
}
func (s *Service) BlockReason(ctx context.Context, r *model.Reseller) string {
	if r == nil {
		return "revenda não encontrada"
	}
	if !r.Active {
		return "revenda bloqueada"
	}
	if !r.ExpiresAt.IsZero() && time.Now().After(r.ExpiresAt) {
		return "revenda expirada"
	}
	seen := map[int64]bool{r.TelegramID: true}
	cur := r
	for cur.ParentTelegramID != 0 {
		if seen[cur.ParentTelegramID] {
			return "cadeia inválida"
		}
		seen[cur.ParentTelegramID] = true
		parent, err := s.st.FindReseller(ctx, cur.ParentTelegramID)
		if err != nil || parent == nil {
			return "revenda principal não encontrada"
		}
		if !parent.Active {
			return "revenda principal bloqueada"
		}
		if !parent.ExpiresAt.IsZero() && time.Now().After(parent.ExpiresAt) {
			return "revenda principal expirada"
		}
		cur = parent
	}
	return ""
}

func (s *Service) ensureCanManage(ctx context.Context, actor model.Actor, r *model.Reseller) error {
	if actor.IsAdmin || actor.Role == model.RoleAdmin {
		return nil
	}
	if actor.Role == model.RoleSubReseller {
		return errors.New("SubRevenda não pode criar, editar, renovar, suspender ou remover outra SubRevenda")
	}
	if actor.Role == model.RoleReseller && r.ParentTelegramID == actor.TelegramID {
		return nil
	}
	return errors.New("sem permissão para essa revenda")
}
func (s *Service) refresh(ctx context.Context) error {
	if s.mirrors != nil {
		return s.mirrors.WriteResellersJSON(ctx)
	}
	return nil
}
func ValidatePassword(s string) error {
	if strings.ContainsAny(s, "\n\r:") {
		return errors.New("senha inválida")
	}
	if len(s) > 72 {
		return errors.New("senha grande demais")
	}
	return nil
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
