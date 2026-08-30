-- Run as the Cloud SQL postgres superuser after creating role `ignition`.
-- The API and controller apply schema on startup, so this user needs CREATE.
GRANT ALL PRIVILEGES ON DATABASE ignition TO ignition;
-- Cloud SQL's postgres bootstrap user is not a true superuser. PostgreSQL
-- requires membership in the target owner role for ALTER ... OWNER.
GRANT ignition TO postgres;
ALTER DATABASE ignition OWNER TO ignition;
\c ignition
GRANT ALL ON SCHEMA public TO ignition;
ALTER SCHEMA public OWNER TO ignition;
REVOKE ignition FROM postgres;
