package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const icloudCalDAV = "https://caldav.icloud.com"

type davMultiStatus struct {
	Responses []davResponse `xml:"response"`
}
type davResponse struct {
	Href  string `xml:"href"`
	Props []struct {
		Prop   davProp `xml:"prop"`
		Status string  `xml:"status"`
	} `xml:"propstat"`
}
type davProp struct {
	DisplayName      string  `xml:"displayname"`
	ETag             string  `xml:"getetag"`
	CalendarData     string  `xml:"calendar-data"`
	CurrentPrincipal davHref `xml:"current-user-principal"`
	CalendarHome     davHref `xml:"calendar-home-set"`
	ResourceType     struct {
		Calendar *struct{} `xml:"calendar"`
	} `xml:"resourcetype"`
	Color      string `xml:"calendar-color"`
	Privileges []struct {
		Privilege struct {
			Write *struct{} `xml:"write"`
		} `xml:"privilege"`
	} `xml:"current-user-privilege-set"`
}
type davHref struct {
	Href string `xml:"href"`
}

func davRequest(ctx context.Context, client *http.Client, cred accountCredential, method, endpoint, depth, body string) (davMultiStatus, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
	if err != nil {
		return davMultiStatus{}, err
	}
	req.SetBasicAuth("", "")
	req.SetBasicAuth(strings.TrimSpace(req.URL.User.Username()), cred.Password)
	req.URL.User = nil
	// The Apple ID is carried in the URL user field by callers so credentials
	// never enter logs or error strings.
	if depth != "" {
		req.Header.Set("Depth", depth)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	}
	resp, err := client.Do(req)
	if err != nil {
		return davMultiStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return davMultiStatus{}, fmt.Errorf("iCloud CalDAV: %s", resp.Status)
	}
	if method == http.MethodDelete || method == http.MethodPut {
		return davMultiStatus{}, nil
	}
	var out davMultiStatus
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func davURL(raw, email string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.User = url.User(email)
	return u.String(), nil
}

func (s *synchronizer) syncICloud(ctx context.Context, a syncAccount, cred accountCredential, imp *importPublisher) error {
	client := &http.Client{Timeout: 45 * time.Second}
	base, _ := davURL(icloudCalDAV+"/", a.Email)
	principalBody := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`
	ms, err := davRequest(ctx, client, cred, "PROPFIND", base, "0", principalBody)
	if err != nil {
		return err
	}
	principal := firstProp(ms).CurrentPrincipal.Href
	if principal == "" {
		return errors.New("iCloud did not return a calendar principal")
	}
	principalURL, err := resolveDAV(icloudCalDAV, principal, a.Email)
	if err != nil {
		return err
	}
	homeBody := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-home-set/></d:prop></d:propfind>`
	ms, err = davRequest(ctx, client, cred, "PROPFIND", principalURL, "0", homeBody)
	if err != nil {
		return err
	}
	home := firstProp(ms).CalendarHome.Href
	if home == "" {
		return errors.New("iCloud did not return a calendar home")
	}
	homeURL, err := resolveDAV(icloudCalDAV, home, a.Email)
	if err != nil {
		return err
	}
	listBody := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/"><d:prop><d:displayname/><d:resourcetype/><cs:calendar-color/><d:current-user-privilege-set/></d:prop></d:propfind>`
	ms, err = davRequest(ctx, client, cred, "PROPFIND", homeURL, "1", listBody)
	if err != nil {
		return err
	}
	if err = s.pushCalDAV(ctx, a, cred, client); err != nil {
		return err
	}
	for _, r := range ms.Responses {
		p := responseProp(r)
		if p.ResourceType.Calendar == nil {
			continue
		}
		href, err := resolveDAV(icloudCalDAV, r.Href, a.Email)
		if err != nil {
			continue
		}
		writable := false
		for _, x := range p.Privileges {
			if x.Privilege.Write != nil {
				writable = true
			}
		}
		name := p.DisplayName
		if name == "" {
			name = "Calendar"
		}
		color := strings.TrimSpace(p.Color)
		if len(color) >= 7 {
			color = color[:7]
		} else {
			color = "#2563eb"
		}
		var cid int64
		err = s.db.QueryRowContext(ctx, `INSERT INTO calendars(account_id,remote_id,href,name,color,writable) VALUES(?,?,?,?,?,?) ON CONFLICT(account_id,remote_id) DO UPDATE SET href=excluded.href,name=excluded.name,color=excluded.color,writable=excluded.writable RETURNING id`, a.ID, r.Href, stripUser(href), name, color, writable).Scan(&cid)
		if err != nil {
			return err
		}
		if err = s.pullCalDAV(ctx, cred, client, cid, href, name, imp); err != nil {
			return err
		}
	}
	return nil
}

func firstProp(ms davMultiStatus) davProp {
	if len(ms.Responses) == 0 {
		return davProp{}
	}
	return responseProp(ms.Responses[0])
}
func responseProp(r davResponse) davProp {
	for _, p := range r.Props {
		if strings.Contains(p.Status, " 200 ") {
			return p.Prop
		}
	}
	if len(r.Props) > 0 {
		return r.Props[0].Prop
	}
	return davProp{}
}
func resolveDAV(base, href, email string) (string, error) {
	b, _ := url.Parse(base)
	h, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	u := b.ResolveReference(h)
	u.User = url.User(email)
	return u.String(), nil
}
func stripUser(raw string) string {
	u, err := url.Parse(raw)
	if err == nil {
		u.User = nil
		return u.String()
	}
	return raw
}

func (s *synchronizer) pullCalDAV(ctx context.Context, cred accountCredential, client *http.Client, calendarID int64, href, calendar string, imp *importPublisher) error {
	from := time.Now().AddDate(-1, 0, 0).UTC().Format("20060102T150405Z")
	to := time.Now().AddDate(2, 0, 0).UTC().Format("20060102T150405Z")
	body := fmt.Sprintf(`<?xml version="1.0"?><c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop><c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="%s" end="%s"/></c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`, from, to)
	ms, err := davRequest(ctx, client, cred, "REPORT", href, "1", body)
	if err != nil {
		return err
	}
	for _, r := range ms.Responses {
		p := responseProp(r)
		if p.CalendarData == "" {
			continue
		}
		ev, err := parseICS(p.CalendarData)
		if err != nil {
			continue
		}
		ev.ETag = p.ETag
		ev.Href = r.Href
		if err = storeICSEvent(ctx, s.db, calendarID, ev, calendar, imp); err != nil {
			return err
		}
	}
	return nil
}

type icsEvent struct{ UID, Summary, Description, Location, Start, End, StartDate, EndDate, Timezone, Recurrence, Status, ETag, Href string }

func parseICS(raw string) (icsEvent, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\n ", "")
	raw = strings.ReplaceAll(raw, "\n\t", "")
	var e icsEvent
	inside := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "BEGIN:VEVENT" {
			inside = true
			continue
		}
		if line == "END:VEVENT" {
			break
		}
		if !inside {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		base := strings.ToUpper(strings.Split(key, ";")[0])
		val = unescapeICS(val)
		switch base {
		case "UID":
			e.UID = val
		case "SUMMARY":
			e.Summary = val
		case "DESCRIPTION":
			e.Description = val
		case "LOCATION":
			e.Location = val
		case "STATUS":
			e.Status = strings.ToLower(val)
		case "RRULE":
			e.Recurrence = "RRULE:" + val
		case "DTSTART":
			e.Timezone = icsParam(key, "TZID")
			if strings.Contains(key, "VALUE=DATE") {
				e.StartDate = val
			} else {
				e.Start = parseICSTime(val, e.Timezone).Format(time.RFC3339Nano)
			}
		case "DTEND":
			if strings.Contains(key, "VALUE=DATE") && len(val) == 8 {
				e.EndDate = val[:4] + "-" + val[4:6] + "-" + val[6:8]
			} else {
				e.End = parseICSTime(val, icsTimezone(key, e.Timezone)).Format(time.RFC3339Nano)
			}
		}
	}
	if e.StartDate != "" && len(e.StartDate) == 8 {
		e.StartDate = e.StartDate[:4] + "-" + e.StartDate[4:6] + "-" + e.StartDate[6:]
	}
	if e.UID == "" || e.Summary == "" {
		return e, errors.New("invalid calendar event")
	}
	return e, nil
}
func parseICSTime(value, timezone string) time.Time {
	if strings.HasSuffix(value, "Z") {
		if stamp, err := time.Parse("20060102T150405Z", value); err == nil {
			return stamp.UTC()
		}
	}
	location := time.Local
	if timezone != "" {
		if loaded, err := time.LoadLocation(timezone); err == nil {
			location = loaded
		}
	}
	for _, layout := range []string{"20060102T150405", "20060102T1504"} {
		if stamp, err := time.ParseInLocation(layout, value, location); err == nil {
			return stamp.UTC()
		}
	}
	return time.Time{}
}

func icsTimezone(key, fallback string) string {
	if timezone := icsParam(key, "TZID"); timezone != "" {
		return timezone
	}
	return fallback
}
func icsParam(key, want string) string {
	for _, p := range strings.Split(key, ";")[1:] {
		k, v, ok := strings.Cut(p, "=")
		if ok && strings.EqualFold(k, want) {
			return v
		}
	}
	return ""
}
func unescapeICS(v string) string {
	r := strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`)
	return r.Replace(v)
}
func escapeICS(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, ",", `\,`, ";", `\;`)
	return r.Replace(v)
}
func storeICSEvent(ctx context.Context, db *sql.DB, cid int64, e icsEvent, calendar string, imp *importPublisher) error {
	var start, end, startDate, endDate any
	if e.StartDate != "" {
		startDate = e.StartDate
		endDate = e.EndDate
	} else {
		start = e.Start
		end = e.End
	}
	// Rows already present — including locally created events whose push
	// rewrote remote_id and echoes back here — are updates, never imports.
	var existing int64
	known := db.QueryRowContext(ctx, `SELECT id FROM events WHERE calendar_id=? AND remote_id=?`, cid, e.UID).Scan(&existing) == nil
	res, err := db.ExecContext(ctx, `INSERT INTO events(calendar_id,remote_id,etag,title,description,location,start_at,end_at,start_date,end_date,timezone,recurrence,status,dirty) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,0) ON CONFLICT(calendar_id,remote_id) DO UPDATE SET etag=excluded.etag,title=excluded.title,description=excluded.description,location=excluded.location,start_at=excluded.start_at,end_at=excluded.end_at,start_date=excluded.start_date,end_date=excluded.end_date,timezone=excluded.timezone,recurrence=excluded.recurrence,status=excluded.status,updated_at=datetime('now') WHERE events.dirty=0`, cid, e.UID, e.ETag, e.Summary, e.Description, e.Location, start, end, startDate, endDate, e.Timezone, e.Recurrence, e.Status)
	if err != nil {
		return err
	}
	if !known {
		id, _ := res.LastInsertId()
		st, _ := time.Parse(time.RFC3339Nano, e.Start)
		imp.imported(ctx, id, e.Summary, calendar, st, e.StartDate)
	}
	return nil
}

func (s *synchronizer) pushCalDAV(ctx context.Context, a syncAccount, cred accountCredential, client *http.Client) error {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.remote_id,c.href,e.title,e.description,e.location,COALESCE(e.start_at,''),COALESCE(e.end_at,''),COALESCE(e.start_date,''),COALESCE(e.end_date,''),e.timezone,e.recurrence,e.deleted FROM events e JOIN calendars c ON c.id=e.calendar_id WHERE c.account_id=? AND e.dirty=1`, a.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var rid, href, title, desc, loc, start, end, sd, ed, tz, rec string
		var deleted bool
		if err := rows.Scan(&id, &rid, &href, &title, &desc, &loc, &start, &end, &sd, &ed, &tz, &rec, &deleted); err != nil {
			return err
		}
		uid := rid
		if strings.HasPrefix(uid, "local-") {
			uid += "@bespoke"
		}
		endpoint := strings.TrimRight(href, "/") + "/" + url.PathEscape(uid) + ".ics"
		endpoint, _ = davURL(endpoint, a.Email)
		if deleted {
			if strings.HasPrefix(rid, "local-") {
				_, err = s.db.ExecContext(ctx, "DELETE FROM events WHERE id=?", id)
			} else {
				_, err = davRequest(ctx, client, cred, http.MethodDelete, endpoint, "", "")
				if err == nil {
					_, err = s.db.ExecContext(ctx, "DELETE FROM events WHERE id=?", id)
				}
			}
			if err != nil {
				return err
			}
			continue
		}
		ics := buildICS(uid, title, desc, loc, start, end, sd, ed, tz, rec)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewBufferString(ics))
		if err != nil {
			return err
		}
		req.SetBasicAuth(a.Email, cred.Password)
		req.URL.User = nil
		req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("iCloud CalDAV write: %s", resp.Status)
		}
		_, err = s.db.ExecContext(ctx, `UPDATE events SET remote_id=?,dirty=0 WHERE id=?`, uid, id)
		if err != nil {
			return err
		}
	}
	return rows.Err()
}
func buildICS(uid, title, desc, loc, start, end, sd, ed, tz, rec string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Bespoke//Calendar//EN\r\nBEGIN:VEVENT\r\nUID:" + escapeICS(uid) + "\r\nDTSTAMP:" + time.Now().UTC().Format("20060102T150405Z") + "\r\nSUMMARY:" + escapeICS(title) + "\r\n")
	if desc != "" {
		b.WriteString("DESCRIPTION:" + escapeICS(desc) + "\r\n")
	}
	if loc != "" {
		b.WriteString("LOCATION:" + escapeICS(loc) + "\r\n")
	}
	if sd != "" {
		b.WriteString("DTSTART;VALUE=DATE:" + strings.ReplaceAll(sd, "-", "") + "\r\nDTEND;VALUE=DATE:" + strings.ReplaceAll(ed, "-", "") + "\r\n")
	} else {
		st, _ := time.Parse(time.RFC3339Nano, start)
		et, _ := time.Parse(time.RFC3339Nano, end)
		b.WriteString("DTSTART:" + st.UTC().Format("20060102T150405Z") + "\r\nDTEND:" + et.UTC().Format("20060102T150405Z") + "\r\n")
	}
	if rec != "" {
		b.WriteString(rec + "\r\n")
	}
	b.WriteString("END:VEVENT\r\nEND:VCALENDAR\r\n")
	return b.String()
}

func addICloudAccount(ctx context.Context, db *sql.DB, vault *credentialVault, login, email, password string) (int64, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	password = strings.ReplaceAll(strings.TrimSpace(password), "-", "")
	if email == "" || password == "" {
		return 0, errors.New("email and app-specific password are required")
	}
	cipher, nonce, err := vault.Seal(accountCredential{Password: password})
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.QueryRowContext(ctx, `INSERT INTO accounts(login,provider,email,credential_ciphertext,credential_nonce,status,status_detail) VALUES(?,'icloud',?,?,?,'pending','Ready for first sync') ON CONFLICT(login,email) DO UPDATE SET credential_ciphertext=excluded.credential_ciphertext,credential_nonce=excluded.credential_nonce,status='pending',status_detail='Credentials updated',updated_at=datetime('now') RETURNING id`, login, email, cipher, nonce).Scan(&id)
	return id, err
}
