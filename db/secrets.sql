-- Register a project-scoped secretId so CreateSandbox may reference it in
-- secretRefs. This does not create the Secret Manager secret itself, and it
-- stores no payload -- it only authorizes this project to name this
-- secret_id; ignition-controller still resolves the value from Secret
-- Manager (project IGNITION_GCP_PROJECT) by that same secret_id at Pod
-- create. Until the Secret API ships, rows here are seed data, exactly like
-- db/rolebindings.sql seeds owners and images/rolebindings.sql seeds images.
--
-- Run against the `ignition` database through the Cloud SQL Auth Proxy after
-- the API has applied its schema (the secrets table must exist):
--
--   psql "postgres://ignition:PASS@127.0.0.1:5432/ignition?sslmode=disable" \
--        -v project=prj_dev \
--        -v secret_id=sec_model_token \
--        -f db/secrets.sql
--
-- Idempotent: re-running is a no-op if the row already exists.

\if :{?project}
\else
  \set project 'prj_dev'
\endif

\if :{?secret_id}
\else
  \echo 'secret_id is required: -v secret_id=<id>'
  \quit 1
\endif

INSERT INTO projects (id, name)
VALUES (:'project', :'project')
ON CONFLICT (id) DO NOTHING;

INSERT INTO secrets (project_id, secret_id)
VALUES (:'project', :'secret_id')
ON CONFLICT (project_id, secret_id) DO NOTHING;

TABLE secrets;
