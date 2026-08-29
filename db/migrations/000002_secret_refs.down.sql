-- +goose Down
ALTER TABLE sandboxes DROP COLUMN IF EXISTS secret_refs;
