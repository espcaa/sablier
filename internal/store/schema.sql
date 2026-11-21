CREATE TABLE IF NOT EXISTS events (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT, -- never reused, gaps mean tampering
    ts          INTEGER NOT NULL,                  -- unix ms, utc
    kind        TEXT    NOT NULL,
    session_id  TEXT    NOT NULL,
    project_id  TEXT    NOT NULL,
    harness     TEXT    NOT NULL,
    source      TEXT    NOT NULL,
    data        TEXT    NOT NULL DEFAULT '{}',     -- json payload
    uploaded_at INTEGER,                           -- unix ms, null until sent
    prev_hash   TEXT,                              -- chain, see event.ChainHash. null before the chain
    hash        TEXT
);

CREATE INDEX IF NOT EXISTS events_ts ON events (ts);
CREATE INDEX IF NOT EXISTS events_pending ON events (seq) WHERE uploaded_at IS NULL;

-- key/value, e.g. install salt
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- adapter state per session, shared by hook processes
CREATE TABLE IF NOT EXISTS session_state (
    session_id TEXT PRIMARY KEY,
    data       TEXT    NOT NULL, -- json
    updated_at INTEGER NOT NULL  -- unix ms
);

-- left over in databases from before project ids were readable
DROP TABLE IF EXISTS projects;
