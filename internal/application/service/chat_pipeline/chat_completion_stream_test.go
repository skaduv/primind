package chatpipeline

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/primind/primind/internal/common"
	"github.com/primind/primind/internal/event"
	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/models/chat"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// syncEventBus is a thread-safe recorder; the stream plugin emits from a
// background goroutine so the test must guard concurrent appends.
type syncEventBus struct {
	mu     sync.Mutex
	events []types.Event
}

func (b *syncEventBus) On(types.EventType, types.EventHandler) {}

func (b *syncEventBus) Emit(_ context.Context, evt types.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, evt)
	return nil
}

func (b *syncEventBus) finalAnswerContents() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, evt := range b.events {
		if evt.Type != types.EventType(event.EventAgentFinalAnswer) {
			continue
		}
		if data, ok := evt.Data.(event.AgentFinalAnswerData); ok {
			out = append(out, data.Content)
		}
	}
	return out
}

func (b *syncEventBus) finalAnswerEvents() []event.AgentFinalAnswerData {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []event.AgentFinalAnswerData
	for _, evt := range b.events {
		if evt.Type != types.EventType(event.EventAgentFinalAnswer) {
			continue
		}
		if data, ok := evt.Data.(event.AgentFinalAnswerData); ok {
			out = append(out, data)
		}
	}
	return out
}

// openStreamChat returns a buffered channel preloaded with chunks and never
// closes it, so the stream plugin blocks on the channel until ctx is cancelled
// — deterministically exercising the ctx.Done() branch.
type openStreamChat struct {
	chunks      []types.StreamResponse
	closeStream bool
}

func (m *openStreamChat) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return nil, nil
}

func (m *openStreamChat) ChatStream(
	context.Context, []chat.Message, *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	ch := make(chan types.StreamResponse, len(m.chunks))
	for _, c := range m.chunks {
		ch <- c
	}
	if m.closeStream {
		close(ch)
	}
	return ch, nil
}

func (m *openStreamChat) GetModelName() string { return "mock" }
func (m *openStreamChat) GetModelID() string   { return "mock" }

// stubModelService only needs GetChatModel; the rest is unused for this test.
type stubModelService struct {
	interfaces.ModelService
	model chat.Chat
}

func (s *stubModelService) GetChatModel(context.Context, string) (chat.Chat, error) {
	return s.model, nil
}

// TestStreamDropsIncompleteHandleOnCancel verifies that cancellation cannot
// leak a model-context protocol fragment to the client.
func TestStreamDropsIncompleteHandleOnCancel(t *testing.T) {
	const ref = "resource://AbCdEfGhIjKlMnOpQrStUv"
	bus := &syncEventBus{}
	model := &openStreamChat{chunks: []types.StreamResponse{
		// Ends with a partial alias prefix ("res://0"), so the stream decoder
		// holds it back waiting for the rest that never arrives before cancel.
		{ResponseType: types.ResponseTypeAnswer, Content: "hello res://0"},
	}}

	chatManage := &types.ChatManage{}
	chatManage.SessionID = "sess-cancel"
	chatManage.UserContent = ref // seeds the registry so res://0001 becomes a known alias
	chatManage.EventBus = bus

	ctx, cancel := context.WithCancel(context.Background())
	plugin := &PluginChatCompletionStream{modelService: &stubModelService{model: model}}
	require.Nil(t, plugin.OnEvent(ctx, types.CHAT_COMPLETION_STREAM, chatManage, func() *PluginError { return nil }))

	// Wait until the pre-hold content has been emitted, then cancel.
	require.Eventually(t, func() bool {
		for _, c := range bus.finalAnswerContents() {
			if c == "hello " {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)

	cancel()

	// Give the cancellation path time to flush, then assert that only the
	// meaningful prefix was emitted; the incomplete private handle is dropped.
	require.Eventually(t, func() bool { return len(bus.finalAnswerContents()) >= 1 }, 2*time.Second, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, []string{"hello "}, bus.finalAnswerContents())
}

func TestStreamIgnoresDuplicateTerminalAnswer(t *testing.T) {
	bus := &syncEventBus{}
	model := &openStreamChat{closeStream: true, chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "hello"},
		{ResponseType: types.ResponseTypeAnswer, Done: true},
		// Some providers repeat the same terminal response when their EOF
		// sentinel is received after finish_reason.
		{ResponseType: types.ResponseTypeAnswer, Done: true},
	}}

	chatManage := &types.ChatManage{}
	chatManage.SessionID = "sess-duplicate-done"
	chatManage.EventBus = bus
	plugin := &PluginChatCompletionStream{modelService: &stubModelService{model: model}}
	require.Nil(t, plugin.OnEvent(context.Background(), types.CHAT_COMPLETION_STREAM, chatManage, func() *PluginError { return nil }))

	require.Eventually(t, func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return len(bus.events) == 2
	}, 2*time.Second, 5*time.Millisecond)

	bus.mu.Lock()
	defer bus.mu.Unlock()
	var answerEvents []event.AgentFinalAnswerData
	for _, evt := range bus.events {
		if evt.Type == types.EventType(event.EventAgentFinalAnswer) {
			answerEvents = append(answerEvents, evt.Data.(event.AgentFinalAnswerData))
		}
	}
	require.Equal(t, []event.AgentFinalAnswerData{{Content: "hello"}, {Done: true}}, answerEvents)
}

func TestStreamReportsEmptyLengthTruncation(t *testing.T) {
	bus := &syncEventBus{}
	model := &openStreamChat{closeStream: true, chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeThinking, Content: "planning"},
		{ResponseType: types.ResponseTypeThinking, Done: true},
		{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "length"},
	}}

	chatManage := &types.ChatManage{}
	chatManage.SessionID = "sess-empty-length"
	chatManage.EventBus = bus
	plugin := &PluginChatCompletionStream{modelService: &stubModelService{model: model}}
	require.Nil(t, plugin.OnEvent(
		context.Background(), types.CHAT_COMPLETION_STREAM, chatManage,
		func() *PluginError { return nil },
	))

	want := event.AgentFinalAnswerData{
		Content:   EmptyTruncatedAnswerFallback,
		Done:      true,
		Truncated: true,
	}
	require.Eventually(t, func() bool {
		return len(bus.finalAnswerEvents()) == 1
	}, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, []event.AgentFinalAnswerData{want}, bus.finalAnswerEvents())
}

func TestStreamMarksPartialLengthTruncation(t *testing.T) {
	for _, finishReason := range []string{"length", "max_tokens", "max_output_tokens"} {
		t.Run(finishReason, func(t *testing.T) {
			bus := &syncEventBus{}
			model := &openStreamChat{closeStream: true, chunks: []types.StreamResponse{
				{ResponseType: types.ResponseTypeAnswer, Content: "partial answer"},
				{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: finishReason},
			}}

			chatManage := &types.ChatManage{}
			chatManage.SessionID = "sess-partial-" + finishReason
			chatManage.EventBus = bus
			plugin := &PluginChatCompletionStream{modelService: &stubModelService{model: model}}
			require.Nil(t, plugin.OnEvent(
				context.Background(), types.CHAT_COMPLETION_STREAM, chatManage,
				func() *PluginError { return nil },
			))

			require.Eventually(t, func() bool {
				return len(bus.finalAnswerEvents()) == 2
			}, 2*time.Second, 5*time.Millisecond)
			events := bus.finalAnswerEvents()
			require.Equal(t, "partial answer", events[0].Content+events[1].Content)
			require.False(t, events[0].Done)
			require.False(t, events[0].Truncated)
			require.True(t, events[1].Done)
			require.True(t, events[1].Truncated)
			require.NotContains(t, events[1].Content, EmptyTruncatedAnswerFallback)
		})
	}
}

func TestStreamLeavesEmptyNaturalStopUnchanged(t *testing.T) {
	bus := &syncEventBus{}
	model := &openStreamChat{closeStream: true, chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "stop"},
	}}

	chatManage := &types.ChatManage{}
	chatManage.SessionID = "sess-empty-stop"
	chatManage.EventBus = bus
	plugin := &PluginChatCompletionStream{modelService: &stubModelService{model: model}}
	require.Nil(t, plugin.OnEvent(
		context.Background(), types.CHAT_COMPLETION_STREAM, chatManage,
		func() *PluginError { return nil },
	))

	require.Eventually(t, func() bool {
		return len(bus.finalAnswerEvents()) == 1
	}, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, []event.AgentFinalAnswerData{{Done: true}}, bus.finalAnswerEvents())
}

func TestStreamReportsIncompleteAsError(t *testing.T) {
	bus := &syncEventBus{}
	model := &openStreamChat{closeStream: true, chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "partial"},
		{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: types.FinishReasonIncomplete},
	}}

	chatManage := &types.ChatManage{}
	chatManage.SessionID = "sess-incomplete"
	chatManage.EventBus = bus
	plugin := &PluginChatCompletionStream{modelService: &stubModelService{model: model}}
	require.Nil(t, plugin.OnEvent(
		context.Background(), types.CHAT_COMPLETION_STREAM, chatManage,
		func() *PluginError { return nil },
	))

	errorMessage := func() string {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		for _, evt := range bus.events {
			if data, ok := evt.Data.(event.ErrorData); ok {
				return data.Error
			}
		}
		return ""
	}
	require.Eventually(t, func() bool { return errorMessage() != "" }, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, types.StreamEndedEarlyError, errorMessage())
	for _, answer := range bus.finalAnswerEvents() {
		require.False(t, answer.Done, "a cut-off stream must not close the answer as complete")
	}
}

// lockedBuffer is a bytes.Buffer the stream plugin's goroutine can write to
// while the test goroutine reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// citationStreamChatManage seeds a quick-Q&A request whose two retrieved
// chunks are citable as c1 and c2.
func citationStreamChatManage(bus *syncEventBus, sessionID string) *types.ChatManage {
	chatManage := &types.ChatManage{}
	chatManage.SessionID = sessionID
	chatManage.MessageID = "msg-" + sessionID
	chatManage.ChatModelID = "model-" + sessionID
	chatManage.MergeResult = []*types.SearchResult{
		{ID: "chunk-1", KnowledgeTitle: "Doc"},
		{ID: "chunk-2", KnowledgeTitle: "Doc"},
	}
	chatManage.EventBus = bus
	return chatManage
}

// variantLogLines returns the captured log lines that report a citation variant.
func variantLogLines(logs *lockedBuffer) []string {
	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, common.CitationVariantLogMessage) {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestStreamExpandsBracketedCitationAndLogsVariant drives the quick-Q&A answer
// path at the plugin level (BF-01 AC1, AC4, AC6): a model writing the
// Markdown-style [ref id="cN"], split across provider chunks, reaches the
// client only as a finished citation card, and the closing log line names the
// model, the answer and the variant.
func TestStreamExpandsBracketedCitationAndLogsVariant(t *testing.T) {
	logs := &lockedBuffer{}
	logger.SetOutput(logs)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })

	bus := &syncEventBus{}
	model := &openStreamChat{closeStream: true, chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "有效期内[ref"},
		{ResponseType: types.ResponseTypeAnswer, Content: ` id="c`},
		{ResponseType: types.ResponseTypeAnswer, Content: `2"]。`, Done: true, FinishReason: "stop"},
	}}
	plugin := &PluginChatCompletionStream{modelService: &stubModelService{model: model}}
	require.Nil(t, plugin.OnEvent(
		context.Background(), types.CHAT_COMPLETION_STREAM,
		citationStreamChatManage(bus, "bracket"),
		func() *PluginError { return nil },
	))

	require.Eventually(t, func() bool {
		events := bus.finalAnswerEvents()
		return len(events) > 0 && events[len(events)-1].Done
	}, 2*time.Second, 5*time.Millisecond)
	events := bus.finalAnswerEvents()
	// The half marker never reaches the wire: the text before it streams at
	// once, the tag is released only once it is a complete card. (A provider
	// chunk that resolves to nothing still produces an empty event.)
	var wire strings.Builder
	for _, evt := range events {
		require.NotContains(t, evt.Content, "ref id=")
		wire.WriteString(evt.Content)
	}
	require.Equal(t, "有效期内", events[0].Content)
	require.Equal(t, `<kb doc="Doc" chunk_id="chunk-2" />。`, events[len(events)-1].Content)
	require.Equal(t, "有效期内"+`<kb doc="Doc" chunk_id="chunk-2" />。`, wire.String())

	require.Eventually(t, func() bool {
		return len(variantLogLines(logs)) == 1
	}, 2*time.Second, 5*time.Millisecond)
	line := variantLogLines(logs)[0]
	require.Contains(t, line, "variant=square_double_quoted")
	require.Contains(t, line, "count=1")
	require.Contains(t, line, "model_id=model-bracket")
	require.Contains(t, line, "session_id=bracket")
	require.Contains(t, line, "message_id=msg-bracket")
}

// TestStreamLogsNoVariantForCanonicalCitations keeps the log quiet for a model
// that follows the protocol: the card still streams, no variant line is
// written, which is what lets the per-model counts be compared.
func TestStreamLogsNoVariantForCanonicalCitations(t *testing.T) {
	logs := &lockedBuffer{}
	logger.SetOutput(logs)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })

	bus := &syncEventBus{}
	model := &openStreamChat{closeStream: true, chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: `有效期内<ref id="c`},
		{ResponseType: types.ResponseTypeAnswer, Content: `2"/>。`, Done: true, FinishReason: "stop"},
	}}
	plugin := &PluginChatCompletionStream{modelService: &stubModelService{model: model}}
	require.Nil(t, plugin.OnEvent(
		context.Background(), types.CHAT_COMPLETION_STREAM,
		citationStreamChatManage(bus, "canonical"),
		func() *PluginError { return nil },
	))

	require.Eventually(t, func() bool {
		return len(bus.finalAnswerEvents()) == 2
	}, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, `<kb doc="Doc" chunk_id="chunk-2" />。`, bus.finalAnswerEvents()[1].Content)
	time.Sleep(20 * time.Millisecond)
	require.Empty(t, variantLogLines(logs))
}
