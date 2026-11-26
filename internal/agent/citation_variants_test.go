package agent

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/primind/primind/internal/agent/tools"
	"github.com/primind/primind/internal/common"
	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/modelcontext"
	"github.com/primind/primind/internal/types"
	"github.com/stretchr/testify/require"
)

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func citationVariantLogLines(logs *lockedLogBuffer) []string {
	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, common.CitationVariantLogMessage) {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestAgentTurnExpandsBracketedCitationsAndLogsVariant drives one whole Agent
// turn (BF-01 AC1 / AC6 on the Agent path): the model writes the
// Markdown-style [ref id="cN"] split across provider chunks, the answer ends
// up with a citation card and no marker, and the turn's closing log line
// names the model, the message and the variant.
func TestAgentTurnExpandsBracketedCitationsAndLogsVariant(t *testing.T) {
	logs := &lockedLogBuffer{}
	logger.SetOutput(logs)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })

	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "有效期内[ref"},
		{ResponseType: types.ResponseTypeAnswer, Content: ` id="c1"]。参见[ref id="c2"]`},
		{ResponseType: types.ResponseTypeAnswer, Content: `。`, Done: true, FinishReason: "stop"},
	}}}}
	engine := newTestEngine(t, model)
	engine.toolRegistry = tools.NewToolRegistry()
	require.Equal(t, "c1", engine.modelContext.RegisterChunk(modelcontext.ChunkReference{
		ChunkID: "chunk-1", DocumentTitle: "Doc",
	}))
	// c2 was never retrieved this turn, so it must fail closed even though the
	// model cited it with a well-formed marker.
	require.Equal(t, "d1", engine.modelContext.RegisterDocument("doc-real"))

	state, err := engine.Execute(context.Background(), "sess-1", "msg-1", "车辆管理规定", nil)
	require.NoError(t, err)
	require.True(t, state.IsComplete)
	require.Equal(t, `有效期内<kb doc="Doc" chunk_id="chunk-1" />。参见。`, state.FinalAnswer)

	require.Len(t, citationVariantLogLines(logs), 1)
	line := citationVariantLogLines(logs)[0]
	require.Contains(t, line, "variant=square_double_quoted")
	require.Contains(t, line, "count=2")
	require.Contains(t, line, "model_id=mock-id")
	require.Contains(t, line, "session_id=sess-1")
	require.Contains(t, line, "message_id=msg-1")
}

// TestAgentTurnLogsNoVariantForCanonicalCitations keeps the log quiet when the
// model follows the protocol, so per-model counts stay comparable.
func TestAgentTurnLogsNoVariantForCanonicalCitations(t *testing.T) {
	logs := &lockedLogBuffer{}
	logger.SetOutput(logs)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })

	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: `有效期内<ref id="c1"/>。`, Done: true, FinishReason: "stop"},
	}}}}
	engine := newTestEngine(t, model)
	engine.toolRegistry = tools.NewToolRegistry()
	engine.modelContext.RegisterChunk(modelcontext.ChunkReference{ChunkID: "chunk-1", DocumentTitle: "Doc"})

	state, err := engine.Execute(context.Background(), "sess-1", "msg-1", "车辆管理规定", nil)
	require.NoError(t, err)
	require.Equal(t, `有效期内<kb doc="Doc" chunk_id="chunk-1" />。`, state.FinalAnswer)
	require.Empty(t, citationVariantLogLines(logs))
}
