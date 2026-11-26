package database

import (
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/require"
)

// BF-03 000037 adds the turn status columns to messages. An existing Lite
// database must upgrade without losing rows (every old row reads as
// status=”), and the down migration must remove exactly what up added so a
// rollback leaves no half-migrated schema behind.
func TestSQLiteMigrationsUpgradeV36AddsMessageTurnStatus(t *testing.T) {
	repoRoot := sqliteRepoRoot(t)
	chdirAndRestore(t, copySQLiteMigrationsThrough(t, repoRoot, 36))
	dbPath := filepath.Join(t.TempDir(), "upgrade-turn-status.db")
	opts := MigrationOptions{SQLiteDBPath: dbPath}
	require.NoError(t, RunMigrationsWithOptions("sqlite3://unused", opts))
	db := openSQLiteDB(t, dbPath)
	require.False(t, sqliteColumnExists(t, db, "messages", "status"))
	_, err := db.Exec(`INSERT INTO messages (id, session_id, request_id, role, content, is_completed)
		VALUES ('legacy-1', 'sess-1', 'req-1', 'assistant', 'old answer', 1)`)
	require.NoError(t, err)

	chdirAndRestore(t, repoRoot)
	require.NoError(t, RunMigrationsWithOptions("sqlite3://unused", opts))
	version, dirty := sqliteMigrationState(t, db)
	require.Equal(t, expectedSQLiteMigrationVersion, version)
	require.False(t, dirty)

	for _, column := range []string{"status", "error_code", "error_message"} {
		require.True(t, sqliteColumnExists(t, db, "messages", column), "messages.%s", column)
	}
	require.True(t, sqliteIndexExists(t, db, "idx_messages_generating"))
	var content string
	var completed bool
	var status, code, message string
	require.NoError(t, db.QueryRow(`SELECT content, is_completed, status, error_code, error_message
		FROM messages WHERE id = 'legacy-1'`).Scan(&content, &completed, &status, &code, &message))
	require.Equal(t, "old answer", content)
	require.True(t, completed)
	require.Empty(t, status, "old rows keep status='' and infer their state from is_completed")
	require.Empty(t, code)
	require.Empty(t, message)

	// A row written through the new columns round-trips.
	_, err = db.Exec(`INSERT INTO messages (id, session_id, request_id, role, content, is_completed, status, error_code, error_message)
		VALUES ('new-1', 'sess-1', 'req-2', 'assistant', '', 1, 'interrupted', 'process_lost', 'interrupted on purpose')`)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT status, error_code FROM messages WHERE id = 'new-1'`).Scan(&status, &code))
	require.Equal(t, "interrupted", status)
	require.Equal(t, "process_lost", code)

	// Down: the three columns and the partial index go away, the data stays.
	m, err := migrate.New("file://"+filepath.Join(repoRoot, "migrations", "sqlite"), "sqlite3://"+dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	require.NoError(t, m.Steps(-1))
	require.False(t, sqliteColumnExists(t, db, "messages", "status"))
	require.False(t, sqliteColumnExists(t, db, "messages", "error_code"))
	require.False(t, sqliteColumnExists(t, db, "messages", "error_message"))
	require.False(t, sqliteIndexExists(t, db, "idx_messages_generating"))
	require.NoError(t, db.QueryRow(`SELECT content FROM messages WHERE id = 'legacy-1'`).Scan(&content))
	require.Equal(t, "old answer", content)

	// Up again restores the schema.
	require.NoError(t, m.Steps(1))
	require.True(t, sqliteColumnExists(t, db, "messages", "status"))
	require.True(t, sqliteIndexExists(t, db, "idx_messages_generating"))
}
