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

// Same-store document move rewrites knowledge_base_id in both the vector and
// the keyword index, clears tags, and leaves other documents of the source KB.
func TestLocalRealMoveWithinStore(t *testing.T) {
	c := newLocalCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	vecIndex, kw := c.ownIndices()[0], c.ownIndices()[1]
	kbSrc, kbDst := "kb-msrc-"+c.storeID, "kb-mdst-"+c.storeID

	moved := sampleChunks(kbSrc, "docM", 3, 0)
	moved[0].info.TagID = "tag-src"
	stay := sampleChunks(kbSrc, "docStay", 2, 4)
	for _, set := range [][]localChunk{moved, stay} {
		infos, params := batchParams(set)
		if err := c.repo.BatchSave(ctx, infos, params); err != nil {
			t.Fatal(err)
		}
	}
	kwOnly := sampleChunks(kbSrc, "docM", 1, 7)
	kwOnly[0].info.ChunkID, kwOnly[0].info.SourceID = "docM-kw", "docM-kw"
	kwInfos, _ := batchParams(kwOnly)
	if err := c.repo.BatchSave(ctx, kwInfos, nil); err != nil {
		t.Fatal(err)
	}

	mover, ok := c.repo.(interface {
		MoveKnowledgeIndices(context.Context, string, string, string, []string, int, string) error
	})
	if !ok {
		t.Fatal("repository does not support MoveKnowledgeIndices")
	}
	if err := mover.MoveKnowledgeIndices(ctx, kbSrc, kbDst, "docM", nil, localDim, "manual"); err != nil {
		t.Fatal(err)
	}
	byKB := func(index, kb string) int {
		return c.count(index, fmt.Sprintf(`{"query":{"term":{"knowledge_base_id":%q}}}`, kb))
	}
	if got := [4]int{byKB(vecIndex, kbSrc), byKB(vecIndex, kbDst), byKB(kw, kbSrc), byKB(kw, kbDst)}; got != [4]int{2, 3, 0, 1} {
		t.Fatalf("vec src/dst, kw src/dst = %v, want [2 3 0 1]", got)
	}
	if tag := c.source(vecIndex, "docM-c0")["tag_id"]; tag != "" {
		t.Fatalf("tag not cleared after move: %v", tag)
	}
	vr, err := c.repo.Retrieve(ctx, types.RetrieveParams{RetrieverType: types.VectorRetrieverType,
		Embedding: moved[1].emb, KnowledgeBaseIDs: []string{kbSrc}, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range chunkIDs(vr) {
		if strings.HasPrefix(id, "docM-") {
			t.Fatalf("moved chunk %s still hit in source KB", id)
		}
	}
}

// By-query operations issued right after a write (no refresh in between, as
// in the service's cleanup and enable/disable paths) must still see that write.
func TestLocalRealByQueryOpsSeeFreshWrites(t *testing.T) {
	c := newLocalCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	vecIndex := c.ownIndices()[0]
	kb := "kb-fresh-" + c.storeID
	byKnowledge := func(doc string) string {
		return fmt.Sprintf(`{"query":{"term":{"knowledge_id":%q}}}`, doc)
	}
	write := func(doc string) []localChunk {
		set := sampleChunks(kb, doc, 3, 0)
		infos, params := batchParams(set)
		if err := c.repo.BatchSave(ctx, infos, params); err != nil {
			t.Fatal(err)
		}
		return set
	}

	write("docDel")
	if err := c.repo.DeleteByKnowledgeIDList(ctx, []string{"docDel"}, localDim, "manual"); err != nil {
		t.Fatal(err)
	}
	if n := c.count(vecIndex, byKnowledge("docDel")); n != 0 {
		t.Errorf("delete right after write left %d chunks", n)
	}

	write("docDis")
	if err := c.repo.BatchUpdateChunkEnabledStatus(ctx, map[string]bool{"docDis-c0": false}); err != nil {
		t.Fatal(err)
	}
	if v := c.source(vecIndex, "docDis-c0")["is_enabled"]; v != false {
		t.Errorf("disable right after write not applied: is_enabled=%v", v)
	}

	src := write("docCp")
	chunkMap := map[string]string{}
	for _, ch := range src {
		chunkMap[ch.info.ChunkID] = "cp-" + ch.info.ChunkID
	}
	if err := c.repo.CopyIndices(ctx, kb, map[string]string{"docCp": "docCp2"}, chunkMap, kb+"-dst", localDim, "manual"); err != nil {
		t.Fatal(err)
	}
	if n := c.count(vecIndex, byKnowledge("docCp2")); n != 3 {
		t.Errorf("copy right after write copied %d of 3", n)
	}
}

// KB copy at 4096 dims: a 500-doc scan page is ~22MB of NDJSON, above the
// 10MB bulk cap, so the copy must write in size-bounded groups. 620 chunks
// also cross the scan page boundary.
func TestLocalRealCopyLargeDim(t *testing.T) {
	c := newLocalCluster(t)
	const dim, n = 4096, 620
	index := fmt.Sprintf("%s_%d_v1", c.base, dim)
	t.Cleanup(func() { c.raw(http.MethodDelete, "/"+index, "") })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	kbSrc, kbDst := "kb-big-src-"+c.storeID, "kb-big-dst-"+c.storeID
	chunkMap := map[string]string{}
	for off := 0; off < n; off += 40 {
		infos := make([]*types.IndexInfo, 0, 40)
		emb := map[string][]float32{}
		for i := off; i < min(off+40, n); i++ {
			id := fmt.Sprintf("big-%04d", i)
			infos = append(infos, &types.IndexInfo{ChunkID: id, SourceID: id, KnowledgeID: "docBig",
				KnowledgeBaseID: kbSrc, IsEnabled: i%7 != 0, Content: "large dim chunk " + id})
			v := make([]float32, dim)
			for j := range v {
				v[j] = float32((i*31+j)%997)/997 - 0.5
			}
			emb[id] = v
			chunkMap[id] = "cp-" + id
		}
		if err := c.repo.BatchSave(ctx, infos, map[string]any{"embedding": emb}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.repo.CopyIndices(ctx, kbSrc, map[string]string{"docBig": "docBig-copy"}, chunkMap, kbDst, dim, "manual"); err != nil {
		t.Fatal(err)
	}
	byKB := func(kb string) int {
		return c.count(index, fmt.Sprintf(`{"query":{"term":{"knowledge_base_id":%q}}}`, kb))
	}
	if s, d := byKB(kbSrc), byKB(kbDst); s != n || d != n {
		t.Fatalf("src=%d dst=%d, want %d each", s, d, n)
	}
	for _, i := range []int{0, 7, 499, 500, n - 1} {
		id := fmt.Sprintf("big-%04d", i)
		src, dst := c.source(index, id), c.source(index, "cp-"+id)
		if dst["content"] != src["content"] || dst["is_enabled"] != src["is_enabled"] || dst["knowledge_id"] != "docBig-copy" {
			t.Fatalf("chunk %s copied wrong: %v / %v / %v", id, dst["content"], dst["is_enabled"], dst["knowledge_id"])
		}
		sv, dv := src["embedding"].([]any), dst["embedding"].([]any)
		if len(dv) != dim || fmt.Sprint(sv) != fmt.Sprint(dv) {
			t.Fatalf("chunk %s vector differs (len %d)", id, len(dv))
		}
	}
}
