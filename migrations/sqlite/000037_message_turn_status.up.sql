-- BF-03: terminal state of an assistant turn (same contract as the versioned
-- 000119 migration). Only adds columns with defaults; no backfill.
ALTER TABLE messages ADD COLUMN status       TEXT        NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN error_code   VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN error_message TEXT        NOT NULL DEFAULT '';

-- R2: only unfinished assistant rows can match the reconciler's scan.
CREATE INDEX IF NOT EXISTS idx_messages_generating
    ON messages (updated_at) WHERE is_completed = false;
