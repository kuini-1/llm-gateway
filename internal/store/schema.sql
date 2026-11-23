CREATE TABLE IF NOT EXISTS projects (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL UNIQUE,
    monthly_budget_usd NUMERIC(12, 4) NOT NULL DEFAULT 0, -- 0 = no cap
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS api_keys (
    id                 TEXT PRIMARY KEY,
    project_id         TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,          -- who the key was issued to
    key_hash           TEXT NOT NULL UNIQUE,   -- sha256 of the secret; the secret itself is never stored
    prefix             TEXT NOT NULL,          -- first characters, to recognise a key in lists
    monthly_budget_usd NUMERIC(12, 4) NOT NULL DEFAULT 0,
    rpm                INTEGER NOT NULL DEFAULT 0, -- 0 = gateway default
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at         TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS requests (
    id                 TEXT PRIMARY KEY,
    ts                 TIMESTAMPTZ NOT NULL,
    project_id         TEXT NOT NULL,
    key_id             TEXT NOT NULL,
    endpoint           TEXT NOT NULL,           -- chat, embeddings
    route              TEXT NOT NULL,
    provider           TEXT NOT NULL DEFAULT '',
    model              TEXT NOT NULL DEFAULT '',
    stream             BOOLEAN NOT NULL DEFAULT false,
    status             TEXT NOT NULL,           -- ok, error
    http_status        INTEGER NOT NULL,
    error_kind         TEXT NOT NULL DEFAULT '',
    error              TEXT NOT NULL DEFAULT '',
    latency_ms         INTEGER NOT NULL,
    ttfb_ms            INTEGER,
    input_tokens       BIGINT NOT NULL DEFAULT 0,
    output_tokens      BIGINT NOT NULL DEFAULT 0,
    cache_read_tokens  BIGINT NOT NULL DEFAULT 0,
    cache_write_tokens BIGINT NOT NULL DEFAULT 0,
    cost_usd           NUMERIC(14, 8) NOT NULL DEFAULT 0,
    emb_inputs         INTEGER NOT NULL DEFAULT 0,
    emb_cache_hits     INTEGER NOT NULL DEFAULT 0,
    saved_usd          NUMERIC(14, 8) NOT NULL DEFAULT 0, -- list price of cached embedding tokens
    fallback_from      TEXT NOT NULL DEFAULT '',
    attempts           JSONB NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS requests_ts ON requests (ts DESC);
CREATE INDEX IF NOT EXISTS requests_project_ts ON requests (project_id, ts);
CREATE INDEX IF NOT EXISTS requests_key_ts ON requests (key_id, ts);
CREATE INDEX IF NOT EXISTS requests_errors ON requests (ts DESC) WHERE status = 'error';
