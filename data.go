package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bketelsen/bespoke-app-calendar/views"
	"github.com/bketelsen/bespoke/pkg/web"
)

type eventInput struct {
	CalendarID                   int64
	Title, Description, Location string
	Start, End                   time.Time
	StartDate, EndDate           string
	Timezone, Recurrence         string
}

func (e eventInput) validate() error {
	if e.CalendarID <= 0 || strings.TrimSpace(e.Title) == "" {
		return errors.New("calendar and title are required")
	}
	if e.StartDate != "" || e.EndDate != "" {
		start, err1 := time.Parse("2006-01-02", e.StartDate)
		end, err2 := time.Parse("2006-01-02", e.EndDate)
		if err1 != nil || err2 != nil || !end.After(start) {
			return errors.New("all-day end date must be after start date")
		}
		return nil
	}
	if e.Start.IsZero() || e.End.IsZero() || !e.End.After(e.Start) {
		return errors.New("event end must be after start")
	}
	return nil
}

func loadAccounts(ctx context.Context, db *sql.DB, login string) ([]views.Account, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,provider,email,status,status_detail,last_sync_at FROM accounts WHERE login=? ORDER BY id`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []views.Account
	for rows.Next() {
		var a views.Account
		var last sql.NullString
		if err := rows.Scan(&a.ID, &a.Provider, &a.Email, &a.Status, &a.StatusDetail, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			stamp, err := parseDBTime(last.String)
			if err != nil {
				return nil, fmt.Errorf("parse account last sync time: %w", err)
			}
			a.LastSyncAt = &stamp
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func parseDBTime(value string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
		if stamp, err := time.Parse(layout, value); err == nil {
			return stamp, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}

func loadCalendars(ctx context.Context, db *sql.DB, login string) ([]views.Calendar, error) {
	rows, err := db.QueryContext(ctx, `SELECT c.id,c.name,c.color,c.selected,c.writable,a.provider,a.email
		FROM calendars c JOIN accounts a ON a.id=c.account_id WHERE a.login=? ORDER BY a.id,c.name`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []views.Calendar
	for rows.Next() {
		var c views.Calendar
		if err := rows.Scan(&c.ID, &c.Name, &c.Color, &c.Selected, &c.Writable, &c.Provider, &c.Email); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func loadEvents(ctx context.Context, db *sql.DB, login string, from, to time.Time) ([]views.Event, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.id,c.id,c.name,c.color,e.title,e.description,e.location,
		COALESCE(e.start_at,''),COALESCE(e.end_at,''),COALESCE(e.start_date,''),COALESCE(e.end_date,''),e.timezone,e.recurrence,e.dirty
		FROM events e JOIN calendars c ON c.id=e.calendar_id JOIN accounts a ON a.id=c.account_id
		WHERE a.login=? AND c.selected=1 AND e.deleted=0 AND (
		(e.start_at IS NOT NULL AND e.start_at < ? AND e.end_at > ?) OR
		(e.start_date IS NOT NULL AND e.start_date < ? AND e.end_date > ?))
		ORDER BY COALESCE(e.start_at,e.start_date),e.title LIMIT 500`, login, to.UTC().Format(time.RFC3339Nano), from.UTC().Format(time.RFC3339Nano), to.Format("2006-01-02"), from.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []views.Event
	for rows.Next() {
		var e views.Event
		var start, end string
		if err := rows.Scan(&e.ID, &e.CalendarID, &e.Calendar, &e.Color, &e.Title, &e.Description, &e.Location, &start, &end, &e.StartDate, &e.EndDate, &e.Timezone, &e.Recurrence, &e.Dirty); err != nil {
			return nil, err
		}
		if start != "" {
			e.Start, _ = time.Parse(time.RFC3339Nano, start)
			e.End, _ = time.Parse(time.RFC3339Nano, end)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func createLocalEvent(ctx context.Context, db *sql.DB, login string, in eventInput) (int64, error) {
	if err := in.validate(); err != nil {
		return 0, err
	}
	var writable int
	if err := db.QueryRowContext(ctx, `SELECT c.writable FROM calendars c JOIN accounts a ON a.id=c.account_id WHERE c.id=? AND a.login=?`, in.CalendarID, login).Scan(&writable); err != nil || writable == 0 {
		return 0, errors.New("calendar is unavailable or read-only")
	}
	remoteID := fmt.Sprintf("local-%d", time.Now().UnixNano())
	var start, end any
	if in.StartDate == "" {
		start = in.Start.UTC().Format(time.RFC3339Nano)
		end = in.End.UTC().Format(time.RFC3339Nano)
	}
	res, err := db.ExecContext(ctx, `INSERT INTO events(calendar_id,remote_id,title,description,location,start_at,end_at,start_date,end_date,timezone,recurrence,dirty)
		VALUES(?,?,?,?,?,?,?,NULLIF(?,''),NULLIF(?,''),?,?,1)`, in.CalendarID, remoteID, strings.TrimSpace(in.Title), strings.TrimSpace(in.Description), strings.TrimSpace(in.Location), start, end, in.StartDate, in.EndDate, in.Timezone, in.Recurrence)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func updateLocalEvent(ctx context.Context, db *sql.DB, login string, id int64, in eventInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	var start, end any
	if in.StartDate == "" {
		start = in.Start.UTC().Format(time.RFC3339Nano)
		end = in.End.UTC().Format(time.RFC3339Nano)
	}
	res, err := db.ExecContext(ctx, `UPDATE events SET calendar_id=?,title=?,description=?,location=?,start_at=?,end_at=?,start_date=NULLIF(?,''),end_date=NULLIF(?,''),timezone=?,recurrence=?,dirty=1,updated_at=datetime('now')
		WHERE id=? AND calendar_id IN(SELECT c.id FROM calendars c JOIN accounts a ON a.id=c.account_id WHERE a.login=? AND c.writable=1)`, in.CalendarID, strings.TrimSpace(in.Title), strings.TrimSpace(in.Description), strings.TrimSpace(in.Location), start, end, in.StartDate, in.EndDate, in.Timezone, in.Recurrence, id, login)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("event is unavailable or read-only")
	}
	return nil
}

func deleteLocalEvent(ctx context.Context, db *sql.DB, login string, id int64) error {
	res, err := db.ExecContext(ctx, `UPDATE events SET deleted=1,dirty=1,updated_at=datetime('now') WHERE id=? AND calendar_id IN(SELECT c.id FROM calendars c JOIN accounts a ON a.id=c.account_id WHERE a.login=? AND c.writable=1)`, id, login)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("event is unavailable or read-only")
	}
	return nil
}

func searchEvents(ctx context.Context, db *sql.DB, login, q string) ([]web.SearchResult, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	rows, err := db.QueryContext(ctx, `SELECT e.id,e.title,e.location,COALESCE(e.start_at,e.start_date) FROM events e JOIN calendars c ON c.id=e.calendar_id JOIN accounts a ON a.id=c.account_id WHERE a.login=? AND e.deleted=0 AND (e.title LIKE ? ESCAPE '\' OR e.description LIKE ? ESCAPE '\' OR e.location LIKE ? ESCAPE '\') ORDER BY COALESCE(e.start_at,e.start_date) DESC LIMIT 20`, login, pattern, pattern, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []web.SearchResult
	for rows.Next() {
		var id int64
		var title, loc, stamp string
		if err := rows.Scan(&id, &title, &loc, &stamp); err != nil {
			return nil, err
		}
		out = append(out, web.SearchResult{Title: title, Snippet: loc, URL: fmt.Sprintf("/?event=%d", id), Timestamp: stamp})
	}
	return out, rows.Err()
}
