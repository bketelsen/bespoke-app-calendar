package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/bketelsen/bespoke/pkg/web"
)

type syncAccount struct {
	ID                     int64
	Login, Provider, Email string
	Ciphertext, Nonce      []byte
}
type synchronizer struct {
	db     *sql.DB
	vault  *credentialVault
	google *googleConfig
}

func (s *synchronizer) Sync(ctx context.Context, login string, id int64) error {
	var a syncAccount
	err := s.db.QueryRowContext(ctx, `SELECT id,login,provider,email,credential_ciphertext,credential_nonce FROM accounts WHERE id=? AND login=?`, id, login).Scan(&a.ID, &a.Login, &a.Provider, &a.Email, &a.Ciphertext, &a.Nonce)
	if err != nil {
		return err
	}
	cred, err := s.vault.Open(a.Ciphertext, a.Nonce)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE accounts SET status='syncing',status_detail='Connecting',updated_at=datetime('now') WHERE id=?`, id)
	switch a.Provider {
	case "gmail":
		err = s.syncGoogle(ctx, a, cred)
	case "icloud":
		err = s.syncICloud(ctx, a, cred)
	default:
		err = errors.New("unsupported calendar provider")
	}
	if err != nil {
		detail := truncate(err)
		_, _ = s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE accounts SET status='error',status_detail=?,updated_at=datetime('now') WHERE id=?`, detail, id)
		web.Changed(login)
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE accounts SET status='healthy',status_detail='',last_sync_at=datetime('now'),updated_at=datetime('now') WHERE id=?`, id)
	web.Changed(login)
	return err
}

func (s *synchronizer) SyncAll(ctx context.Context, login string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM accounts WHERE login=? AND status!='disabled' ORDER BY id`, login)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var failures []string
	for _, id := range ids {
		if err := s.Sync(ctx, login, id); err != nil {
			failures = append(failures, fmt.Sprintf("#%d: %v", id, err))
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}
func truncate(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
