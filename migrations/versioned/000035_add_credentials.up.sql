-- Description: Add credentials column to tenants for third-party provider credentials.
-- The column is retained for compatibility; the application no longer writes to it.
DO $$ BEGIN RAISE NOTICE '[Migration 000035] Adding credentials column to tenants'; END $$;

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS credentials JSONB DEFAULT NULL;
COMMENT ON COLUMN tenants.credentials IS 'Legacy third-party provider credentials column; retained for compatibility, no longer written by the application';
