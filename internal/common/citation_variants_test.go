package common

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/primind/primind/internal/logger"
	"github.com/stretchr/testify/require"
)

// captureLogs routes the application logger into a buffer for one test. Tests
// in a package run sequentially, so the global writer is safe to swap.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	logger.SetOutput(&buf)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })
	return &buf
}

func TestLogCitationVariantsWritesOneStructuredLinePerVariant(t *testing.T) {
	logs := captureLogs(t)

	ctx := context.Background()
	LogCitationVariants(ctx, nil, "model-1", "sess-1", "msg-1")
	LogCitationVariants(ctx, map[string]int{}, "model-1", "sess-1", "msg-1")
	LogCitationVariants(ctx, map[string]int{"square_double_quoted": 0, "": 2}, "model-1", "sess-1", "msg-1")
	require.Empty(t, logs.String(), "nothing to report must mean no log line")

	LogCitationVariants(ctx, map[string]int{
		"truncated_bracket":    1,
		"square_double_quoted": 3,
	}, "model-1", "sess-1", "msg-1")

	out := logs.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 2)
	// Variants are sorted so the order is stable across runs.
	require.Contains(t, lines[0], "variant=square_double_quoted")
	require.Contains(t, lines[0], "count=3")
	require.Contains(t, lines[0], "model_id=model-1")
	require.Contains(t, lines[0], "session_id=sess-1")
	require.Contains(t, lines[0], "message_id=msg-1")
	require.Contains(t, lines[0], CitationVariantLogMessage)
	require.Contains(t, lines[1], "variant=truncated_bracket")
	require.Contains(t, lines[1], "count=1")
	require.NotContains(t, out, "count=0")
	require.NotContains(t, out, "variant= ")
}

func TestLogCitationVariantsSanitizesIdentifiers(t *testing.T) {
	logs := captureLogs(t)

	LogCitationVariants(context.Background(), map[string]int{"square_bare": 1},
		"model\n forged INFO line", "sess\t1", "msg\r\n2")

	out := logs.String()
	require.Len(t, strings.Split(strings.TrimSpace(out), "\n"), 1, "an identifier must not forge a log line")
	// SanitizeForLog replaces each control character with a space.
	require.Contains(t, out, "model_id=model  forged INFO line")
	require.Contains(t, out, "session_id=sess 1")
	require.Contains(t, out, "message_id=msg  2")
}
