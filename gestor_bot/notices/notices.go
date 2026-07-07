package notices

import (
	"context"
	"errors"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
)

type Sender interface {
	SendMessage(ctx context.Context, chatID int64, text string) error
}
type Manager struct {
	Store  *store.DB
	Sender Sender
}

func NewManager(st *store.DB, sender Sender) *Manager { return &Manager{Store: st, Sender: sender} }

type Report struct {
	Kind      string `json:"kind"`
	Targets   int    `json:"targets"`
	Delivered int    `json:"delivered"`
	Failed    int    `json:"failed"`
}

func (m *Manager) Broadcast(ctx context.Context, kind, message string) (Report, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "aviso"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return Report{}, errors.New("mensagem vazia")
	}
	rs, err := m.Store.ListResellers(ctx)
	if err != nil {
		return Report{}, err
	}
	prefix := "📢 Aviso:"
	if strings.Contains(strings.ToLower(kind), "nov") {
		prefix = "🆕 Novidades:"
	}
	rep := Report{Kind: kind}
	for _, r := range rs {
		if r.DeletedAt != nil || r.TelegramID == 0 {
			continue
		}
		rep.Targets++
		if m.Sender != nil {
			if err := m.Sender.SendMessage(ctx, r.TelegramID, prefix+"\n"+message); err != nil {
				rep.Failed++
			} else {
				rep.Delivered++
				time.Sleep(40 * time.Millisecond)
			}
		}
	}
	if m.Sender == nil {
		rep.Delivered = 0
	}
	_ = m.Store.AddNoticeEvent(ctx, kind, message, rep.Targets, rep.Delivered, rep.Failed)
	return rep, nil
}
func VisibleTargets(actor model.Actor, resellers []model.Reseller) []int64 {
	var ids []int64
	if !actor.IsAdmin {
		return ids
	}
	for _, r := range resellers {
		ids = append(ids, r.TelegramID)
	}
	return ids
}
