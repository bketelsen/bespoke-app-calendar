package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const googleCalendarScope = "https://www.googleapis.com/auth/calendar"

type googleConfig struct {
	ClientID, ClientSecret, RedirectURL string
	HTTP                                *http.Client
}
type googleToken struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	ErrorDescription string `json:"error_description"`
}

func loadGoogleConfig() (*googleConfig, error) {
	get := func(primary, fallback string) string {
		if v := strings.TrimSpace(os.Getenv(primary)); v != "" {
			return v
		}
		return strings.TrimSpace(os.Getenv(fallback))
	}
	c := &googleConfig{ClientID: get("BESPOKE_CALENDAR_GOOGLE_CLIENT_ID", "BESPOKE_MAIL_GOOGLE_CLIENT_ID"), ClientSecret: get("BESPOKE_CALENDAR_GOOGLE_CLIENT_SECRET", "BESPOKE_MAIL_GOOGLE_CLIENT_SECRET"), RedirectURL: strings.TrimSpace(os.Getenv("BESPOKE_CALENDAR_GOOGLE_REDIRECT_URL")), HTTP: &http.Client{Timeout: 30 * time.Second}}
	if c.ClientID == "" || c.ClientSecret == "" || c.RedirectURL == "" {
		return nil, errors.New("calendar Google OAuth is not configured")
	}
	u, err := url.Parse(c.RedirectURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("calendar Google redirect URL must be absolute HTTPS")
	}
	return c, nil
}

func beginGoogleOAuth(ctx context.Context, db *sql.DB, login string, c *googleConfig) (string, error) {
	if c == nil {
		return "", errors.New("google OAuth is unavailable")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(state))
	if _, err := db.ExecContext(ctx, `INSERT INTO oauth_states(state_hash,login,expires_at) VALUES(?,?,datetime('now','+10 minutes'))`, hash[:], login); err != nil {
		return "", err
	}
	v := url.Values{"client_id": {c.ClientID}, "redirect_uri": {c.RedirectURL}, "response_type": {"code"}, "scope": {googleCalendarScope + " openid email"}, "access_type": {"offline"}, "prompt": {"consent"}, "state": {state}}
	return "https://accounts.google.com/o/oauth2/v2/auth?" + v.Encode(), nil
}

func finishGoogleOAuth(ctx context.Context, db *sql.DB, vault *credentialVault, c *googleConfig, login, state, code string) (int64, error) {
	if vault == nil || c == nil {
		return 0, errors.New("google OAuth is unavailable")
	}
	hash := sha256.Sum256([]byte(state))
	var owner string
	if err := db.QueryRowContext(ctx, `DELETE FROM oauth_states WHERE state_hash=? AND expires_at>datetime('now') RETURNING login`, hash[:]).Scan(&owner); err != nil || owner != login {
		return 0, errors.New("google authorization expired or did not match")
	}
	tok, err := c.exchange(ctx, url.Values{"client_id": {c.ClientID}, "client_secret": {c.ClientSecret}, "redirect_uri": {c.RedirectURL}, "grant_type": {"authorization_code"}, "code": {code}})
	if err != nil {
		return 0, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://openidconnect.googleapis.com/v1/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var profile struct {
		Email string `json:"email"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&profile); err != nil || profile.Email == "" {
		return 0, errors.New("google did not return an email address")
	}
	cred := accountCredential{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, TokenExpiry: time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)}
	ciphertext, nonce, err := vault.Seal(cred)
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.QueryRowContext(ctx, `INSERT INTO accounts(login,provider,email,credential_ciphertext,credential_nonce,status,status_detail) VALUES(?,'gmail',?,?,?,'pending','Ready for first sync') ON CONFLICT(login,email) DO UPDATE SET credential_ciphertext=excluded.credential_ciphertext,credential_nonce=excluded.credential_nonce,status='pending',status_detail='Authorization renewed',updated_at=datetime('now') RETURNING id`, login, strings.ToLower(profile.Email), ciphertext, nonce).Scan(&id)
	return id, err
}

func (c *googleConfig) exchange(ctx context.Context, v url.Values) (googleToken, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return googleToken{}, err
	}
	defer resp.Body.Close()
	var t googleToken
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return t, err
	}
	if resp.StatusCode != 200 || t.AccessToken == "" {
		return t, fmt.Errorf("google authorization failed: %s", t.ErrorDescription)
	}
	return t, nil
}

func (s *synchronizer) freshGoogle(ctx context.Context, a syncAccount, cred accountCredential) (accountCredential, error) {
	expiry, _ := time.Parse(time.RFC3339Nano, cred.TokenExpiry)
	if cred.AccessToken != "" && time.Until(expiry) > time.Minute {
		return cred, nil
	}
	if s.google == nil || cred.RefreshToken == "" {
		return cred, errors.New("google authorization needs renewal")
	}
	tok, err := s.google.exchange(ctx, url.Values{"client_id": {s.google.ClientID}, "client_secret": {s.google.ClientSecret}, "grant_type": {"refresh_token"}, "refresh_token": {cred.RefreshToken}})
	if err != nil {
		return cred, err
	}
	cred.AccessToken = tok.AccessToken
	cred.TokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	cipher, nonce, err := s.vault.Seal(cred)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE accounts SET credential_ciphertext=?,credential_nonce=? WHERE id=?`, cipher, nonce, a.ID)
	}
	return cred, err
}

type googleCalendarList struct {
	Items []struct {
		ID, Summary, BackgroundColor, AccessRole string
		Selected                                 bool
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}
type googleEventList struct {
	Items         []googleEvent `json:"items"`
	NextPageToken string        `json:"nextPageToken"`
}
type googleEvent struct {
	ID, Summary, Description, Location, Status, RecurrenceID string
	Start, End                                               struct{ DateTime, Date, TimeZone string }
	Recurrence                                               []string `json:"recurrence"`
}

func (s *synchronizer) googleRequest(ctx context.Context, cred accountCredential, method, endpoint string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.google.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("google Calendar: %s: %s", resp.Status, string(b))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
	}
	return nil
}

func (s *synchronizer) syncGoogle(ctx context.Context, a syncAccount, cred accountCredential, imp *importPublisher) error {
	cred, err := s.freshGoogle(ctx, a, cred)
	if err != nil {
		return err
	}
	if err = s.pushGoogle(ctx, a, cred); err != nil {
		return err
	}
	var list googleCalendarList
	if err = s.googleRequest(ctx, cred, http.MethodGet, "https://www.googleapis.com/calendar/v3/users/me/calendarList?maxResults=250", nil, &list); err != nil {
		return err
	}
	for _, rc := range list.Items {
		var cid int64
		err = s.db.QueryRowContext(ctx, `INSERT INTO calendars(account_id,remote_id,name,color,selected,writable) VALUES(?,?,?,?,?,?) ON CONFLICT(account_id,remote_id) DO UPDATE SET name=excluded.name,color=excluded.color,writable=excluded.writable RETURNING id`, a.ID, rc.ID, rc.Summary, rc.BackgroundColor, rc.Selected, rc.AccessRole == "owner" || rc.AccessRole == "writer").Scan(&cid)
		if err != nil {
			return err
		}
		if err = s.pullGoogleEvents(ctx, cred, cid, rc.ID, rc.Summary, imp); err != nil {
			return err
		}
	}
	return nil
}

func (s *synchronizer) pullGoogleEvents(ctx context.Context, cred accountCredential, calendarID int64, remote, calendar string, imp *importPublisher) error {
	from := time.Now().AddDate(-1, 0, 0).UTC().Format(time.RFC3339)
	to := time.Now().AddDate(2, 0, 0).UTC().Format(time.RFC3339)
	token := ""
	for {
		endpoint := "https://www.googleapis.com/calendar/v3/calendars/" + url.PathEscape(remote) + "/events?singleEvents=true&showDeleted=true&maxResults=2500&timeMin=" + url.QueryEscape(from) + "&timeMax=" + url.QueryEscape(to)
		if token != "" {
			endpoint += "&pageToken=" + url.QueryEscape(token)
		}
		var page googleEventList
		if err := s.googleRequest(ctx, cred, http.MethodGet, endpoint, nil, &page); err != nil {
			return err
		}
		for _, e := range page.Items {
			if e.Status == "cancelled" {
				_, _ = s.db.ExecContext(ctx, `DELETE FROM events WHERE calendar_id=? AND remote_id=? AND dirty=0`, calendarID, e.ID)
				continue
			}
			if err := storeGoogleEvent(ctx, s.db, calendarID, e, calendar, imp); err != nil {
				return err
			}
		}
		token = page.NextPageToken
		if token == "" {
			return nil
		}
	}
}

func storeGoogleEvent(ctx context.Context, db *sql.DB, calendarID int64, e googleEvent, calendar string, imp *importPublisher) error {
	var st time.Time
	var start, end, startDate, endDate any
	if e.Start.Date != "" {
		startDate = e.Start.Date
		endDate = e.End.Date
	} else {
		var err error
		st, err = time.Parse(time.RFC3339, e.Start.DateTime)
		if err != nil {
			return nil
		}
		et, err := time.Parse(time.RFC3339, e.End.DateTime)
		if err != nil {
			return nil
		}
		start = st.UTC().Format(time.RFC3339Nano)
		end = et.UTC().Format(time.RFC3339Nano)
	}
	// Rows already present — including locally created events whose push
	// rewrote remote_id and echoes back here — are updates, never imports.
	var existing int64
	known := db.QueryRowContext(ctx, `SELECT id FROM events WHERE calendar_id=? AND remote_id=?`, calendarID, e.ID).Scan(&existing) == nil
	res, err := db.ExecContext(ctx, `INSERT INTO events(calendar_id,remote_id,title,description,location,start_at,end_at,start_date,end_date,timezone,recurrence,status,dirty) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0) ON CONFLICT(calendar_id,remote_id) DO UPDATE SET title=excluded.title,description=excluded.description,location=excluded.location,start_at=excluded.start_at,end_at=excluded.end_at,start_date=excluded.start_date,end_date=excluded.end_date,timezone=excluded.timezone,recurrence=excluded.recurrence,status=excluded.status,updated_at=datetime('now') WHERE events.dirty=0`, calendarID, e.ID, e.Summary, e.Description, e.Location, start, end, startDate, endDate, e.Start.TimeZone, strings.Join(e.Recurrence, "\n"), e.Status)
	if err != nil {
		return err
	}
	if !known {
		id, _ := res.LastInsertId()
		imp.imported(ctx, id, e.Summary, calendar, st, e.Start.Date)
	}
	return nil
}

func (s *synchronizer) pushGoogle(ctx context.Context, a syncAccount, cred accountCredential) error {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.remote_id,c.remote_id,e.title,e.description,e.location,COALESCE(e.start_at,''),COALESCE(e.end_at,''),COALESCE(e.start_date,''),COALESCE(e.end_date,''),e.timezone,e.recurrence,e.deleted FROM events e JOIN calendars c ON c.id=e.calendar_id WHERE c.account_id=? AND e.dirty=1`, a.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var rid, cal, title, desc, loc, start, end, sd, ed, tz, rec string
		var deleted bool
		if err := rows.Scan(&id, &rid, &cal, &title, &desc, &loc, &start, &end, &sd, &ed, &tz, &rec, &deleted); err != nil {
			return err
		}
		endpoint := "https://www.googleapis.com/calendar/v3/calendars/" + url.PathEscape(cal) + "/events"
		method := http.MethodPost
		if !strings.HasPrefix(rid, "local-") {
			endpoint += "/" + url.PathEscape(rid)
			method = http.MethodPut
		}
		if deleted {
			if strings.HasPrefix(rid, "local-") {
				_, err = s.db.ExecContext(ctx, "DELETE FROM events WHERE id=?", id)
			} else {
				err = s.googleRequest(ctx, cred, http.MethodDelete, endpoint, nil, nil)
				if err == nil {
					_, err = s.db.ExecContext(ctx, "DELETE FROM events WHERE id=?", id)
				}
			}
			if err != nil {
				return err
			}
			continue
		}
		payload := map[string]any{"summary": title, "description": desc, "location": loc}
		if sd != "" {
			payload["start"] = map[string]string{"date": sd}
			payload["end"] = map[string]string{"date": ed}
		} else {
			payload["start"] = map[string]string{"dateTime": start, "timeZone": tz}
			payload["end"] = map[string]string{"dateTime": end, "timeZone": tz}
		}
		if rec != "" {
			payload["recurrence"] = strings.Split(rec, "\n")
		}
		b, _ := json.Marshal(payload)
		var saved googleEvent
		if err = s.googleRequest(ctx, cred, method, endpoint, strings.NewReader(string(b)), &saved); err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `UPDATE events SET remote_id=?,dirty=0 WHERE id=?`, saved.ID, id)
		if err != nil {
			return err
		}
	}
	return rows.Err()
}
