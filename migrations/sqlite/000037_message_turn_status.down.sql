DROP INDEX IF EXISTS idx_messages_generating;
ALTER TABLE messages DROP COLUMN error_message;
ALTER TABLE messages DROP COLUMN error_code;
ALTER TABLE messages DROP COLUMN status;
