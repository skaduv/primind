-- BF-03 §4.6: one-off repair of assistant turns left "generating" by an older build.
--
-- Prefer `primind-server -backfill-interrupted-turns` (same thing, but it skips
-- turns another replica is still running, via the live-run marker). This script
-- is the psql-only fallback for a deployment without shell access to the image.
--
-- Run each statement in turn and record the numbers in the spec (§10):
--   1. count before   2. update   3. count after (expected: 0 for idle turns)
--
-- The 1-hour window mirrors the stream TTL: before it, continue-stream can
-- still replay the turn's events and the turn may still be running.
SELECT count(*) AS interrupted_turns_before
FROM messages
WHERE role = 'assistant'
  AND is_completed = false
  AND (status IS NULL OR status IN ('', 'generating'))
  AND deleted_at IS NULL
  AND updated_at < now() - interval '1 hour';

UPDATE messages
SET status = 'interrupted',
    error_code = 'process_lost',
    error_message = '本次回答已中断，可以重新提问',
    is_completed = true,
    updated_at = now()
WHERE role = 'assistant'
  AND is_completed = false
  AND (status IS NULL OR status IN ('', 'generating'))
  AND deleted_at IS NULL
  AND updated_at < now() - interval '1 hour';

SELECT count(*) AS interrupted_turns_after
FROM messages
WHERE role = 'assistant'
  AND is_completed = false
  AND (status IS NULL OR status IN ('', 'generating'))
  AND deleted_at IS NULL
  AND updated_at < now() - interval '1 hour';
