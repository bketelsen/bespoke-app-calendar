package views

import "time"

type Account struct {
	ID                                    int64
	Provider, Email, Status, StatusDetail string
	LastSyncAt                            *time.Time
}
type Calendar struct {
	ID                           int64
	Name, Color, Provider, Email string
	Selected, Writable           bool
}
type Event struct {
	ID, CalendarID                                int64
	Calendar, Color, Title, Description, Location string
	Start, End                                    time.Time
	StartDate, EndDate, Timezone, Recurrence      string
	Dirty                                         bool
}

func (e Event) AllDay() bool { return e.StartDate != "" }
func (e Event) When() string {
	if e.AllDay() {
		return "All day"
	}
	return e.Start.Local().Format("3:04 PM") + "–" + e.End.Local().Format("3:04 PM")
}

type PageData struct {
	View      string
	Date      time.Time
	Events    []Event
	Calendars []Calendar
	Selected  *Event
}
