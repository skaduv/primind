package repository

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// BF-03 T2: FinalizeMessage is the idempotent terminal write every path
// shares. These tests pin its predicate (only a still-generating row is
// touched) and its side effects (is_completed flips in the same statement).

func newFinalizeRepository(t *testing.T) (*messageRepository, *gorm.DB) {
	t.Helper()
	repo, db := newMessageRepositoryForForkTest(t)
	// One connection: SQLite's in-memory database is per connection, and the
	// concurrency test below wants every goroutine to see the same rows.
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	return repo, db
}

func loadRawMessage(t *testing.T, db *gorm.DB, id string) types.Message {
	t.Helper()
	var row types.Message
	require.NoError(t, db.Session(&gorm.Session{SkipHooks: true}).Where("id = ?", id).First(&row).Error)
	return row
}

func TestFinalizeMessageEndsAGeneratingTurnOnce(t *testing.T) {
	repo, db := newFinalizeRepository(t)
	ctx := context.Background()
	seedMessage(t, db, "a1", "s1", "assistant", time.Now())
	require.NoError(t, db.Model(&types.Message{}).Where("id = ?", "a1").Update("content", "partial").Error)

	applied, err := repo.FinalizeMessage(ctx, "s1", "a1",
		types.MessageStatusInterrupted, types.TurnErrorCodeProcessLost, "interrupted", nil)
	require.NoError(t, err)
	require.True(t, applied, "a generating row is finalized")

	row := loadRawMessage(t, db, "a1")
	assert.Equal(t, types.MessageStatusInterrupted, row.Status)
	assert.True(t, row.IsCompleted, "is_completed is set by the same statement (C1)")
	assert.Equal(t, types.TurnErrorCodeProcessLost, row.ErrorCode)
	assert.Equal(t, "interrupted", row.ErrorMessage)
	assert.Equal(t, "partial", row.Content, "a nil content pointer leaves the stored text alone")
	assert.False(t, row.UpdatedAt.IsZero())

	// A second finalizer — any status — must not relabel the ended turn.
	applied, err = repo.FinalizeMessage(ctx, "s1", "a1",
		types.MessageStatusFailed, types.TurnErrorCodeModelError, "late failure", nil)
	require.NoError(t, err)
	assert.False(t, applied, "an ended turn is not overwritten")
	row = loadRawMessage(t, db, "a1")
	assert.Equal(t, types.MessageStatusInterrupted, row.Status)
	assert.Equal(t, types.TurnErrorCodeProcessLost, row.ErrorCode)
}

func TestFinalizeMessageContentPointerKeepsPartialOutput(t *testing.T) {
	repo, db := newFinalizeRepository(t)
	seedMessage(t, db, "a1", "s1", "assistant", time.Now())
	partial := "answer so far"

	applied, err := repo.FinalizeMessage(context.Background(), "s1", "a1",
		types.MessageStatusStopped, "", "", &partial)
	require.NoError(t, err)
	require.True(t, applied)

	row := loadRawMessage(t, db, "a1")
	assert.Equal(t, partial, row.Content)
	assert.Equal(t, types.MessageStatusStopped, row.Status)
	assert.Empty(t, row.ErrorCode, "a stop is not an error")
}

// The success path, the error path, the deferred backstop, continue-stream
// and the reconciler can all try to end the same turn. Whichever gets there
// first decides; everyone else sees applied=false and the row keeps the
// winner's state.
func TestFinalizeMessageConcurrentWritersOnlyTheFirstApplies(t *testing.T) {
	repo, db := newFinalizeRepository(t)
	seedMessage(t, db, "a1", "s1", "assistant", time.Now())

	statuses := []types.MessageStatus{
		types.MessageStatusCompleted, types.MessageStatusFailed,
		types.MessageStatusStopped, types.MessageStatusInterrupted,
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, status := range statuses {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(status types.MessageStatus) {
				defer wg.Done()
				applied, err := repo.FinalizeMessage(context.Background(), "s1", "a1", status, "", "", nil)
				assert.NoError(t, err)
				if applied {
					wins.Add(1)
				}
			}(status)
		}
	}
	wg.Wait()

	assert.Equal(t, int32(1), wins.Load(), "exactly one writer ends the turn")
	row := loadRawMessage(t, db, "a1")
	assert.True(t, row.IsCompleted)
	assert.Contains(t, statuses, row.Status)
}

func TestFinalizeMessageIgnoresRowsItMustNotTouch(t *testing.T) {
	repo, db := newFinalizeRepository(t)
	ctx := context.Background()
	now := time.Now()
	seedMessage(t, db, "legacy-done", "s1", "assistant", now) // status='' but completed
	require.NoError(t, db.Model(&types.Message{}).Where("id = ?", "legacy-done").Update("is_completed", true).Error)
	seedMessage(t, db, "other-session", "s2", "assistant", now)
	seedMessage(t, db, "u1", "s1", "user", now)
	seedMessage(t, db, "already-failed", "s1", "assistant", now)
	require.NoError(t, db.Model(&types.Message{}).Where("id = ?", "already-failed").Updates(map[string]interface{}{
		"status": types.MessageStatusFailed, "is_completed": false,
	}).Error)

	for _, tc := range []struct {
		name                 string
		sessionID, messageID string
	}{
		{"completed before the column existed", "s1", "legacy-done"},
		{"message lives in another session", "s1", "other-session"},
		{"user row", "s1", "u1"},
		{"already terminal", "s1", "already-failed"},
		{"unknown id", "s1", "missing"},
	} {
		applied, err := repo.FinalizeMessage(ctx, tc.sessionID, tc.messageID,
			types.MessageStatusInterrupted, types.TurnErrorCodeProcessLost, "", nil)
		require.NoError(t, err, tc.name)
		assert.False(t, applied, tc.name)
	}
	assert.Equal(t, types.MessageStatusFailed, loadRawMessage(t, db, "already-failed").Status)
	assert.Empty(t, loadRawMessage(t, db, "other-session").Status, "a wrong session_id never matches")
}

func TestFinalizeMessageRejectsNonTerminalStatus(t *testing.T) {
	repo, _ := newFinalizeRepository(t)
	for _, status := range []types.MessageStatus{"", types.MessageStatusGenerating} {
		_, err := repo.FinalizeMessage(context.Background(), "s1", "a1", status, "", "", nil)
		require.Error(t, err, "status %q", status)
	}
	_, err := repo.FinalizeMessage(context.Background(), "", "a1", types.MessageStatusCompleted, "", "", nil)
	require.Error(t, err, "ids are required")
}

// seedGeneratingAt writes an assistant row that has been left generating since
// at. GORM stamps updated_at with the wall clock on insert, so the test's own
// timestamp has to be written explicitly afterwards.
func seedGeneratingAt(t *testing.T, db *gorm.DB, id, sessionID string, at time.Time) {
	t.Helper()
	seedMessage(t, db, id, sessionID, "assistant", at)
	require.NoError(t, db.Model(&types.Message{}).Where("id = ?", id).UpdateColumn("updated_at", at).Error)
}

// The reconciler's scan: only live assistant rows that are still generating
// and old enough are candidates, oldest first.
func TestListGeneratingAssistantMessagesBefore(t *testing.T) {
	repo, db := newFinalizeRepository(t)
	ctx := context.Background()
	// Rows are candidates when updated before the cutoff, i.e. when they have
	// been sitting unfinished for longer than the grace period.
	now := time.Now()
	cutoff := now.Add(-time.Hour)

	seedGeneratingAt(t, db, "stale-new", "s1", now.Add(-10*time.Minute)) // too recent
	seedGeneratingAt(t, db, "stale-2", "s2", now.Add(-2*time.Hour))      // candidate
	seedGeneratingAt(t, db, "stale-1", "s1", now.Add(-3*time.Hour))      // candidate, oldest
	seedGeneratingAt(t, db, "done", "s1", now.Add(-4*time.Hour))         // completed below
	seedGeneratingAt(t, db, "failed", "s1", now.Add(-4*time.Hour))       // terminal through status
	seedGeneratingAt(t, db, "deleted", "s1", now.Add(-4*time.Hour))      // soft deleted
	require.NoError(t, db.Model(&types.Message{}).Where("id = ?", "done").UpdateColumn("is_completed", true).Error)
	require.NoError(t, db.Model(&types.Message{}).Where("id = ?", "failed").Updates(map[string]interface{}{
		"status": types.MessageStatusFailed, "is_completed": true,
	}).Error)
	require.NoError(t, db.Model(&types.Message{}).Where("id = ?", "deleted").
		Update("deleted_at", now.Add(-time.Minute)).Error)

	got, err := repo.ListGeneratingAssistantMessagesBefore(ctx, cutoff, 200)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "stale-1", got[0].ID, "oldest first so the longest-abandoned turn is repaired first")
	assert.Equal(t, "stale-2", got[1].ID)
	assert.Empty(t, got[0].Content, "the scan projects identity columns only")

	page, err := repo.ListGeneratingAssistantMessagesBefore(ctx, cutoff, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "stale-1", page[0].ID)

	count, err := repo.CountGeneratingAssistantMessagesBefore(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	// Finalizing every candidate empties the scan.
	for _, m := range got {
		applied, err := repo.FinalizeMessage(ctx, m.SessionID, m.ID,
			types.MessageStatusInterrupted, types.TurnErrorCodeProcessLost, "", nil)
		require.NoError(t, err)
		require.True(t, applied)
	}
	none, err := repo.ListGeneratingAssistantMessagesBefore(ctx, cutoff, 200)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestMessageEffectiveStatusInfersLegacyRows(t *testing.T) {
	assert.Equal(t, types.MessageStatusCompleted, (&types.Message{IsCompleted: true}).EffectiveStatus())
	assert.Equal(t, types.MessageStatusGenerating, (&types.Message{}).EffectiveStatus())
	explicit := &types.Message{Status: types.MessageStatusGenerating, IsCompleted: true}
	assert.Equal(t, types.MessageStatusGenerating, explicit.EffectiveStatus(),
		"an explicit status always wins")
	for _, status := range []types.MessageStatus{
		types.MessageStatusCompleted, types.MessageStatusFailed,
		types.MessageStatusStopped, types.MessageStatusInterrupted,
	} {
		assert.True(t, (&types.Message{Status: status, IsCompleted: true}).IsTerminal(), status)
	}
	assert.False(t, (&types.Message{}).IsTerminal())
	assert.True(t, (*types.Message)(nil).IsTerminal())
}
