package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bketelsen/bespoke/pkg/auth"
	"github.com/bketelsen/bespoke/pkg/web"
)

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
func registerTools(mux *http.ServeMux, db *sql.DB, syncer *synchronizer, queue func(string, int64) error) {
	web.Tool(mux, web.ToolDef{Name: "list_accounts", Description: "List connected Google and iCloud calendar accounts with sync health. Never returns credentials.", Schema: schema(map[string]any{}), Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
		a, err := loadAccounts(ctx, db, user.Login)
		if err != nil {
			return "", err
		}
		b, _ := json.MarshalIndent(a, "", "  ")
		return string(b), nil
	}})
	web.Tool(mux, web.ToolDef{Name: "list_calendars", Description: "List the user's provider calendars, ids, selection, and write access.", Schema: schema(map[string]any{}), Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
		c, err := loadCalendars(ctx, db, user.Login)
		if err != nil {
			return "", err
		}
		b, _ := json.MarshalIndent(c, "", "  ")
		return string(b), nil
	}})
	web.Tool(mux, web.ToolDef{Name: "list_events", Description: "List calendar events in a time range using local RFC3339 timestamps.", Schema: schema(map[string]any{"from": map[string]any{"type": "string", "description": "RFC3339; default now"}, "to": map[string]any{"type": "string", "description": "RFC3339; default three months from now"}}), Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
		var a struct{ From, To string }
		_ = json.Unmarshal(args, &a)
		from := time.Now()
		to := from.AddDate(0, 3, 0)
		var err error
		if a.From != "" {
			from, err = time.Parse(time.RFC3339, a.From)
			if err != nil {
				return "", errorsf("from must be RFC3339")
			}
		}
		if a.To != "" {
			to, err = time.Parse(time.RFC3339, a.To)
			if err != nil {
				return "", errorsf("to must be RFC3339")
			}
		}
		events, err := loadEvents(ctx, db, user.Login, from, to)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, e := range events {
			fmt.Fprintf(&b, "#%d [%s] %s — %s %s", e.ID, e.Calendar, e.Title, eventDayText(e.Start, e.StartDate), e.When())
			if e.Location != "" {
				fmt.Fprintf(&b, " @ %s", e.Location)
			}
			b.WriteByte('\n')
		}
		if b.Len() == 0 {
			return "No events in that range.", nil
		}
		return b.String(), nil
	}})
	web.Tool(mux, web.ToolDef{Name: "create_event", Description: "Create an event on one writable calendar. Times must include RFC3339 offsets. The event is queued for provider sync.", Schema: schema(map[string]any{"calendar_id": map[string]any{"type": "integer"}, "title": map[string]any{"type": "string"}, "start": map[string]any{"type": "string"}, "end": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}, "location": map[string]any{"type": "string"}, "recurrence": map[string]any{"type": "string", "description": "optional RRULE:FREQ=..."}}, "calendar_id", "title", "start", "end"), Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
		var a struct {
			CalendarID                                           int64 `json:"calendar_id"`
			Title, Start, End, Description, Location, Recurrence string
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", err
		}
		start, err := time.Parse(time.RFC3339, a.Start)
		if err != nil {
			return "", errorsf("start must be RFC3339")
		}
		end, err := time.Parse(time.RFC3339, a.End)
		if err != nil {
			return "", errorsf("end must be RFC3339")
		}
		id, err := createLocalEvent(ctx, db, user.Login, eventInput{CalendarID: a.CalendarID, Title: a.Title, Start: start, End: end, Description: a.Description, Location: a.Location, Timezone: start.Location().String(), Recurrence: a.Recurrence})
		if err != nil {
			return "", err
		}
		web.Changed(user.Login)
		queueEventAccount(ctx, db, user.Login, id, queue)
		return fmt.Sprintf("Created event #%d: %s", id, a.Title), nil
	}})
	web.Tool(mux, web.ToolDef{Name: "update_event", Description: "Update a whole event or recurring series. Times must include RFC3339 offsets.", Schema: schema(map[string]any{"id": map[string]any{"type": "integer"}, "calendar_id": map[string]any{"type": "integer"}, "title": map[string]any{"type": "string"}, "start": map[string]any{"type": "string"}, "end": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}, "location": map[string]any{"type": "string"}, "recurrence": map[string]any{"type": "string"}}, "id", "calendar_id", "title", "start", "end"), Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
		var a struct {
			ID          int64  `json:"id"`
			CalendarID  int64  `json:"calendar_id"`
			Title       string `json:"title"`
			Start       string `json:"start"`
			End         string `json:"end"`
			Description string `json:"description"`
			Location    string `json:"location"`
			Recurrence  string `json:"recurrence"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", err
		}
		start, err := time.Parse(time.RFC3339, a.Start)
		if err != nil {
			return "", errorsf("start must be RFC3339")
		}
		end, err := time.Parse(time.RFC3339, a.End)
		if err != nil {
			return "", errorsf("end must be RFC3339")
		}
		err = updateLocalEvent(ctx, db, user.Login, a.ID, eventInput{CalendarID: a.CalendarID, Title: a.Title, Start: start, End: end, Description: a.Description, Location: a.Location, Timezone: start.Location().String(), Recurrence: a.Recurrence})
		if err != nil {
			return "", err
		}
		web.Changed(user.Login)
		queueEventAccount(ctx, db, user.Login, a.ID, queue)
		return fmt.Sprintf("Updated event #%d.", a.ID), nil
	}})
	web.Tool(mux, web.ToolDef{Name: "delete_event", Description: "Delete a whole event or recurring series. Destructive — only on an explicit user request.", Schema: schema(map[string]any{"id": map[string]any{"type": "integer"}}, "id"), Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
		var a struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", err
		}
		var accountID int64
		if err := db.QueryRowContext(ctx, `SELECT c.account_id FROM events e JOIN calendars c ON c.id=e.calendar_id JOIN accounts x ON x.id=c.account_id WHERE e.id=? AND x.login=?`, a.ID, user.Login).Scan(&accountID); err != nil {
			return "", errorsf("event is unavailable")
		}
		if err := deleteLocalEvent(ctx, db, user.Login, a.ID); err != nil {
			return "", err
		}
		web.Changed(user.Login)
		_ = queue(user.Login, accountID)
		return fmt.Sprintf("Queued event #%d for deletion.", a.ID), nil
	}})
	web.Tool(mux, web.ToolDef{Name: "sync_accounts", Description: "Queue synchronization for all connected Google and iCloud calendar accounts.", Schema: schema(map[string]any{}), Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
		rows, err := db.QueryContext(ctx, `SELECT id FROM accounts WHERE login=? AND status!='disabled'`, user.Login)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				if err := queue(user.Login, id); err != nil {
					return "", err
				}
				n++
			}
		}
		return fmt.Sprintf("Queued synchronization for %d calendar account(s).", n), rows.Err()
	}})
	_ = syncer
}
func errorsf(s string) error { return fmt.Errorf("%s", s) }
func eventDayText(start time.Time, date string) string {
	if date != "" {
		return date
	}
	return start.Local().Format(time.RFC3339)
}
