package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/bketelsen/bespoke-app-calendar/views"
	"github.com/bketelsen/bespoke/pkg/auth"
	"github.com/bketelsen/bespoke/pkg/db"
	"github.com/bketelsen/bespoke/pkg/events"
	"github.com/bketelsen/bespoke/pkg/web"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type syncRequest struct {
	login string
	id    int64
}

func main() {
	migrations, _ := fs.Sub(migrationFS, "migrations")
	sqldb, err := db.Open("calendar", migrations)
	if err != nil {
		log.Fatal(err)
	}
	vault, vaultErr := loadCredentialVault()
	google, googleErr := loadGoogleConfig()
	syncer := &synchronizer{db: sqldb, vault: vault, google: google}
	queue := make(chan syncRequest, 32)
	go func() {
		for q := range queue {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			if err := syncer.Sync(ctx, q.login, q.id); err != nil {
				log.Printf("calendar sync account %d: %v", q.id, err)
			}
			cancel()
		}
	}()
	queueSync := func(login string, id int64) error {
		select {
		case queue <- syncRequest{login, id}:
			return nil
		default:
			return errors.New("calendar sync queue is busy")
		}
	}
	go backgroundSync(sqldb, queueSync)
	web.Run("calendar", func(mux *http.ServeMux) {
		registerTools(mux, sqldb, syncer, queueSync)
		web.EnableChat(mux, "calendar", func(ctx context.Context, user auth.User) (string, error) { return chatContext(ctx, sqldb, user.Login) })
		web.Search(mux, func(ctx context.Context, user auth.User, q string) ([]web.SearchResult, error) {
			return searchEvents(ctx, sqldb, user.Login, q)
		})
		web.DashboardCard(mux, func(ctx context.Context, user auth.User) (templ.Component, error) {
			events, err := loadEvents(ctx, sqldb, user.Login, time.Now(), endOfDay(time.Now()))
			if err != nil {
				return nil, err
			}
			next := ""
			if len(events) > 0 {
				next = events[0].Title + " · " + events[0].When()
			}
			return views.DashCard(next, len(events)), nil
		})
		web.Live(mux, func(ctx context.Context, user auth.User) (templ.Component, error) {
			data, err := pageData(ctx, sqldb, user.Login, "agenda", time.Now(), 0)
			if err != nil {
				return nil, err
			}
			return views.EventsLive(data), nil
		})
		web.Intent(mux, "Calendar", web.IntentDef{Name: "create-event", Title: "Create Calendar Event", Prompt: "Review the title and times before saving.", Handler: func(ctx context.Context, user auth.User, text string) (string, error) {
			return "/?new=" + urlQuery(text), nil
		}})
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			view := r.URL.Query().Get("view")
			date, _ := time.ParseInLocation("2006-01-02", r.URL.Query().Get("date"), time.Local)
			if date.IsZero() {
				date = time.Now()
			}
			eventID, _ := strconv.ParseInt(r.URL.Query().Get("event"), 10, 64)
			data, err := pageData(r.Context(), sqldb, user.Login, view, date, eventID)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			views.Home(user, data).Render(r.Context(), w)
		})
		mux.HandleFunc("POST /events", func(w http.ResponseWriter, r *http.Request) { mutateEvent(w, r, sqldb, 0, queueSync) })
		mux.HandleFunc("POST /events/{id}", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
			mutateEvent(w, r, sqldb, id, queueSync)
		})
		mux.HandleFunc("POST /events/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err := deleteLocalEvent(r.Context(), sqldb, user.Login, id); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			web.Changed(user.Login)
			queueEventAccount(r.Context(), sqldb, user.Login, id, queueSync)
			http.Redirect(w, r, "/", http.StatusSeeOther)
		})
		mux.HandleFunc("POST /calendars/selection", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid calendar selection", http.StatusBadRequest)
				return
			}
			tx, _ := sqldb.BeginTx(r.Context(), nil)
			_, err := tx.ExecContext(r.Context(), `UPDATE calendars SET selected=0 WHERE account_id IN(SELECT id FROM accounts WHERE login=?)`, user.Login)
			if err == nil {
				for _, raw := range r.Form["calendar"] {
					_, err = tx.ExecContext(r.Context(), `UPDATE calendars SET selected=1 WHERE id=? AND account_id IN(SELECT id FROM accounts WHERE login=?)`, raw, user.Login)
					if err != nil {
						break
					}
				}
			}
			if err == nil {
				err = tx.Commit()
			} else {
				tx.Rollback()
			}
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			web.Changed(user.Login)
			http.Redirect(w, r, "/", http.StatusSeeOther)
		})
		mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			accounts, err := loadAccounts(r.Context(), sqldb, user.Login)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			views.Settings(user, accounts, vaultErr == nil, googleErr == nil, r.URL.Query().Get("notice"), r.URL.Query().Get("error")).Render(r.Context(), w)
		})
		mux.HandleFunc("GET /oauth/google/start", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			dest, err := beginGoogleOAuth(r.Context(), sqldb, user.Login, google)
			if err != nil {
				redirectSettings(w, r, "", err)
				return
			}
			http.Redirect(w, r, dest, http.StatusFound)
		})
		mux.HandleFunc("GET /oauth/google/callback", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			id, err := finishGoogleOAuth(r.Context(), sqldb, vault, google, user.Login, r.URL.Query().Get("state"), r.URL.Query().Get("code"))
			if err != nil {
				redirectSettings(w, r, "", err)
				return
			}
			publishAccountConnected(r.Context(), sqldb, user, id)
			if err := queueSync(user.Login, id); err != nil {
				redirectSettings(w, r, "", err)
				return
			}
			web.Changed(user.Login)
			redirectSettings(w, r, "Google Calendar connected.", nil)
		})
		mux.HandleFunc("POST /settings/accounts/icloud", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			id, err := addICloudAccount(r.Context(), sqldb, vault, user.Login, r.FormValue("email"), r.FormValue("password"))
			if err != nil {
				redirectSettings(w, r, "", err)
				return
			}
			publishAccountConnected(r.Context(), sqldb, user, id)
			if err := queueSync(user.Login, id); err != nil {
				redirectSettings(w, r, "", err)
				return
			}
			web.Changed(user.Login)
			redirectSettings(w, r, "iCloud Calendar connected.", nil)
		})
		mux.HandleFunc("POST /settings/accounts/{id}/sync", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if !ownsAccount(r.Context(), sqldb, user.Login, id) {
				http.NotFound(w, r)
				return
			}
			err := queueSync(user.Login, id)
			redirectSettings(w, r, "Calendar sync queued.", err)
		})
		mux.HandleFunc("POST /settings/accounts/{id}/disconnect", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
			var provider, email string
			_ = sqldb.QueryRowContext(r.Context(), `SELECT provider,email FROM accounts WHERE id=? AND login=?`, id, user.Login).Scan(&provider, &email)
			res, err := sqldb.ExecContext(r.Context(), `DELETE FROM accounts WHERE id=? AND login=?`, id, user.Login)
			if err != nil {
				redirectSettings(w, r, "", err)
				return
			}
			if n, _ := res.RowsAffected(); n == 0 {
				http.NotFound(w, r)
				return
			}
			publishEvent(r.Context(), user, events.Event{Type: "calendar.account.disconnected", SubjectID: fmt.Sprint(id), Data: map[string]any{"account_id": id, "provider": provider, "email": email}, Notification: &events.Notification{Title: "Calendar account disconnected", Body: clip(email+" was disconnected and its locally synced calendars and events were removed.", 500), AppSlug: "calendar", Path: "/settings"}})
			web.Changed(user.Login)
			redirectSettings(w, r, "Calendar account disconnected.", nil)
		})
	})
}

func backgroundSync(db *sql.DB, queue func(string, int64) error) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rows, err := db.Query(`SELECT login,id FROM accounts WHERE status!='disabled' ORDER BY COALESCE(last_sync_at,'')`)
		if err != nil {
			continue
		}
		for rows.Next() {
			var login string
			var id int64
			if rows.Scan(&login, &id) == nil {
				_ = queue(login, id)
			}
		}
		rows.Close()
	}
}
func pageData(ctx context.Context, db *sql.DB, login, view string, date time.Time, eventID int64) (views.PageData, error) {
	if view != "day" && view != "week" && view != "month" {
		view = "agenda"
	}
	from, to := dateRange(view, date)
	events, err := loadEvents(ctx, db, login, from, to)
	if err != nil {
		return views.PageData{}, err
	}
	cals, err := loadCalendars(ctx, db, login)
	data := views.PageData{View: view, Date: date, Events: events, Calendars: cals}
	if eventID > 0 {
		for i := range events {
			if events[i].ID == eventID {
				data.Selected = &events[i]
				break
			}
		}
	}
	return data, err
}
func dateRange(view string, date time.Time) (time.Time, time.Time) {
	y, m, d := date.Date()
	switch view {
	case "day":
		from := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
		return from, from.AddDate(0, 0, 1)
	case "week":
		from := time.Date(y, m, d-int(date.Weekday()), 0, 0, 0, 0, time.Local)
		return from, from.AddDate(0, 0, 7)
	case "month":
		from := time.Date(y, m, 1, 0, 0, 0, 0, time.Local)
		return from, from.AddDate(0, 1, 0)
	default:
		from := time.Now()
		return from, from.AddDate(0, 3, 0)
	}
}
func endOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
}
func mutateEvent(w http.ResponseWriter, r *http.Request, db *sql.DB, id int64, queue func(string, int64) error) {
	user := auth.FromContext(r.Context())
	start, err := time.ParseInLocation("2006-01-02T15:04", r.FormValue("start"), time.Local)
	if err != nil {
		http.Error(w, "valid start time required", 400)
		return
	}
	end, err := time.ParseInLocation("2006-01-02T15:04", r.FormValue("end"), time.Local)
	if err != nil {
		http.Error(w, "valid end time required", 400)
		return
	}
	calID, _ := strconv.ParseInt(r.FormValue("calendar_id"), 10, 64)
	in := eventInput{CalendarID: calID, Title: r.FormValue("title"), Description: r.FormValue("description"), Location: r.FormValue("location"), Start: start, End: end, Timezone: time.Local.String(), Recurrence: r.FormValue("recurrence")}
	typ := "calendar.event.updated"
	if id == 0 {
		typ = "calendar.event.created"
		id, err = createLocalEvent(r.Context(), db, user.Login, in)
	} else {
		err = updateLocalEvent(r.Context(), db, user.Login, id, in)
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	web.Changed(user.Login)
	publishMutation(r.Context(), user, typ, id, in, "ui")
	queueEventAccount(r.Context(), db, user.Login, id, queue)
	http.Redirect(w, r, fmt.Sprintf("/?event=%d", id), http.StatusSeeOther)
}
func queueEventAccount(ctx context.Context, db *sql.DB, login string, eventID int64, queue func(string, int64) error) {
	var accountID int64
	if db.QueryRowContext(ctx, `SELECT c.account_id FROM events e JOIN calendars c ON c.id=e.calendar_id JOIN accounts a ON a.id=c.account_id WHERE e.id=? AND a.login=?`, eventID, login).Scan(&accountID) == nil {
		_ = queue(login, accountID)
	}
}
func ownsAccount(ctx context.Context, db *sql.DB, login string, id int64) bool {
	var one int
	return db.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id=? AND login=?`, id, login).Scan(&one) == nil
}
func redirectSettings(w http.ResponseWriter, r *http.Request, notice string, err error) {
	q := ""
	if err != nil {
		q = "?error=" + urlQuery(err.Error())
	} else if notice != "" {
		q = "?notice=" + urlQuery(notice)
	}
	http.Redirect(w, r, "/settings"+q, http.StatusSeeOther)
}
func urlQuery(s string) string { return url.QueryEscape(s) }
func chatContext(ctx context.Context, db *sql.DB, login string) (string, error) {
	events, err := loadEvents(ctx, db, login, time.Now(), time.Now().AddDate(0, 1, 0))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Upcoming calendar agenda (local time):\n")
	for _, e := range events {
		fmt.Fprintf(&b, "#%d %s — %s %s (%s)\n", e.ID, e.Title, e.Start.Local().Format("Mon Jan 2"), e.When(), e.Calendar)
	}
	return b.String(), nil
}
