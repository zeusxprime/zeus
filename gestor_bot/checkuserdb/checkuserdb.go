package checkuserdb

import (
	"context"
	"errors"
	"os"
	"strings"

	"primecel-gestor/gestor_bot/store"
)

const DefaultPath = "/root/db.sqlite3"

// ClearUser removes DTunnel CheckUser-Go device registrations for one username
// from the separated CheckUser database. Missing database/table is treated as no-op.
func ClearUser(ctx context.Context, path, username string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("username obrigatório")
	}
	db, ok, err := openExisting(ctx, path)
	if err != nil || !ok {
		return err
	}
	defer db.Close()
	exists, err := tableExists(ctx, db)
	if err != nil || !exists {
		return err
	}
	return db.Exec(ctx, `DELETE FROM devices WHERE username=? OR lower(username)=lower(?)`, username, username)
}

// ClearUsers removes DTunnel CheckUser-Go device registrations for a group of usernames.
// Missing database/table is treated as no-op.
func ClearUsers(ctx context.Context, path string, usernames []string) error {
	db, ok, err := openExisting(ctx, path)
	if err != nil || !ok {
		return err
	}
	defer db.Close()
	exists, err := tableExists(ctx, db)
	if err != nil || !exists {
		return err
	}
	seen := map[string]bool{}
	for _, username := range usernames {
		username = strings.TrimSpace(username)
		key := strings.ToLower(username)
		if username == "" || seen[key] {
			continue
		}
		seen[key] = true
		if err := db.Exec(ctx, `DELETE FROM devices WHERE username=? OR lower(username)=lower(?)`, username, username); err != nil {
			return err
		}
	}
	return nil
}

// ClearAll removes every DTunnel CheckUser-Go device registration from the separated
// CheckUser database. Missing database/table is treated as no-op.
func ClearAll(ctx context.Context, path string) error {
	db, ok, err := openExisting(ctx, path)
	if err != nil || !ok {
		return err
	}
	defer db.Close()
	exists, err := tableExists(ctx, db)
	if err != nil || !exists {
		return err
	}
	return db.Exec(ctx, `DELETE FROM devices`)
}

func openExisting(ctx context.Context, path string) (*store.DB, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = DefaultPath
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if info.IsDir() {
		return nil, false, errors.New("CHECKUSER_DB_PATH aponta para uma pasta")
	}
	db, err := store.Open(path)
	if err != nil {
		return nil, false, err
	}
	return db, true, nil
}

func tableExists(ctx context.Context, db *store.DB) (bool, error) {
	rows, err := db.Query(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='devices' LIMIT 1`)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}
