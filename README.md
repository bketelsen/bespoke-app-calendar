# Calendar — a Bespoke app

A private calendar for the owner's Google and iCloud accounts, with a fast
local view backed by bounded provider synchronization.

This is an **unofficial, third-party app** for [Bespoke](https://github.com/bketelsen/bespoke). Nobody vetted it but its
author. Read [What it does with your stuff](#what-it-does-with-your-stuff)
before installing it, the same as any code you run as yourself.

## Install

From your Bespoke instance directory, with the platform at v0.13.0 or newer:

```sh
go tool bespoke add calendar
```

That pins this module, writes `apps/calendar/app.toml` with a free port, and
recompiles your instance stylesheet. Pin a version you have actually read with
`bespoke add calendar@v0.1.0`.

The directory must be named `calendar`: the slug names this app's database,
process, and subdomain, and the source picks it.

## What it does with your stuff

- **Stores** in its own SQLite database: your calendars, events, and account
  credentials. Credentials are encrypted with `BESPOKE_CALENDAR_KEY` (falling
  back to `BESPOKE_MAIL_KEY` so an instance can share one key with Mail).
- **Talks to** Google (`accounts.google.com`, `oauth2.googleapis.com`,
  `www.googleapis.com`) and Apple (`caldav.icloud.com`) — only the accounts you
  connect, and only to sync calendars and events.
- **Publishes** domain events and in-app notifications to your own platformd —
  the exact types, fields, and suppression rules are in
  [Events and automation](#events-and-automation). They never leave your
  instance.
- **Never** sends your calendar anywhere else.

Set `BESPOKE_CALENDAR_KEY` (32 random bytes, base64) and the Google OAuth client
values in `~/bespoke/env.d/calendar` on the app host — never the shared env file,
or every other app can read them.

Like every Bespoke app it runs as your user, so nothing above is *enforced* by
the platform — it is a description you can check against the source
([ADR-0031](https://github.com/bketelsen/bespoke/blob/main/docs/adr/0031-third-party-app-packages.md)).

## Events and automation

The app publishes domain events to your instance's platformd, which stores
them durably, shows their notifications in the platform inbox, and matches
automation rules against them
([ADR-0035](https://github.com/bketelsen/bespoke/blob/main/docs/adr/0035-durable-events-notifications-automations.md)).
Rules match on the exact `type` string and on condition paths under `data`, so
the names below are the contract. Events publish only after the local mutation
committed; a failed publish is logged and never rolls back or blocks the
mutation it describes.

| Type | Fires when | Notification | `data` fields |
| --- | --- | --- | --- |
| `calendar.event.imported` | a sync stores a remote event never seen locally | future-start only, first 10 per sync | `id`, `title`, `calendar`, `start` |
| `calendar.event.created` | the owner creates an event in the UI or via a tool | never — silent | `id`, `title`, `start`, `calendar_id`, `source` |
| `calendar.event.updated` | the owner edits an event in the UI or via a tool | never — silent | `id`, `title`, `start`, `calendar_id`, `source` |
| `calendar.account.connected` | Google OAuth completes or an iCloud account is added | never — silent | `account_id`, `provider`, `email` |
| `calendar.account.disconnected` | the owner disconnects an account | always | `account_id`, `provider`, `email` |
| `calendar.account.sync_failed` | an account's sync newly fails (non-error → error) | always | `account_id`, `provider`, `email`, `detail` |

Every event also carries the standard envelope: `id`, `type`, `subject_id`
(the event or account ID as a string), `occurred_at`, `data`, and a `source`
of `calendar`.

What a rule can rely on:

- **`calendar.event.imported` means a remote-origin event the app has never
  stored.** Each incoming event is checked against the local
  `(calendar_id, remote_id)` identity before the write, so refreshes of known
  events are updates, and a locally created event echoed back after its push —
  its remote identifier rewritten to the provider's — never fires an import.
- **An account's first sync is suppressed entirely.** Until an account has a
  successful sync on record, imports publish nothing at all, so connecting an
  account cannot flood the event log or inbox with up to three years of
  backfilled history.
- **Import notifications are capped; events are not.** Only imports whose
  start is in the future attach a notification — title `New event: <title>`,
  body is the local start time (or all-day date) plus the calendar name, path
  `/`, group key `calendar:imported` — capped at 10 per sync; every further
  import in that sync still publishes, silently. `data.start` is the UTC
  RFC 3339 start, or the plain `YYYY-MM-DD` date for all-day events.
- **`calendar.event.created` and `calendar.event.updated` are local-origin
  only.** They fire from the event form and the `create_event` and
  `update_event` tools, with `data.source` set to `ui` or `tool`; remote
  changes arriving by sync never masquerade as them.
- **`calendar.account.sync_failed` fires once per outage.** It publishes only
  on the transition into the `error` status, so the 10-minute background
  poller cannot re-notify while an account stays broken; it can fire again
  only after a successful sync resets the status. `detail` is the failure text
  truncated to 300 characters, and the notification (title
  `Calendar sync failing for <email>`) links to `/settings`.
- **`calendar.account.disconnected` notifies** (title
  `Calendar account disconnected`, path `/settings`) because disconnecting
  also deletes the account's locally synced calendars and events;
  `calendar.account.connected` is silent.

Notification titles are truncated to 120 bytes and bodies to 500, on rune
boundaries.

### Tools available to automation

Only the read-only tools opt into automation rules: `list_accounts`,
`list_calendars`, and `list_events`. The mutating tools — `create_event`,
`update_event`, `delete_event`, and `sync_accounts` — declare no automation
policy, so the platform refuses to run them from a rule. An automation can
read your calendar; it can never create, change, delete, or sync anything.

## Spec

The behavior this app is built to, unchanged from its original private build.

### Configuration

Calendar uses `BESPOKE_CALENDAR_KEY` for credential encryption, falling back to
the existing `BESPOKE_MAIL_KEY` during instance upgrades. Google uses
`BESPOKE_CALENDAR_GOOGLE_CLIENT_ID` and
`BESPOKE_CALENDAR_GOOGLE_CLIENT_SECRET`, with deliberate fallbacks to the Mail
OAuth client values, plus its own required
`BESPOKE_CALENDAR_GOOGLE_REDIRECT_URL`. Production runtime values belong in
`~/bespoke/env` on the app host; `deploy/deploy.env` configures deployment hosts
and is not copied into app services. The redirect URL must also be added to the
Google OAuth client's authorized redirect URIs. The Google Calendar API must be
enabled for the project.

### Usage

The owner opens Calendar to understand today, browse a day, week, or month,
search upcoming and past events, and create or change personal events without
visiting a provider-specific calendar UI. Calendar data is also available to
the Bespoke dashboard, global search, chat, and tools.

### Records

- **Account:** owner-scoped Google or iCloud connection, encrypted provider
  credential, health, and last successful sync.
- **Calendar:** provider calendar identifier, display name, color, selection,
  access level, and sync token/state.
- **Event:** provider event identifier, calendar, title, description, location,
  UTC start/end, source time zone, all-day dates, recurrence rules, recurrence
  identity, status, and local dirty/deleted state.

Provider identifiers and sync state are implementation details. Credentials are
encrypted at rest and never returned by pages, tools, logs, or chat context.

### Views and actions

- Upcoming agenda is the mobile-first home view.
- Day, week, and month views share date navigation and selected-calendar
  filters; wide week/month layouts scroll within their own region.
- Event detail supports create, edit, and explicit delete.
- Settings connects Google with OAuth and iCloud with Apple ID plus app-specific
  password, selects calendars, shows sync health, and offers manual sync.
- Search matches event title, description, and location.
- Recurring series are displayed from provider recurrence data. Editing or
  deleting a recurrence applies to the whole series in v1.
- Background and manual sync pull remote changes and push locally queued
  mutations. Provider failures retain queued work and expose actionable status.

All meaningful mutations are exposed as user-scoped tools and call
`web.Changed`. Tools include listing calendars/events, creating, updating, and
deleting events, connecting-status inspection, and queueing synchronization.
Destructive tools require an explicit user request.

### Provider behavior

- Google uses the Google Calendar API and OAuth. Authorization requests the
  calendar scope in addition to identity. Existing Mail Google credentials may
  be reused only through an explicit, secure shared credential mechanism; the
  calendar app otherwise owns its connection.
- iCloud uses CalDAV with an Apple ID and app-specific password. Existing Mail
  iCloud credentials may be copied only through an explicit owner action; the
  calendar app never reads Mail's database directly.
- Sync is bounded and incremental where the provider supports it. Initial sync
  covers one year in the past through two years in the future and preserves
  recurring series needed to populate that window.
- Times are stored in UTC while all-day dates remain date-only. UI and chat
  render in the owner's local time zone.

### Bespoke integration

- The dashboard card shows today's next event and remaining event count using
  cheap local queries.
- Live fragments refresh after local mutations and completed syncs.
- Global search returns event deep links.
- Chat receives a compact agenda and calendar summary.
- A `create_event` intent accepts natural cross-app event inputs. Existing apps
  are reviewed for reciprocal intents; no automatic event creation is added
  without an explicit owner action.

### Non-goals for v1

- Sending invitations, managing attendees, or responding to invitations.
- Reminders, travel-time alerts, or free/busy scheduling. The app does publish
  platform events with in-app notifications for sync failures, imported
  events, and account disconnects.
- Resource/room booking, attachments, conferencing administration, or task
  management.
- Per-occurrence editing or deletion within a recurring series.
- Automatic account sharing between app databases or silent credential import.

## Developing

`views/*_templ.go` is committed on purpose — instances build this module
straight out of the read-only Go module cache and never run `templ generate`
over it. After editing a `.templ`, run `just ui` and commit the output.

## License

MIT — see [LICENSE](LICENSE).
