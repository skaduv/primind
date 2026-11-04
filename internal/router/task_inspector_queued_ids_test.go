package router

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/primind/primind/internal/types"
	"github.com/redis/go-redis/v9"
)

// One pass collects every knowledge a cancellable task references, across
// pages and states, and skips task types that are not per-knowledge.
func TestQueuedKnowledgeIDsCollectsAcrossPagesAndStates(t *testing.T) {
	server := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	asynqClient := asynq.NewClientFromRedisClient(redisClient)
	t.Cleanup(func() {
		_ = asynqClient.Close()
		_ = redisClient.Close()
	})
	inspector := &asynqTaskInspector{inspector: asynq.NewInspectorFromRedisClient(redisClient), redis: redisClient}

	const pending = listPageSize + 5
	for i := 0; i < pending; i++ {
		enqueueTask(t, asynqClient, types.TypeDocumentProcess,
			fmt.Sprintf(`{"knowledge_id":"k-%d"}`, i), fmt.Sprintf("doc-%03d", i))
	}
	scheduleTask(t, asynqClient, types.TypeKnowledgePostProcess, `{"knowledge_id":"k-scheduled"}`, "post")
	enqueueTask(t, asynqClient, types.TypeKBClone, `{"knowledge_id":"k-kb-level"}`, "clone")

	ids, err := inspector.QueuedKnowledgeIDs(context.Background())
	if err != nil {
		t.Fatalf("QueuedKnowledgeIDs: %v", err)
	}
	if len(ids) != pending+1 {
		t.Fatalf("got %d ids, want %d", len(ids), pending+1)
	}
	for _, id := range []string{"k-0", fmt.Sprintf("k-%d", pending-1), "k-scheduled"} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("missing %s", id)
		}
	}
	if _, ok := ids["k-kb-level"]; ok {
		t.Fatal("KB-level task types must not count")
	}
}

// A Redis failure is an error, not an empty queue.
func TestQueuedKnowledgeIDsReportsBackendErrors(t *testing.T) {
	server := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	inspector := &asynqTaskInspector{inspector: asynq.NewInspectorFromRedisClient(redisClient), redis: redisClient}
	server.Close()

	if _, err := inspector.QueuedKnowledgeIDs(context.Background()); err == nil {
		t.Fatal("expected an error when Redis is down")
	}
}

// The residue sweep asks whether a kb:delete task is still queued for a KB;
// only kb:delete payloads naming that exact KB count.
func TestHasQueuedKBDeleteTasksMatchesOnlyThatKB(t *testing.T) {
	server := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	asynqClient := asynq.NewClientFromRedisClient(redisClient)
	t.Cleanup(func() {
		_ = asynqClient.Close()
		_ = redisClient.Close()
	})
	inspector := &asynqTaskInspector{inspector: asynq.NewInspectorFromRedisClient(redisClient), redis: redisClient}

	enqueueTask(t, asynqClient, types.TypeKBDelete, `{"tenant_id":1,"knowledge_base_id":"kb-a"}`, "kbdel-a")
	enqueueTask(t, asynqClient, types.TypeKBClone, `{"knowledge_base_id":"kb-b"}`, "clone-b")

	for kb, want := range map[string]bool{"kb-a": true, "kb-b": false, "kb-c": false} {
		got, err := inspector.HasQueuedKBDeleteTasks(context.Background(), kb)
		if err != nil {
			t.Fatalf("%s: %v", kb, err)
		}
		if got != want {
			t.Fatalf("%s: queued=%v, want %v", kb, got, want)
		}
	}
}
