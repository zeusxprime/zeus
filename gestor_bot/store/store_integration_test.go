package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"primecel-gestor/gestor_bot/model"
)

func TestAccountRoundTrip(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "gestor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	acc := model.Account{Username: "Cliente1", Password: "12345", LimitConnections: 1, ExpiresAt: time.Now().Add(24 * time.Hour), ExpiryDate: "2026-07-02", OwnerType: "admin", Status: "active", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.UpsertAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	got, err := db.FindAccount(context.Background(), "cliente1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Username != "Cliente1" {
		t.Fatalf("unexpected account: %#v", got)
	}
	if err := db.MarkAccountDeleted(context.Background(), "Cliente1"); err != nil {
		t.Fatal(err)
	}
	got, err = db.FindAccount(context.Background(), "Cliente1")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("deleted account still visible: %#v", got)
	}
}
