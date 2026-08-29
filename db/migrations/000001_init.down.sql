-- +goose Down
DROP TABLE IF EXISTS controller_leases;
DROP TABLE IF EXISTS project_quota;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS processes;
DROP TABLE IF EXISTS operations;
DROP TABLE IF EXISTS sandboxes;
DROP TABLE IF EXISTS images;
DROP TABLE IF EXISTS role_bindings;
DROP TABLE IF EXISTS projects;
