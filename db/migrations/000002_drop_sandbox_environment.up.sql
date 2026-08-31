-- Sandbox-level environment values are no longer part of the public contract.
-- Process-level environment values remain in processes.environment.
ALTER TABLE sandboxes DROP COLUMN IF EXISTS environment;
