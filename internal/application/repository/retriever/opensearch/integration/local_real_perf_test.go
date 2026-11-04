//go:build integration

package integration

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
)

// TestLocalRealBaseline measures adapter-only write throughput and query
// latency on the developer cluster. It is a baseline, not an acceptance
// gate: no embedding/LLM time, single node, no concurrent business load.
//
//	PRIMIND_OS_LOCAL_URL=http://127.0.0.1:9200 PRIMIND_OS_LOCAL_PERF=1 \
//	  go test -tags integration ./internal/application/repository/retriever/opensearch/integration \
//	  -run '^TestLocalRealBaseline$' -count=1 -v -timeout 20m
//
// Optional: PRIMIND_OS_PERF_CHUNKS (default 10000), PRIMIND_OS_PERF_DIM (768),
// PRIMIND_OS_PERF_BATCH (40), PRIMIND_OS_PERF_QUERIES (200).
func TestLocalRealBaseline(t *testing.T) {
	if os.Getenv("PRIMIND_OS_LOCAL_PERF") == "" {
		t.Skip("PRIMIND_OS_LOCAL_PERF not set")
	}
	envInt := func(k string, def int) int {
		if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
			return v
		}
		return def
	}
	nChunks, dim := envInt("PRIMIND_OS_PERF_CHUNKS", 10000), envInt("PRIMIND_OS_PERF_DIM", 768)
	batch, nQueries := envInt("PRIMIND_OS_PERF_BATCH", 40), envInt("PRIMIND_OS_PERF_QUERIES", 200)

	c := newLocalCluster(t)
	index := fmt.Sprintf("%s_%d_v1", c.base, dim)
	t.Cleanup(func() { c.raw("DELETE", "/"+index, "") })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	rng := rand.New(rand.NewSource(1))
	randVec := func() []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = rng.Float32()*2 - 1
		}
		return v
	}
	kb := "kb-perf-" + c.storeID
	words := []string{"opensearch", "检索", "向量", "知识库", "primind", "索引", "片段", "召回"}

	// Write phase: sequential BatchSave, one worker, like the indexer's default.
	var batchDur []time.Duration
	var sample [][]float32
	start := time.Now()
	for off := 0; off < nChunks; off += batch {
		n := min(batch, nChunks-off)
		infos := make([]*types.IndexInfo, n)
		emb := make(map[string][]float32, n)
		for i := range infos {
			id := fmt.Sprintf("perf-%07d", off+i)
			infos[i] = &types.IndexInfo{ChunkID: id, SourceID: id, KnowledgeID: fmt.Sprintf("doc-%d", (off+i)/50),
				KnowledgeBaseID: kb, IsEnabled: true, SourceType: types.ChunkSourceType,
				Content: fmt.Sprintf("%s %s chunk %d token%d", words[(off+i)%len(words)], words[(off+i*3)%len(words)], off+i, off+i)}
			emb[id] = randVec()
			if len(sample) < nQueries && (off+i)%max(1, nChunks/nQueries) == 0 {
				sample = append(sample, emb[id])
			}
		}
		t0 := time.Now()
		if err := c.repo.BatchSave(ctx, infos, map[string]any{"embedding": emb}); err != nil {
			t.Fatalf("batch at %d: %v", off, err)
		}
		batchDur = append(batchDur, time.Since(t0))
	}
	writeDur := time.Since(start)
	c.raw("POST", "/"+index+"/_refresh", "")
	if got := c.count(index, ""); got != nChunks {
		t.Fatalf("indexed %d, want %d", got, nChunks)
	}

	measure := func(name string, run func(i int) error) {
		var lat []time.Duration
		for i := 0; i < nQueries; i++ {
			t0 := time.Now()
			if err := run(i); err != nil {
				t.Fatalf("%s query %d: %v", name, i, err)
			}
			lat = append(lat, time.Since(t0))
		}
		t.Logf("%-8s queries=%d p50=%v p95=%v p99=%v max=%v", name, len(lat),
			pct(lat, 50), pct(lat, 95), pct(lat, 99), pct(lat, 100))
	}
	measure("vector", func(i int) error {
		_, err := c.repo.Retrieve(ctx, types.RetrieveParams{RetrieverType: types.VectorRetrieverType,
			Embedding: sample[i%len(sample)], KnowledgeBaseIDs: []string{kb}, TopK: 10})
		return err
	})
	measure("keyword", func(i int) error {
		_, err := c.repo.Retrieve(ctx, types.RetrieveParams{RetrieverType: types.KeywordsRetrieverType,
			Query: words[i%len(words)] + " token" + strconv.Itoa(i*37%nChunks), KnowledgeBaseIDs: []string{kb}, TopK: 10,
			AdditionalParams: map[string]any{"dim": dim}})
		return err
	})

	// Cost of refreshBeforeByQuery: every by-query op (delete, disable/tag,
	// move, copy) now refreshes first. Measure, right after a fresh batch so
	// the refresh has segments to publish, a bare refresh and a full
	// single-chunk disable (refresh + update_by_query + its own refresh).
	var refreshLat, disableLat []time.Duration
	for i := 0; i < 50; i++ {
		write := func(tag string) string {
			infos := make([]*types.IndexInfo, batch)
			emb := make(map[string][]float32, batch)
			for j := range infos {
				id := fmt.Sprintf("perf-%s-%03d-%03d", tag, i, j)
				infos[j] = &types.IndexInfo{ChunkID: id, SourceID: id, KnowledgeID: "doc-r", KnowledgeBaseID: kb,
					IsEnabled: true, SourceType: types.ChunkSourceType, Content: "refresh cost " + id}
				emb[id] = randVec()
			}
			if err := c.repo.BatchSave(ctx, infos, map[string]any{"embedding": emb}); err != nil {
				t.Fatal(err)
			}
			return infos[0].ChunkID
		}
		write("r")
		t0 := time.Now()
		c.raw("POST", "/"+c.base+"_*/_refresh", "")
		refreshLat = append(refreshLat, time.Since(t0))
		id := write("d")
		t0 = time.Now()
		if err := c.repo.BatchUpdateChunkEnabledStatus(ctx, map[string]bool{id: false}); err != nil {
			t.Fatal(err)
		}
		disableLat = append(disableLat, time.Since(t0))
	}
	t.Logf("refresh-after-write n=50 p50=%v p95=%v max=%v; disable-1-chunk (incl. refresh) p50=%v p95=%v max=%v",
		pct(refreshLat, 50), pct(refreshLat, 95), pct(refreshLat, 100),
		pct(disableLat, 50), pct(disableLat, 95), pct(disableLat, 100))

	t.Logf("write chunks=%d dim=%d batch=%d total=%v throughput=%.0f chunks/s batch_p50=%v batch_p95=%v batch_max=%v",
		nChunks, dim, batch, writeDur.Round(time.Millisecond), float64(nChunks)/writeDur.Seconds(),
		pct(batchDur, 50), pct(batchDur, 95), pct(batchDur, 100))
}

// pct returns the nearest-rank percentile.
func pct(d []time.Duration, p int) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := (p*len(s)+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	return s[idx].Round(100 * time.Microsecond)
}
