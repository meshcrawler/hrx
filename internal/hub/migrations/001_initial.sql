CREATE TABLE hrx_hub_hosts (
    host_id text PRIMARY KEY,
    name text NOT NULL,
    token_digest text NOT NULL UNIQUE,
    prefixes jsonb NOT NULL,
    revoked boolean NOT NULL DEFAULT false,
    last_sequence bigint NOT NULL DEFAULT 0,
    last_heartbeat timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE hrx_hub_runs (
    host_id text NOT NULL REFERENCES hrx_hub_hosts(host_id),
    run_id text NOT NULL,
    sequence bigint NOT NULL CHECK (sequence > 0),
    record jsonb NOT NULL,
    ended boolean NOT NULL DEFAULT false,
    live_seen_at timestamptz,
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (host_id, run_id)
);

CREATE TABLE hrx_hub_receipts (
    host_id text NOT NULL REFERENCES hrx_hub_hosts(host_id),
    sequence bigint NOT NULL CHECK (sequence > 0),
    payload_digest text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (host_id, sequence)
);

CREATE TABLE hrx_hub_leases (
    identity text PRIMARY KEY,
    host_id text NOT NULL REFERENCES hrx_hub_hosts(host_id),
    run_id text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    expires_at timestamptz NOT NULL
);

-- Acquisition receipts fence delayed retries even after another run has owned
-- the identity. A run ID cannot be reused as a new ownership incarnation.
CREATE TABLE hrx_hub_claims (
    host_id text NOT NULL REFERENCES hrx_hub_hosts(host_id),
    run_id text NOT NULL,
    identity text NOT NULL,
    generation bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(host_id, run_id)
);

CREATE INDEX hrx_hub_runs_received ON hrx_hub_runs(received_at);
CREATE INDEX hrx_hub_receipts_received ON hrx_hub_receipts(received_at);
CREATE INDEX hrx_hub_leases_owner ON hrx_hub_leases(host_id, run_id);
