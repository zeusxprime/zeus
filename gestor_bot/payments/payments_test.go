package payments

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
)

func TestPaymentOrderAppliesOnce(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "gestor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r := model.Reseller{TelegramID: 123, Name: "Revenda", WhatsAppPhone: "5585999999999", Credits: 2, Active: true, MaxDays: 90, MaxLimit: 10, AllowXray: true, ExpiresAt: now.AddDate(0, 0, 10), MonthlyPrice: 30, CreatedAt: now, UpdatedAt: now}
	if err := db.UpsertReseller(ctx, r); err != nil {
		t.Fatal(err)
	}
	m := NewManager(db)
	if _, err := m.ConfigureOwner(ctx, OwnerConfigInput{OwnerID: 0, Bank: BankMercadoPago, Token: "TEST", Enabled: true, DataJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	o, err := m.CreateOrder(ctx, OrderInput{OwnerID: 0, TargetResellerID: 123, Kind: KindRenewLimit, Months: 1, Credits: 5, Amount: 50, Bank: BankMercadoPago})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.MarkPaid(ctx, o.OrderID, []byte(`{"external_reference":"`+o.OrderID+`"}`)); err != nil {
		t.Fatal(err)
	}
	after, _ := db.FindReseller(ctx, 123)
	if after.Credits != 7 {
		t.Fatalf("credits after first apply = %d, want 7", after.Credits)
	}
	if !after.ExpiresAt.After(now.AddDate(0, 0, 39)) {
		t.Fatalf("renew did not accumulate from current expiry: %s", after.ExpiresAt)
	}
	if _, err := m.MarkPaid(ctx, o.OrderID, []byte(`{"external_reference":"`+o.OrderID+`"}`)); err != nil {
		t.Fatal(err)
	}
	after2, _ := db.FindReseller(ctx, 123)
	if after2.Credits != 7 {
		t.Fatalf("duplicate webhook credited again: got %d", after2.Credits)
	}
}

func TestWebhookProcessLogsAndIgnoresDuplicate(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "gestor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r := model.Reseller{TelegramID: 555, Name: "Revenda", Credits: 1, Active: true, MaxDays: 90, MaxLimit: 10, AllowXray: true, ExpiresAt: now, MonthlyPrice: 30, CreatedAt: now, UpdatedAt: now}
	if err := db.UpsertReseller(ctx, r); err != nil {
		t.Fatal(err)
	}
	m := NewManager(db)
	if _, err := m.ConfigureOwner(ctx, OwnerConfigInput{OwnerID: 0, Bank: BankMercadoPago, Token: "TEST", Enabled: true, DataJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	o, err := m.CreateOrder(ctx, OrderInput{OwnerID: 0, TargetResellerID: 555, Kind: KindLimit, Credits: 3, Amount: 30, Bank: BankMercadoPago})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"external_reference":"` + o.OrderID + `","status":"approved","event_id":"evt-1"}`)
	req, _ := http.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("x-request-id", "evt-1")
	resp, code := m.ProcessWebhook(req, body)
	if code != http.StatusOK || resp["ok"] != true {
		t.Fatalf("webhook resp=%v code=%d", resp, code)
	}
	after, _ := db.FindReseller(ctx, 555)
	if after.Credits != 4 {
		t.Fatalf("credits=%d want 4", after.Credits)
	}
	resp2, code2 := m.ProcessWebhook(req, body)
	if code2 != http.StatusOK || resp2["duplicate"] != true {
		t.Fatalf("duplicate resp=%v code=%d", resp2, code2)
	}
	after2, _ := db.FindReseller(ctx, 555)
	if after2.Credits != 4 {
		t.Fatalf("duplicate credited again: %d", after2.Credits)
	}
	events, err := db.ListPaymentWebhookEvents(ctx, -1, 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("events len=%d err=%v", len(events), err)
	}
}
