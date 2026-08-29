-- +goose Up
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS secret_refs JSONB NOT NULL DEFAULT '[]';
