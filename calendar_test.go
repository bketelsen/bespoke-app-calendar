package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseICSTimedEvent(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:abc-123\r\nSUMMARY:Planning\\, weekly\r\nDESCRIPTION:First line\\nSecond line\r\nLOCATION:Studio\r\nDTSTART:20260805T140000Z\r\nDTEND:20260805T150000Z\r\nRRULE:FREQ=WEEKLY\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	e, err := parseICS(raw)
	if err != nil {
		t.Fatal(err)
	}
	if e.UID != "abc-123" || e.Summary != "Planning, weekly" || e.Description != "First line\nSecond line" {
		t.Fatalf("unexpected event: %#v", e)
	}
	if e.Start != "2026-08-05T14:00:00Z" || e.End != "2026-08-05T15:00:00Z" {
		t.Fatalf("unexpected times: %s - %s", e.Start, e.End)
	}
	if e.Recurrence != "RRULE:FREQ=WEEKLY" {
		t.Fatalf("recurrence = %q", e.Recurrence)
	}
}

func TestParseICSTimedEventUsesTZID(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:local-time\r\nSUMMARY:Breakfast\r\nDTSTART;TZID=America/New_York:20260805T090000\r\nDTEND;TZID=America/New_York:20260805T100000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	e, err := parseICS(raw)
	if err != nil {
		t.Fatal(err)
	}
	if e.Start != "2026-08-05T13:00:00Z" || e.End != "2026-08-05T14:00:00Z" {
		t.Fatalf("TZID times = %s - %s", e.Start, e.End)
	}
}

func TestParseICSAllDayEvent(t *testing.T) {
	e, err := parseICS("BEGIN:VEVENT\nUID:day\nSUMMARY:Holiday\nDTSTART;VALUE=DATE:20261224\nDTEND;VALUE=DATE:20261225\nEND:VEVENT")
	if err != nil {
		t.Fatal(err)
	}
	if e.StartDate != "2026-12-24" || e.EndDate != "2026-12-25" {
		t.Fatalf("dates = %q - %q", e.StartDate, e.EndDate)
	}
}

func TestBuildICSEscapesUserText(t *testing.T) {
	start := time.Date(2026, 8, 5, 14, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	end := time.Date(2026, 8, 5, 15, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	raw := buildICS("uid", "One, two", "a\nb", "", start, end, "", "", "UTC", "")
	if !strings.Contains(raw, "SUMMARY:One\\, two") || !strings.Contains(raw, "DESCRIPTION:a\\nb") {
		t.Fatalf("ICS was not escaped:\n%s", raw)
	}
}

func TestEventInputValidation(t *testing.T) {
	in := eventInput{CalendarID: 1, Title: "Meeting", Start: time.Now(), End: time.Now().Add(time.Hour)}
	if err := in.validate(); err != nil {
		t.Fatal(err)
	}
	in.End = in.Start
	if err := in.validate(); err == nil {
		t.Fatal("expected invalid range")
	}
}

func TestParseDBTimeFromSQLite(t *testing.T) {
	stamp, err := parseDBTime("2026-08-04 17:31:59")
	if err != nil {
		t.Fatal(err)
	}
	if got := stamp.Format(time.RFC3339); got != "2026-08-04T17:31:59Z" {
		t.Fatalf("timestamp = %s", got)
	}
}
