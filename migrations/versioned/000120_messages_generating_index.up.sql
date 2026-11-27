-- BF-03 (R2): the turn reconciler and continue-stream self-heal look for
-- assistant rows that never reached a terminal state:
--   WHERE role = 'assistant' AND is_completed = false AND updated_at < cutoff
-- Only unfinished rows can match, so a partial index stays tiny however large
-- messages grows, and the scan no longer walks the whole table every 5 minutes.
-- Keep this as a single statement: CONCURRENTLY cannot run in a transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_generating
    ON messages (updated_at)
    WHERE is_completed = false;
