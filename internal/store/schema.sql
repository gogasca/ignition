-- Product schema for Cloud SQL PostgreSQL. Embedded by ignition-api
-- (store.Open) and applied on API startup. ignition-controller uses
-- OpenWithoutMigrate and must not run DDL. Keep in sync with db/migrations/.
CREATE TABLE IF NOT EXISTS projects (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL DEFAULT '',
    create_time TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS role_bindings (
    project_id TEXT NOT NULL REFERENCES projects (id),
    subject    TEXT NOT NULL,
    role       TEXT NOT NULL,
    PRIMARY KEY (project_id, subject)
);

CREATE TABLE IF NOT EXISTS images (
    project_id TEXT NOT NULL REFERENCES projects (id),
    image_id   TEXT NOT NULL,
    state      TEXT NOT NULL DEFAULT 'READY',
    PRIMARY KEY (project_id, image_id)
);

CREATE TABLE IF NOT EXISTS sandboxes (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL REFERENCES projects (id),
    name          TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL,
    state_reason  TEXT NOT NULL DEFAULT '',
    image_id      TEXT NOT NULL,
    operation_id  TEXT NOT NULL DEFAULT '',
    generation    BIGINT NOT NULL DEFAULT 1,
    create_time   TIMESTAMPTZ NOT NULL,
    ready_time    TIMESTAMPTZ,
    finish_time   TIMESTAMPTZ,
    created_by    TEXT NOT NULL DEFAULT '',
    command       JSONB NOT NULL DEFAULT '[]',
    working_dir   TEXT NOT NULL DEFAULT '',
    resources     JSONB NOT NULL DEFAULT '{}',
    placement     JSONB NOT NULL DEFAULT '{}',
    timeouts      JSONB NOT NULL DEFAULT '{}',
    network       JSONB NOT NULL DEFAULT '{}',
    labels        JSONB NOT NULL DEFAULT '{}',
    secret_refs   JSONB NOT NULL DEFAULT '[]'
);

CREATE INDEX IF NOT EXISTS sandboxes_project_id ON sandboxes (project_id, id);

CREATE INDEX IF NOT EXISTS sandboxes_project_state ON sandboxes (project_id, state);

CREATE INDEX IF NOT EXISTS sandboxes_create_time ON sandboxes (create_time);

ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS secret_refs JSONB NOT NULL DEFAULT '[]';
ALTER TABLE sandboxes DROP COLUMN IF EXISTS environment;

CREATE TABLE IF NOT EXISTS operations (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL,
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
    project_id          TEXT NOT NULL,
    sandbox_id          TEXT NOT NULL REFERENCES sandboxes (id),
    state               TEXT NOT NULL,
    command             JSONB NOT NULL DEFAULT '[]',
    working_directory   TEXT NOT NULL DEFAULT '',
    environment         JSONB NOT NULL DEFAULT '{}',
    pty                 BOOLEAN NOT NULL DEFAULT FALSE,
    create_time         TIMESTAMPTZ NOT NULL,
    start_time          TIMESTAMPTZ,
    exit_time           TIMESTAMPTZ,
    exit_code           INT,
    terminating_signal  TEXT NOT NULL DEFAULT '',
    created_by          TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS processes_sandbox ON processes (project_id, sandbox_id, create_time);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    principal  TEXT NOT NULL,
    project_id TEXT NOT NULL,
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
    project_id TEXT PRIMARY KEY,
    active     INT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS controller_leases (
    id         INT PRIMARY KEY CHECK (id = 1),
    holder     TEXT NOT NULL,
    until_time TIMESTAMPTZ NOT NULL
);
