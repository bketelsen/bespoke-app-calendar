package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/bketelsen/bespoke/pkg/auth"
	"github.com/bketelsen/bespoke/pkg/events"
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
	var prevStatus, lastSync string
	err := s.db.QueryRowContext(ctx, `SELECT id,login,provider,email,credential_ciphertext,credential_nonce,status,COALESCE(last_sync_at,'') FROM accounts WHERE id=? AND login=?`, id, login).Scan(&a.ID, &a.Login, &a.Provider, &a.Email, &a.Ciphertext, &a.Nonce, &prevStatus, &lastSync)
	if err != nil {
		return err
	}
	cred, err := s.vault.Open(a.Ciphertext, a.Nonce)
	if err != nil {
		return err
	}
	imp := &importPublisher{login: a.Login, firstSync: lastSync == ""}
	_, _ = s.db.ExecContext(ctx, `UPDATE accounts SET status='syncing',status_detail='Connecting',updated_at=datetime('now') WHERE id=?`, id)
	switch a.Provider {
	case "gmail":
		err = s.syncGoogle(ctx, a, cred, imp)
	case "icloud":
		err = s.syncICloud(ctx, a, cred, imp)
	default:
		err = errors.New("unsupported calendar provider")
	}
	if err != nil {
		detail := truncate(err)
		_, _ = s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE accounts SET status='error',status_detail=?,updated_at=datetime('now') WHERE id=?`, detail, id)
		// Transition-gated: publish only when the account newly breaks, so the
		// 10-minute background poller cannot re-notify while it stays broken.
		if prevStatus != "error" {
			publishEvent(context.WithoutCancel(ctx), auth.User{Login: login}, events.Event{Type: "calendar.account.sync_failed", SubjectID: fmt.Sprint(a.ID), Data: map[string]any{"account_id": a.ID, "provider": a.Provider, "email": a.Email, "detail": detail}, Notification: &events.Notification{Title: clip("Calendar sync failing for "+a.Email, 120), Body: clip(detail, 500), AppSlug: "calendar", Path: "/settings"}})
		}
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
