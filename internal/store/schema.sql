-- Baseline product schema for Cloud SQL PostgreSQL.
--
-- ignition-api embeds and applies this file on startup. The statements are
-- intentionally idempotent so a restart is safe, but this file is not a
-- migration: it describes the complete schema required by this version.
-- ignition-controller connects with OpenWithoutSchema and never runs DDL.
CREATE TABLE IF NOT EXISTS projects (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL DEFAULT '',
    create_time TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS role_bindings (
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    subject    TEXT NOT NULL,
    role       TEXT NOT NULL,
    PRIMARY KEY (project_id, subject)
);

CREATE TABLE IF NOT EXISTS images (
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    image_id   TEXT NOT NULL,
    state      TEXT NOT NULL DEFAULT 'READY',
    PRIMARY KEY (project_id, image_id)
);

CREATE TABLE IF NOT EXISTS sandboxes (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name          TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL,
    state_reason  TEXT NOT NULL DEFAULT '',
    image_id      TEXT NOT NULL,
    operation_id  TEXT NOT NULL DEFAULT '',
    generation    BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0),
    create_time   TIMESTAMPTZ NOT NULL,
    ready_time    TIMESTAMPTZ,
    finish_time   TIMESTAMPTZ,
    created_by    TEXT NOT NULL DEFAULT '',
    command       JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(command) = 'array'),
    working_dir   TEXT NOT NULL DEFAULT '',
    native_entrypoint BOOLEAN NOT NULL DEFAULT FALSE,
    resources     JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(resources) = 'object'),
    placement     JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(placement) = 'object'),
    timeouts      JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(timeouts) = 'object'),
    network       JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(network) = 'object'),
    labels        JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(labels) = 'object'),
    secret_refs   JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(secret_refs) = 'array'),
    UNIQUE (project_id, id)
);

CREATE INDEX IF NOT EXISTS sandboxes_project_state ON sandboxes (project_id, state);

CREATE INDEX IF NOT EXISTS sandboxes_create_time ON sandboxes (create_time);

-- Bounds ignition-controller's reconcile scan to sandboxes that still need a
-- pass: every non-terminal state, plus terminal rows the bounded
-- ListSandboxesAll window (finish_time recency) still includes for
-- crash-recovery cleanup. Without this, a reconcile pass costs O(all sandboxes
-- ever created) instead of O(active sandboxes).
CREATE INDEX IF NOT EXISTS sandboxes_active
    ON sandboxes (create_time)
    WHERE state NOT IN ('FINISHED', 'FAILED');

CREATE INDEX IF NOT EXISTS sandboxes_finish_time ON sandboxes (finish_time);

-- Project-scoped secret registry. Secret payloads never live here: this maps
-- an opaque secretId a client may reference in CreateSandbox.secretRefs to
-- the project that is allowed to use it. ignition-controller resolves the
-- actual value from Secret Manager at Pod create, keyed by this same
-- secret_id. Rows are seed data today, mirroring images (the Secret API is
-- specified but not exposed); see db/secrets.sql.
CREATE TABLE IF NOT EXISTS secrets (
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    secret_id  TEXT NOT NULL,
    PRIMARY KEY (project_id, secret_id)
);

CREATE TABLE IF NOT EXISTS operations (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    kind             TEXT NOT NULL,
    state            TEXT NOT NULL,
    resource_id      TEXT NOT NULL,
    create_time      TIMESTAMPTZ NOT NULL,
    start_time       TIMESTAMPTZ,
    end_time         TIMESTAMPTZ,
    trace_id         TEXT NOT NULL DEFAULT '',
    progress_message TEXT NOT NULL DEFAULT '',
    created_by       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS operations_project_resource ON operations (project_id, resource_id, create_time);

CREATE TABLE IF NOT EXISTS processes (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    sandbox_id          TEXT NOT NULL,
    state               TEXT NOT NULL,
    command             JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(command) = 'array'),
    working_directory   TEXT NOT NULL DEFAULT '',
    environment         JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(environment) = 'object'),
    pty                 BOOLEAN NOT NULL DEFAULT FALSE,
    create_time         TIMESTAMPTZ NOT NULL,
    start_time          TIMESTAMPTZ,
    exit_time           TIMESTAMPTZ,
    exit_code           INT,
    terminating_signal  TEXT NOT NULL DEFAULT '',
    created_by          TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (project_id, sandbox_id)
        REFERENCES sandboxes (project_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS processes_sandbox ON processes (project_id, sandbox_id, create_time);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    principal  TEXT NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    method     TEXT NOT NULL,
    route      TEXT NOT NULL,
    key        TEXT NOT NULL,
    hash       TEXT NOT NULL,
    status     INT NOT NULL DEFAULT 0,
    body       BYTEA NOT NULL DEFAULT '',
    done       BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (principal, project_id, method, route, key)
);

CREATE TABLE IF NOT EXISTS project_quota (
    project_id TEXT PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    active     INT NOT NULL DEFAULT 0 CHECK (active >= 0)
);

CREATE TABLE IF NOT EXISTS controller_leases (
    id         INT PRIMARY KEY CHECK (id = 1),
    holder     TEXT NOT NULL,
    until_time TIMESTAMPTZ NOT NULL
);
