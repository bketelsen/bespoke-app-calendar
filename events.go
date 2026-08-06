package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/bketelsen/bespoke/pkg/auth"
	"github.com/bketelsen/bespoke/pkg/events"
	"github.com/google/uuid"
)

// maxImportNotifications caps in-app notifications per sync; further imported
// events in the same sync are still published, just silently.
const maxImportNotifications = 10

// publishEvent sends one domain event to platformd after the app mutation has
// committed. Failures are logged and swallowed: publication never rolls back
// or blocks the mutation it describes.
func publishEvent(ctx context.Context, user auth.User, event events.Event) {
	event.ID = uuid.NewString()
	event.OccurredAt = time.Now().UTC()
	if err := events.New("calendar").Publish(ctx, user, event); err != nil {
		log.Printf("calendar event publish: %v", err)
	}
}

// clip truncates s to at most n bytes without splitting a UTF-8 rune
// (notification titles and bodies have byte limits).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// publishMutation reports a local create or update as a silent domain event.
func publishMutation(ctx context.Context, user auth.User, typ string, id int64, in eventInput, source string) {
	start := in.StartDate
	if start == "" {
		start = in.Start.UTC().Format(time.RFC3339)
	}
	publishEvent(ctx, user, events.Event{Type: typ, SubjectID: fmt.Sprint(id), Data: map[string]any{"id": id, "title": in.Title, "start": start, "calendar_id": in.CalendarID, "source": source}})
}

// publishAccountConnected reports a newly linked provider account as a silent
// domain event.
func publishAccountConnected(ctx context.Context, db *sql.DB, user auth.User, id int64) {
	var provider, email string
	if err := db.QueryRowContext(ctx, `SELECT provider,email FROM accounts WHERE id=?`, id).Scan(&provider, &email); err != nil {
		return
	}
	publishEvent(ctx, user, events.Event{Type: "calendar.account.connected", SubjectID: fmt.Sprint(id), Data: map[string]any{"account_id": id, "provider": provider, "email": email}})
}

// importPublisher publishes calendar.event.imported for remote-origin events
// the app has never stored before. An account's first sync is suppressed
// entirely so the initial backfill cannot flood the event log or inbox, and
// only near-term (future-start) events earn a notification, capped per sync.
type importPublisher struct {
	login     string
	firstSync bool
	notified  int
}

func (p *importPublisher) imported(ctx context.Context, id int64, title, calendar string, start time.Time, startDate string) {
	if p.firstSync {
		return
	}
	when := start
	display := start.Local().Format("Mon Jan 2 15:04")
	startText := start.UTC().Format(time.RFC3339)
	if startDate != "" {
		when, _ = time.ParseInLocation("2006-01-02", startDate, time.Local)
		display = startDate + " (all day)"
		startText = startDate
	}
	event := events.Event{Type: "calendar.event.imported", SubjectID: fmt.Sprint(id), Data: map[string]any{"id": id, "title": title, "calendar": calendar, "start": startText}}
	if when.After(time.Now()) && p.notified < maxImportNotifications {
		p.notified++
		event.Notification = &events.Notification{Title: clip("New event: "+title, 120), Body: clip(display+" · "+calendar, 500), AppSlug: "calendar", Path: "/", GroupKey: "calendar:imported"}
	}
	publishEvent(ctx, auth.User{Login: p.login}, event)
}
