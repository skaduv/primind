//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	adapter "github.com/primind/primind/internal/application/repository/retriever/opensearch"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	secutils "github.com/primind/primind/internal/utils"
)

// Local real-cluster checks. Run against a developer OpenSearch:
//
//	PRIMIND_OS_LOCAL_URL=http://127.0.0.1:9200 go test -tags integration \
//	  ./internal/application/repository/retriever/opensearch/integration -run '^TestLocalReal' -count=1 -v
//
// Every index uses the primind_os_it_<random> prefix and only the exact
// indices this test created are deleted afterwards.

const localDim = 8

type localCluster struct {
	t        *testing.T
	endpoint string
	base     string
	storeID  string
	repo     interfaces.RetrieveEngineRepository
}

func newLocalCluster(t *testing.T) *localCluster {
	t.Helper()
	return newLocalClusterVia(t, "")
}

// newLocalClusterVia builds the repository against repoAddr (e.g. a fault
// proxy) while raw checks and cleanup still talk to the cluster directly.
func newLocalClusterVia(t *testing.T, repoAddr string) *localCluster {
	t.Helper()
	endpoint := os.Getenv("PRIMIND_OS_LOCAL_URL")
	if endpoint == "" {
		t.Skip("PRIMIND_OS_LOCAL_URL not set")
	}
	if repoAddr == "" {
		repoAddr = endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	secutils.SetSSRFWhitelistFromRaw(u.Hostname())
	t.Cleanup(func() { secutils.SetSSRFWhitelistFromRaw("") })

	b := make([]byte, 8)
	_, _ = rand.Read(b)
	storeID := hex.EncodeToString(b) // 16 chars; base suffix = first 12
	c := &localCluster{t: t, endpoint: strings.TrimRight(endpoint, "/"), storeID: storeID,
		base: "primind_os_it_" + storeID[:12]}

	client, err := adapter.NewOpenSearchClient(&types.ConnectionConfig{Addr: repoAddr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adapter.CloseIdleConnections(client) })
	repo, err := adapter.NewRepository(context.Background(), client, storeID,
		&types.IndexConfig{IndexName: "primind_os_it", NumberOfShards: 1, NumberOfReplicas: 0})
	if err != nil {
		t.Fatal(err)
	}
	c.repo = repo
	t.Cleanup(c.cleanup)
	return c
}

func (c *localCluster) ownIndices() []string {
	return []string{fmt.Sprintf("%s_%d_v1", c.base, localDim), c.base + "_keywords"}
}

func (c *localCluster) cleanup() {
	for _, idx := range c.ownIndices() {
		req, _ := http.NewRequest(http.MethodDelete, c.endpoint+"/"+idx, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
}

func (c *localCluster) raw(method, path, body string) map[string]any {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.endpoint+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		c.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

// count refreshes the exact index then returns docs matching query.
func (c *localCluster) count(index, query string) int {
	c.t.Helper()
	c.raw(http.MethodPost, "/"+index+"/_refresh", "")
	if query == "" {
		query = `{"query":{"match_all":{}}}`
	}
	return int(c.raw(http.MethodPost, "/"+index+"/_count", query)["count"].(float64))
}

func (c *localCluster) source(index, id string) map[string]any {
	c.t.Helper()
	return c.raw(http.MethodGet, "/"+index+"/_doc/"+id, "")["_source"].(map[string]any)
}

func vec(seed int) []float32 {
	v := make([]float32, localDim)
	v[seed%localDim] = 1
	v[(seed+1)%localDim] = 0.5
	return v
}

type localChunk struct {
	info *types.IndexInfo
	emb  []float32
}

func sampleChunks(kb, doc string, n, seedBase int) []localChunk {
	out := make([]localChunk, n)
	for i := range out {
		id := fmt.Sprintf("%s-c%d", doc, i)
		out[i] = localChunk{
			info: &types.IndexInfo{ID: id, ChunkID: id, SourceID: id, SourceType: types.ChunkSourceType,
				KnowledgeID: doc, KnowledgeBaseID: kb, KnowledgeType: "manual", IsEnabled: true,
				Content: fmt.Sprintf("primind opensearch sample %s chunk %d token%d", doc, i, seedBase+i)},
			emb: vec(seedBase + i),
		}
	}
	return out
}

func batchParams(chunks []localChunk) ([]*types.IndexInfo, map[string]any) {
	infos := make([]*types.IndexInfo, len(chunks))
	emb := map[string][]float32{}
	for i, c := range chunks {
		infos[i] = c.info
		emb[c.info.SourceID] = c.emb
	}
	return infos, map[string]any{"embedding": emb}
}

func chunkIDs(rs []*types.RetrieveResult) []string {
	var ids []string
	for _, r := range rs {
		for _, h := range r.Results {
			ids = append(ids, h.ChunkID)
		}
	}
	return ids
}

func TestLocalRealWriteRetrieveDelete(t *testing.T) {
	c := newLocalCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	kbA, kbB := "kb-a-"+c.storeID, "kb-b-"+c.storeID
	docA := sampleChunks(kbA, "docA", 5, 0)
	docB := sampleChunks(kbB, "docB", 3, 3)
	vecIndex := c.ownIndices()[0]

	// Single save + bulk save, then rewrite the same chunks: _id = chunk_id, no duplicates.
	if err := c.repo.Save(ctx, docA[0].info, map[string]any{"embedding": map[string][]float32{docA[0].info.SourceID: docA[0].emb}}); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		for _, set := range [][]localChunk{docA, docB} {
			infos, params := batchParams(set)
			if err := c.repo.BatchSave(ctx, infos, params); err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
	if n := c.count(vecIndex, ""); n != 8 {
		t.Fatalf("expected 8 unique chunks after repeated writes, got %d", n)
	}
	if got := c.source(vecIndex, "docA-c1")["embedding"].([]any); len(got) != localDim {
		t.Fatalf("stored vector dim %d != %d", len(got), localDim)
	}

	// Vector retrieve, scoped to KB A.
	vr, err := c.repo.Retrieve(ctx, types.RetrieveParams{RetrieverType: types.VectorRetrieverType,
		Embedding: docA[2].emb, KnowledgeBaseIDs: []string{kbA}, TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	ids := chunkIDs(vr)
	if len(ids) == 0 || ids[0] != "docA-c2" {
		t.Fatalf("vector top hit = %v, want docA-c2 first", ids)
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, "docA-") {
			t.Fatalf("KB filter leaked %s", id)
		}
	}
	// Keyword retrieve (dim-scoped and dim-less wildcard).
	for _, ap := range []map[string]any{{"dim": localDim}, nil} {
		kr, err := c.repo.Retrieve(ctx, types.RetrieveParams{RetrieverType: types.KeywordsRetrieverType,
			Query: "token4", KnowledgeBaseIDs: []string{kbB}, TopK: 5, AdditionalParams: ap})
		if err != nil {
			t.Fatal(err)
		}
		if ids := chunkIDs(kr); len(ids) != 1 || ids[0] != "docB-c1" {
			t.Fatalf("keyword(%v) hits = %v, want [docB-c1]", ap, ids)
		}
	}

	// Disable + tag update are visible in the stored source and honoured by retrieval.
	if err := c.repo.BatchUpdateChunkEnabledStatus(ctx, map[string]bool{"docA-c2": false}); err != nil {
		t.Fatal(err)
	}
	if err := c.repo.BatchUpdateChunkTagID(ctx, map[string]string{"docA-c3": "tag-x"}); err != nil {
		t.Fatal(err)
	}
	c.raw(http.MethodPost, "/"+vecIndex+"/_refresh", "")
	vr, err = c.repo.Retrieve(ctx, types.RetrieveParams{RetrieverType: types.VectorRetrieverType,
		Embedding: docA[2].emb, KnowledgeBaseIDs: []string{kbA}, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range chunkIDs(vr) {
		if id == "docA-c2" {
			t.Fatal("disabled chunk still retrieved")
		}
	}
	if tag := c.source(vecIndex, "docA-c3")["tag_id"]; tag != "tag-x" {
		t.Fatalf("tag_id = %v", tag)
	}

	// Delete one doc by knowledge id; the other KB is untouched.
	if err := c.repo.DeleteByKnowledgeIDList(ctx, []string{"docA"}, localDim, "manual"); err != nil {
		t.Fatal(err)
	}
	if n := c.count(vecIndex, fmt.Sprintf(`{"query":{"term":{"knowledge_base_id":%q}}}`, kbA)); n != 0 {
		t.Fatalf("docA residue %d", n)
	}
	if n := c.count(vecIndex, fmt.Sprintf(`{"query":{"term":{"knowledge_base_id":%q}}}`, kbB)); n != 3 {
		t.Fatalf("docB affected: %d", n)
	}
}

func TestLocalRealKeywordOnlyAndCopy(t *testing.T) {
	c := newLocalCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	kw := c.ownIndices()[1]
	vecIndex := c.ownIndices()[0]

	// Keyword-only path (KB with vector indexing disabled): no embedding.
	src := sampleChunks("kb-kw-"+c.storeID, "docK", 2, 0)
	infos, _ := batchParams(src)
	if err := c.repo.BatchSave(ctx, infos, nil); err != nil {
		t.Fatal(err)
	}
	if n := c.count(kw, ""); n != 2 {
		t.Fatalf("keyword index count %d", n)
	}
	if _, ok := c.source(kw, "docK-c0")["embedding"]; ok {
		t.Fatal("keyword-only doc must not carry embedding")
	}

	// KB copy within the same store keeps vectors and remaps ids.
	kbSrc, kbDst := "kb-src-"+c.storeID, "kb-dst-"+c.storeID
	vs := sampleChunks(kbSrc, "docS", 3, 0)
	infos, params := batchParams(vs)
	if err := c.repo.BatchSave(ctx, infos, params); err != nil {
		t.Fatal(err)
	}
	c.raw(http.MethodPost, "/"+vecIndex+"/_refresh", "")
	chunkMap := map[string]string{}
	for _, ch := range vs {
		chunkMap[ch.info.ChunkID] = "copy-" + ch.info.ChunkID
	}
	if err := c.repo.CopyIndices(ctx, kbSrc, map[string]string{"docS": "docS-copy"}, chunkMap, kbDst, localDim, "manual"); err != nil {
		t.Fatal(err)
	}
	if n := c.count(vecIndex, fmt.Sprintf(`{"query":{"term":{"knowledge_base_id":%q}}}`, kbDst)); n != 3 {
		t.Fatalf("copied chunks %d", n)
	}
	if n := c.count(vecIndex, fmt.Sprintf(`{"query":{"term":{"knowledge_base_id":%q}}}`, kbSrc)); n != 3 {
		t.Fatalf("source chunks changed: %d", n)
	}
	got := c.source(vecIndex, "copy-docS-c1")
	if got["knowledge_id"] != "docS-copy" || len(got["embedding"].([]any)) != localDim {
		t.Fatalf("copied doc wrong: %v", got["knowledge_id"])
	}
}
