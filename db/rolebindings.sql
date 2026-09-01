-- Seed project role bindings for ignition-api.
--
-- Run against the `ignition` database through the Cloud SQL Auth Proxy after
-- the API has applied its schema (the role_bindings table must exist):
--
--   psql "postgres://ignition:PASS@127.0.0.1:5432/ignition?sslmode=disable" \
--        -v project=prj_dev \
--        -v prober_sa=ignition-prober@ignition-dev.iam.gserviceaccount.com \
--        -v owner_email=you@ignition.dev \
--        -f db/rolebindings.sql
--
-- Idempotent: re-running updates the role of an existing binding.
--
-- Subjects: a Google account email, a service-account email, or `domain:<fqdn>`
-- to bind every Workspace user in a domain. The prober needs sandbox
-- create/get/exec and terminate-own, which the `developer` role grants.
--
-- Alternative for the first human owner: set IGNITION_BOOTSTRAP_PROJECT and
-- IGNITION_BOOTSTRAP_ADMIN on ignition-api; it seeds one owner when the
-- project has none.

\if :{?project}
\else
  \set project 'prj_dev'
\endif

INSERT INTO projects (id, name)
VALUES (:'project', :'project')
ON CONFLICT (id) DO NOTHING;

\if :{?prober_sa}
INSERT INTO role_bindings (project_id, subject, role)
VALUES (:'project', :'prober_sa', 'developer')
ON CONFLICT (project_id, subject) DO UPDATE SET role = EXCLUDED.role;
\endif

\if :{?owner_email}
INSERT INTO role_bindings (project_id, subject, role)
VALUES (:'project', :'owner_email', 'owner')
ON CONFLICT (project_id, subject) DO UPDATE SET role = EXCLUDED.role;
\endif

TABLE role_bindings;
