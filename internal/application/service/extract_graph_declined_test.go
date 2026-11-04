package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/primind/primind/internal/models/chat"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

type graphReplyChat struct{ resp *types.ChatResponse }

func (c graphReplyChat) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return c.resp, nil
}

func (c graphReplyChat) ChatStream(
	context.Context, []chat.Message, *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, errors.New("not implemented")
}
func (c graphReplyChat) GetModelName() string { return "graph-reply" }
func (c graphReplyChat) GetModelID() string   { return "graph-reply" }

type graphChunkRepo struct {
	interfaces.ChunkRepository
	chunk *types.Chunk
}

func (r graphChunkRepo) GetChunkByID(context.Context, uint64, string) (*types.Chunk, error) {
	return r.chunk, nil
}

type graphKBRepo struct {
	interfaces.KnowledgeBaseRepository
	kb *types.KnowledgeBase
}

func (r graphKBRepo) GetKnowledgeBaseByID(context.Context, string) (*types.KnowledgeBase, error) {
	return r.kb, nil
}

// Issue #3600: a prose refusal finishes the per-chunk task (no asynq retry),
// while the same prose cut off by the output budget stays a retriable error.
func TestChunkExtractHandle_ModelDeclinedIsSkipNotRetry(t *testing.T) {
	setGraphBackendForTest(t, types.GraphBackendNebula)
	const prose = "抱歉，该文本为目录页，没有可抽取的实体和关系。"
	payload, err := json.Marshal(types.ExtractChunkPayload{TenantID: 1, ChunkID: "c1", ModelID: "m1"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		finish  string
		wantErr bool
	}{
		{"model stopped on its own: skip", "stop", false},
		{"output truncated: retry", "length", true},
		{"output truncated (max_tokens): retry", "max_tokens", true},
		{"stream broke before stop: retry", types.FinishReasonIncomplete, true},
		{"no finish reason reported: retry", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &ChunkExtractService{
				template: &types.PromptTemplateStructured{Description: "extract"},
				modelService: &stubModelService{chatModel: graphReplyChat{
					resp: &types.ChatResponse{Content: prose, FinishReason: tc.finish},
				}},
				knowledgeBaseRepo: graphKBRepo{kb: &types.KnowledgeBase{
					ID:            "kb1",
					ExtractConfig: &types.ExtractConfig{Enabled: true},
				}},
				chunkRepo: graphChunkRepo{chunk: &types.Chunk{
					ID: "c1", KnowledgeBaseID: "kb1", Content: "目录 …… 1",
				}},
			}
			err := svc.Handle(context.Background(), asynq.NewTask(types.TypeChunkExtract, payload))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Handle err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func setGraphBackendForTest(t *testing.T, backend string) {
	t.Helper()
	types.SetGraphBackendForTest(types.GraphBackendSelection{Backend: backend})
	t.Cleanup(func() {
		types.SetGraphBackendForTest(types.GraphBackendSelection{Backend: types.GraphBackendDisabled})
	})
}

type countingGraphRepo struct {
	interfaces.RetrieveGraphRepository
	adds int
}

func (r *countingGraphRepo) AddGraph(context.Context, types.NameSpace, []*types.GraphData) error {
	r.adds++
	return nil
}

type countingChat struct {
	graphReplyChat
	calls *int
}

func (c countingChat) Chat(ctx context.Context, m []chat.Message, o *chat.ChatOptions) (*types.ChatResponse, error) {
	*c.calls++
	return c.graphReplyChat.Chat(ctx, m, o)
}

// NG-02-AC02: with the graph disabled, NewChunkExtractTask enqueues nothing
// and a task queued before the switch finishes without an LLM or graph call.
func TestChunkExtractDisabledBackendSkipsWithoutWork(t *testing.T) {
	setGraphBackendForTest(t, types.GraphBackendDisabled)
	enq := &countingEnqueuer{}
	ok, err := NewChunkExtractTask(context.Background(), enq, 1, "c1", "m1", "k1", 1, 0)
	if ok || err != nil || enq.calls != 0 {
		t.Fatalf("disabled enqueue: ok=%v err=%v calls=%d", ok, err, enq.calls)
	}

	calls := 0
	graph := &countingGraphRepo{}
	svc := &ChunkExtractService{
		template:     &types.PromptTemplateStructured{Description: "extract"},
		modelService: &stubModelService{chatModel: countingChat{calls: &calls}},
		knowledgeBaseRepo: graphKBRepo{kb: &types.KnowledgeBase{
			ID: "kb1", ExtractConfig: &types.ExtractConfig{Enabled: true},
		}},
		chunkRepo:   graphChunkRepo{chunk: &types.Chunk{ID: "c1", KnowledgeBaseID: "kb1", Content: "x"}},
		graphEngine: graph,
	}
	payload, _ := json.Marshal(types.ExtractChunkPayload{TenantID: 1, ChunkID: "c1", ModelID: "m1"})
	if err := svc.Handle(context.Background(), asynq.NewTask(types.TypeChunkExtract, payload)); err != nil {
		t.Fatalf("Handle = %v, want nil", err)
	}
	if calls != 0 || graph.adds != 0 {
		t.Fatalf("disabled handle did work: llm=%d adds=%d", calls, graph.adds)
	}

	setGraphBackendForTest(t, types.GraphBackendNebula)
	ok, err = NewChunkExtractTask(context.Background(), enq, 1, "c1", "m1", "k1", 1, 0)
	if !ok || err != nil || enq.calls != 1 {
		t.Fatalf("nebulagraph enqueue: ok=%v err=%v calls=%d", ok, err, enq.calls)
	}
}

type countingEnqueuer struct{ calls int }

func (e *countingEnqueuer) Enqueue(task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	e.calls++
	return &asynq.TaskInfo{ID: "t", Queue: types.QueueGraph}, nil
}

const twoNodeGraphReply = "```json\n[\n  {\"entity\": \"Alice\"},\n  {\"entity\": \"Bob\"},\n" +
	"  {\"entity1\": \"Alice\", \"entity2\": \"Bob\", \"relation\": \"knows\"}\n]\n```"

type flakyGraphRepo struct {
	interfaces.RetrieveGraphRepository
	fails, adds int
}

func (r *flakyGraphRepo) AddGraph(context.Context, types.NameSpace, []*types.GraphData) error {
	if r.fails > 0 {
		r.fails--
		return errors.New("nebula execution failed (result may be unknown)")
	}
	r.adds++
	return nil
}

func graphExtractService(calls *int, graph interfaces.RetrieveGraphRepository, tracker SpanTracker) *ChunkExtractService {
	return &ChunkExtractService{
		template: &types.PromptTemplateStructured{Description: "extract", Tags: []string{"knows"}},
		modelService: &stubModelService{chatModel: countingChat{calls: calls, graphReplyChat: graphReplyChat{
			resp: &types.ChatResponse{Content: twoNodeGraphReply, FinishReason: "stop"},
		}}},
		knowledgeBaseRepo: graphKBRepo{kb: &types.KnowledgeBase{
			ID: "kb1", ExtractConfig: &types.ExtractConfig{Enabled: true},
		}},
		chunkRepo: graphChunkRepo{chunk: &types.Chunk{
			ID: "c1", KnowledgeBaseID: "kb1", KnowledgeID: "k1", Content: "Alice knows Bob",
		}},
		graphEngine: graph,
		spanTracker: tracker,
	}
}

// NG-03-AC04: an attempt superseded before or during extraction (reparse
// started while the LLM call was in flight) never writes graph data.
func TestChunkExtractSupersededAttemptWritesNothing(t *testing.T) {
	setGraphBackendForTest(t, types.GraphBackendNebula)
	payload, _ := json.Marshal(types.ExtractChunkPayload{TenantID: 1, ChunkID: "c1", ModelID: "m1", KnowledgeID: "k1", Attempt: 1})
	cases := []struct {
		name      string
		latest    []int
		llm, adds int
	}{
		{"current attempt writes", []int{1}, 1, 1},
		{"superseded before start", []int{2}, 0, 0},
		{"superseded during extraction", []int{1, 2}, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			graph := &flakyGraphRepo{}
			svc := graphExtractService(&calls, graph, newAttemptTracker(tc.latest...))
			if err := svc.Handle(context.Background(), asynq.NewTask(types.TypeChunkExtract, payload)); err != nil {
				t.Fatalf("Handle = %v", err)
			}
			if calls != tc.llm || graph.adds != tc.adds {
				t.Fatalf("llm=%d adds=%d, want %d/%d", calls, graph.adds, tc.llm, tc.adds)
			}
		})
	}
}

// NG-03-AC05: a failed graph write surfaces as a task error (asynq retries it,
// MaxRetry 3) instead of a silent success; the retry writes exactly once.
func TestChunkExtractGraphWriteFailureIsRetriedNotSwallowed(t *testing.T) {
	setGraphBackendForTest(t, types.GraphBackendNebula)
	payload, _ := json.Marshal(types.ExtractChunkPayload{TenantID: 1, ChunkID: "c1", ModelID: "m1"})
	calls := 0
	graph := &flakyGraphRepo{fails: 1}
	svc := graphExtractService(&calls, graph, nil)
	task := asynq.NewTask(types.TypeChunkExtract, payload)
	if err := svc.Handle(context.Background(), task); err == nil {
		t.Fatal("first attempt with failing graph write returned nil")
	}
	if err := svc.Handle(context.Background(), task); err != nil {
		t.Fatalf("retry = %v", err)
	}
	if graph.adds != 1 {
		t.Fatalf("adds = %d, want exactly 1 after retry", graph.adds)
	}
}
