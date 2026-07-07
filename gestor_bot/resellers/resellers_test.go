package resellers

import (
	"context"
	"path/filepath"
	"testing"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/mirrors"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
)

func newTestService(t *testing.T) (*Service, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "gestor.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: dir, UsuariosDBPath: filepath.Join(dir, "usuarios.db")}
	return NewService(st, mirrors.NewWriter(cfg, st)), st
}

func TestCreateSubResellerCreditFlow(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(t)
	defer st.Close()
	admin := model.Actor{TelegramID: 1, Name: "Admin", Role: model.RoleAdmin, IsAdmin: true}
	parent, err := svc.Create(ctx, admin, CreateDraft{TelegramID: 100, Name: "Pai", WhatsAppPhone: "558500000000", Credits: 10, ValidityDays: 30, MaxDays: 90, MaxLimit: 2, MonthlyPrice: 25, AllowXray: true, AllowSubReseller: true})
	if err != nil {
		t.Fatal(err)
	}
	actorParent := model.Actor{TelegramID: parent.TelegramID, Name: parent.Name, Role: model.RoleReseller}
	sub, err := svc.Create(ctx, actorParent, CreateDraft{TelegramID: 200, Name: "Sub", WhatsAppPhone: "558511111111", Credits: 3, ValidityDays: 30, MonthlyPrice: 10})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Level != 1 || sub.ParentTelegramID != parent.TelegramID {
		t.Fatalf("sub fields wrong: %+v", sub)
	}
	parent2, _ := st.FindReseller(ctx, parent.TelegramID)
	if parent2.Credits != 7 {
		t.Fatalf("credits=%d want 7", parent2.Credits)
	}
	if _, err := svc.Create(ctx, model.Actor{TelegramID: sub.TelegramID, Role: model.RoleSubReseller}, CreateDraft{TelegramID: 300, Name: "Sub2", WhatsAppPhone: "558522222222", Credits: 1, ValidityDays: 30}); err == nil {
		t.Fatal("subrevenda should not create subrevenda")
	}
	if err := svc.Delete(ctx, actorParent, sub.TelegramID); err != nil {
		t.Fatal(err)
	}
	parent3, _ := st.FindReseller(ctx, parent.TelegramID)
	if parent3.Credits != 10 {
		t.Fatalf("refund credits=%d want 10", parent3.Credits)
	}
}

func TestBlockReasonParentChain(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(t)
	defer st.Close()
	admin := model.Actor{TelegramID: 1, Role: model.RoleAdmin, IsAdmin: true}
	parent, err := svc.Create(ctx, admin, CreateDraft{TelegramID: 100, Name: "Pai", WhatsAppPhone: "558500000000", Credits: 10, ValidityDays: 30, AllowSubReseller: true})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := svc.Create(ctx, model.Actor{TelegramID: parent.TelegramID, Role: model.RoleReseller}, CreateDraft{TelegramID: 200, Name: "Sub", WhatsAppPhone: "558511111111", Credits: 1, ValidityDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetActive(ctx, admin, parent.TelegramID, false); err != nil {
		t.Fatal(err)
	}
	sub2, _ := st.FindReseller(ctx, sub.TelegramID)
	if br := svc.BlockReason(ctx, sub2); br == "" {
		t.Fatal("expected parent block reason")
	}
}
