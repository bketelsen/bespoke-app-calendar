PRAGMA foreign_keys = ON;

CREATE TABLE accounts (
    id INTEGER PRIMARY KEY,
    login TEXT NOT NULL,
    provider TEXT NOT NULL CHECK (provider IN ('gmail', 'icloud')),
    email TEXT NOT NULL,
    credential_ciphertext BLOB NOT NULL,
    credential_nonce BLOB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','syncing','healthy','error','disabled')),
    status_detail TEXT NOT NULL DEFAULT '',
    last_sync_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(login, email)
);

CREATE TABLE calendars (
    id INTEGER PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    remote_id TEXT NOT NULL,
    href TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    color TEXT NOT NULL DEFAULT '#2563eb',
    selected INTEGER NOT NULL DEFAULT 1,
    writable INTEGER NOT NULL DEFAULT 1,
    sync_token TEXT NOT NULL DEFAULT '',
    UNIQUE(account_id, remote_id)
);

CREATE TABLE events (
    id INTEGER PRIMARY KEY,
    calendar_id INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    remote_id TEXT NOT NULL,
    etag TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    location TEXT NOT NULL DEFAULT '',
    start_at TEXT,
    end_at TEXT,
    start_date TEXT,
    end_date TEXT,
    timezone TEXT NOT NULL DEFAULT '',
    recurrence TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'confirmed',
    dirty INTEGER NOT NULL DEFAULT 0,
    deleted INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(calendar_id, remote_id),
    CHECK ((start_at IS NOT NULL AND end_at IS NOT NULL) OR (start_date IS NOT NULL AND end_date IS NOT NULL))
);

CREATE TABLE oauth_states (
    state_hash BLOB PRIMARY KEY,
    login TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX events_timed_idx ON events(start_at, end_at);
CREATE INDEX events_allday_idx ON events(start_date, end_date);
CREATE INDEX accounts_login_idx ON accounts(login);
