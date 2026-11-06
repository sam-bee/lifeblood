CREATE TABLE IF NOT EXISTS lifeblood_schema (version integer PRIMARY KEY);
INSERT INTO lifeblood_schema VALUES (1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS lifeblood_state (id integer PRIMARY KEY CHECK(id=1), oauth_epoch bigint NOT NULL DEFAULT 0);
INSERT INTO lifeblood_state(id) VALUES(1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS oauth_flows (
    state_hash bytea PRIMARY KEY, payload bytea NOT NULL, expires_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS oura_connections (
    id bigserial PRIMARY KEY,
    singleton boolean NOT NULL DEFAULT true UNIQUE CHECK(singleton),
    oura_user_id text NOT NULL UNIQUE,
    tokens bytea, token_expiry timestamptz, status text NOT NULL DEFAULT 'active',
    reconnect_reason text, generation bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (status IN ('active', 'reconnect_required', 'disconnected'))
);

CREATE TABLE IF NOT EXISTS oura_documents (
    connection_id bigint NOT NULL REFERENCES oura_connections ON DELETE CASCADE,
    collection text NOT NULL, source_id text NOT NULL, source_day date,
    payload jsonb, deleted boolean NOT NULL DEFAULT false,
    event_at timestamptz, fetched_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(connection_id, collection, source_id)
);
CREATE INDEX IF NOT EXISTS oura_documents_day_idx ON oura_documents(connection_id, collection, source_day, source_id);

CREATE TABLE IF NOT EXISTS sync_state (
    connection_id bigint NOT NULL REFERENCES oura_connections ON DELETE CASCADE,
    collection text NOT NULL, last_success_at timestamptz,
    coverage_start date, coverage_end date, status text NOT NULL DEFAULT 'pending',
    PRIMARY KEY(connection_id, collection)
);
CREATE TABLE IF NOT EXISTS webhook_subscriptions (
    subscription_id text PRIMARY KEY, collection text NOT NULL, event_type text NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
    id bigserial PRIMARY KEY,
    connection_id bigint NOT NULL REFERENCES oura_connections ON DELETE CASCADE,
    generation bigint NOT NULL, kind text NOT NULL, args jsonb NOT NULL,
    dedup_key text NOT NULL UNIQUE,
    status text NOT NULL DEFAULT 'queued', attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now(), leased_until timestamptz,
    last_error text, created_at timestamptz NOT NULL DEFAULT now(), completed_at timestamptz,
    CHECK(status IN ('queued','running','done','failed','cancelled'))
);
CREATE INDEX IF NOT EXISTS jobs_claim_idx ON jobs(status, available_at);
CREATE TABLE IF NOT EXISTS webhook_inbox (
    digest bytea PRIMARY KEY, connection_id bigint NOT NULL REFERENCES oura_connections ON DELETE CASCADE,
    received_at timestamptz NOT NULL DEFAULT now()
);
