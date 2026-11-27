-- BF-03: terminal state of an assistant turn.
--
-- Only adds columns with constant defaults. PostgreSQL (11+) fills them from
-- the catalog without rewriting the table, so this runs while the service is
-- up (C2). No backfill here: existing rows keep status='' and readers infer
-- their state from is_completed (EffectiveStatus). Abandoned rows are
-- repaired by the turn reconciler / the one-off backfill command (spec 4.6).

DO $$ BEGIN RAISE NOTICE '[Migration 000119] Adding messages.status / error_code / error_message'; END $$;

ALTER TABLE messages ADD COLUMN IF NOT EXISTS status       VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN IF NOT EXISTS error_code   VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN IF NOT EXISTS error_message TEXT        NOT NULL DEFAULT '';

DO $$ BEGIN RAISE NOTICE '[Migration 000119] messages turn status columns added'; END $$;
