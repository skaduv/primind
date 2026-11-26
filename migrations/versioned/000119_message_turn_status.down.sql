-- BF-03: drop the turn status columns. Only run once no deployment reads
-- them (spec §7 rollback). is_completed is untouched and stays correct on
-- its own.
ALTER TABLE messages
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS error_code,
    DROP COLUMN IF EXISTS error_message;
