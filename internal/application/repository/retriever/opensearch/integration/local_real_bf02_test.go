//go:build integration

package integration

// BF-02 retrieval response size against a developer OpenSearch:
//
//	PRIMIND_OS_LOCAL_URL=http://127.0.0.1:9200 go test -tags integration \
//	  ./internal/application/repository/retriever/opensearch/integration \
//	  -run '^TestLocalRealBF02' -count=1 -v -timeout=20m
//
// Indices use the primind_os_it_<random> prefix; only the indices a test
// created are deleted afterwards.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	adapter "github.com/primind/primind/internal/application/repository/retriever/opensearch"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	secutils "github.com/primind/primind/internal/utils"
)

const bf02Dim = 4096

// bf02Store is one random OpenSearch store shared by several repositories
// that differ only in client options (cap) and the deferred-content switch.
type bf02Store struct {
	t        *testing.T
	endpoint string
	storeID  string
	base     string
}

func newBF02Store(t *testing.T) *bf02Store {
	t.Helper()
	endpoint := os.Getenv("PRIMIND_OS_LOCAL_URL")
	if endpoint == "" {
		t.Skip("PRIMIND_OS_LOCAL_URL not set")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	secutils.SetSSRFWhitelistFromRaw(u.Hostname())
	t.Cleanup(func() { secutils.SetSSRFWhitelistFromRaw("") })
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	storeID := hex.EncodeToString(b)
	s := &bf02Store{
		t: t, endpoint: strings.TrimRight(endpoint, "/"), storeID: storeID,
		base: "primind_os_it_" + storeID[:12],
	}
	t.Cleanup(func() {
		for _, idx := range []string{fmt.Sprintf("%s_%d_v1", s.base, bf02Dim), s.base + "_keywords"} {
			req, _ := http.NewRequest(http.MethodDelete, s.endpoint+"/"+idx, nil)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
	})
	return s
}

// repo opens a repository on the store. capBytes 0 keeps the default cap.
func (s *bf02Store) repo(capBytes int64, deferContent bool) interfaces.RetrieveEngineRepository {
	s.t.Helper()
	var opts []adapter.ClientOption
	if capBytes > 0 {
		o := adapter.DefaultRuntimeOptions()
		o.MaxResponseBytes = capBytes
		opts = append(opts, adapter.WithRuntimeOptions(o))
	}
	client, err := adapter.NewOpenSearchClient(&types.ConnectionConfig{Addr: s.endpoint}, opts...)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { adapter.CloseIdleConnections(client) })
	repo, err := adapter.NewRepository(context.Background(), client, s.storeID,
		&types.IndexConfig{IndexName: "primind_os_it", NumberOfShards: 1, NumberOfReplicas: 0},
		adapter.WithDeferredContent(deferContent))
	if err != nil {
		s.t.Fatal(err)
	}
	return repo
}

func (s *bf02Store) refresh() {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/%s_%d_v1/_refresh", s.endpoint, s.base, bf02Dim), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func bf02Vector(rng *mrand.Rand) []float32 {
	v := make([]float32, bf02Dim)
	for i := range v {
		v[i] = float32(rng.NormFloat64())
	}
	return v
}

// bf02Text builds n bytes of searchable ASCII text around the query words.
func bf02Text(rng *mrand.Rand, n int) string {
	words := []string{"vehicle", "parking", "policy", "registration", "visitor", "garage", "permit", "rules"}
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(words[rng.IntN(len(words))])
		b.WriteByte(' ')
	}
	return b.String()[:n]
}

type bf02Doc struct {
	info *types.IndexInfo
	emb  []float32
}

// index writes docs in bulk batches small enough for the driver's bulk cap.
func (s *bf02Store) index(ctx context.Context, repo interfaces.RetrieveEngineRepository, docs []bf02Doc) {
	s.t.Helper()
	const batch = 40
	for start := 0; start < len(docs); start += batch {
		part := docs[start:min(start+batch, len(docs))]
		infos := make([]*types.IndexInfo, len(part))
		emb := make(map[string][]float32, len(part))
		for i, d := range part {
			infos[i] = d.info
			emb[d.info.SourceID] = d.emb
		}
		if err := repo.BatchSave(ctx, infos, map[string]any{"embedding": emb}); err != nil {
			s.t.Fatalf("bulk save at %d: %v", start, err)
		}
	}
	s.refresh()
}

// bf02Corpus builds kbs x perKB docs. contentLen returns each doc's text
// length. Every 10th doc is an image row, every 7th a generated-question row
// (own chunk ID: in OpenSearch _id is chunk_id, so a real question row would
// overwrite its parent chunk's row).
func bf02Corpus(rng *mrand.Rand, prefix string, kbs, perKB int, contentLen func() int) ([]bf02Doc, []string) {
	var docs []bf02Doc
	var kbIDs []string
	for k := range kbs {
		kb := fmt.Sprintf("%s-kb%02d", prefix, k)
		kbIDs = append(kbIDs, kb)
		for i := range perKB {
			id := fmt.Sprintf("%s-k%02d-c%04d", prefix, k, i)
			info := &types.IndexInfo{
				ID: id, ChunkID: id, SourceID: id, SourceType: types.ChunkSourceType,
				KnowledgeID: fmt.Sprintf("%s-doc%02d", prefix, k), KnowledgeBaseID: kb, KnowledgeType: "manual",
				IsEnabled: true, Content: "Vehicle policy handbook\n" + bf02Text(rng, contentLen()),
			}
			switch {
			case i%10 == 0:
				info.SourceType = types.ImageSourceType
				info.Content = "parking garage floor plan image caption"
			case i%7 == 0:
				info.SourceID = types.GeneratedQuestionSourceID(id, "q1")
				info.Content = "Vehicle policy handbook\nHow do I register a visitor vehicle for parking?"
			}
			docs = append(docs, bf02Doc{info: info, emb: bf02Vector(rng)})
		}
	}
	return docs, kbIDs
}

type bf02Run struct {
	hits      []*types.IndexWithScore
	truncated bool
	bytes     int64
	searches  int64
	elapsed   time.Duration
}

func bf02Retrieve(t *testing.T, repo interfaces.RetrieveEngineRepository, p types.RetrieveParams) bf02Run {
	t.Helper()
	ctx, stats := types.WithRetrievalStats(context.Background())
	started := time.Now()
	res, err := repo.Retrieve(ctx, p)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("%s retrieve: %v", p.RetrieverType, err)
	}
	return bf02Run{
		hits: res[0].Results, truncated: errors.Is(res[0].Error, adapter.ErrResultsTruncated),
		bytes: stats.ResponseBytes.Load(), searches: stats.Searches.Load(), elapsed: elapsed,
	}
}

func sameRanking(t *testing.T, label string, want, got []*types.IndexWithScore, n int) {
	t.Helper()
	if len(want) < n || len(got) < n {
		t.Fatalf("%s: want >= %d hits, have %d and %d", label, n, len(want), len(got))
	}
	for i := range n {
		a, b := want[i], got[i]
		if a.ChunkID != b.ChunkID || a.Score != b.Score || a.SourceType != b.SourceType ||
			a.KnowledgeBaseID != b.KnowledgeBaseID || a.SourceID != b.SourceID {
			t.Fatalf("%s: hit %d differs: %+v vs %+v", label, i, *a, *b)
		}
	}
}

// 6.2: 7 KBs x 200 chunks, 2-40 KiB text, 4096-dim vectors, with image and
// generated-question rows. With the switch off and on, the 500-candidate
// rankings match; on, the response is under 512 KiB and the matched content
// fetched for the kept hits equals what the inline response carried. A
// capped client (R1) returns a prefix of the full k-NN ranking.
func TestLocalRealBF02DeferredContentAndShrink(t *testing.T) {
	s := newBF02Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	rng := mrand.New(mrand.NewPCG(2026, 1125))
	docs, kbIDs := bf02Corpus(rng, "bf02", 7, 200, func() int { return 2<<10 + rng.IntN(38<<10) })
	inline := s.repo(0, false)
	started := time.Now()
	s.index(ctx, inline, docs)
	t.Logf("indexed %d docs (%d KBs, dim %d) in %s",
		len(docs), len(kbIDs), bf02Dim, time.Since(started).Round(time.Millisecond))
	deferred := s.repo(0, true)

	query := bf02Vector(rng)
	params := func(rt types.RetrieverType) types.RetrieveParams {
		return types.RetrieveParams{
			Query: "vehicle parking policy", Embedding: query, KnowledgeBaseIDs: kbIDs, TopK: 500, RetrieverType: rt,
		}
	}
	for _, rt := range []types.RetrieverType{types.VectorRetrieverType, types.KeywordsRetrieverType} {
		off := bf02Retrieve(t, inline, params(rt))
		on := bf02Retrieve(t, deferred, params(rt))
		if len(off.hits) != 500 || len(on.hits) != 500 {
			t.Fatalf("%s: hits off=%d on=%d, want 500", rt, len(off.hits), len(on.hits))
		}
		sameRanking(t, string(rt)+" off/on", off.hits, on.hits, 500)
		if on.bytes >= 512<<10 {
			t.Fatalf("%s: deferred response %d bytes, want < 512 KiB", rt, on.bytes)
		}
		// Kept hits (MatchCount 10): fetched text equals the inline text.
		kept := make([]string, 10)
		for i := range kept {
			kept[i] = on.hits[i].ChunkID
		}
		fetchStarted := time.Now()
		content, err := deferred.(interfaces.MatchedContentFetcher).FetchMatchedContent(ctx, kept)
		fetchElapsed := time.Since(fetchStarted)
		if err != nil {
			t.Fatal(err)
		}
		for i, id := range kept {
			if content[id] != off.hits[i].Content || content[id] == "" {
				t.Fatalf("%s: matched content of %s differs (len %d vs %d)",
					rt, id, len(content[id]), len(off.hits[i].Content))
			}
		}
		t.Logf("BF02 6.2 %-8s off: %8d B %6s | on: %7d B %6s + lookup(10) %s | "+
			"ranking, scores, matched content identical",
			rt, off.bytes, off.elapsed.Round(time.Millisecond), on.bytes, on.elapsed.Round(time.Millisecond),
			fetchElapsed.Round(time.Millisecond))
	}

	// R1 on a real cluster: a 3 MiB cap forces the fallback; the shrunk k-NN
	// result must be a prefix of the full ranking (k kept, size halved).
	capped := s.repo(3<<20, false)
	for _, rt := range []types.RetrieverType{types.VectorRetrieverType, types.KeywordsRetrieverType} {
		full := bf02Retrieve(t, inline, params(rt))
		shrunk := bf02Retrieve(t, capped, params(rt))
		if !shrunk.truncated || len(shrunk.hits) >= 500 || len(shrunk.hits) == 0 {
			t.Fatalf("%s: capped retrieve truncated=%t hits=%d", rt, shrunk.truncated, len(shrunk.hits))
		}
		sameRanking(t, string(rt)+" prefix", full.hits, shrunk.hits, len(shrunk.hits))
		t.Logf("BF02 R1  %-8s cap 3 MiB: %d searches, kept top %d of 500, %d B, %s; prefix of the full ranking",
			rt, shrunk.searches, len(shrunk.hits), shrunk.bytes, shrunk.elapsed.Round(time.Millisecond))
	}
}

// 6.4 (content gradient): 10 KBs x 50 chunks of fixed length, TopK 500 as the
// service asks for MatchCount 10 over 10 KBs. Records bytes, truncation,
// elapsed time and Go heap for each length, with the switch off and on.
func TestLocalRealBF02ContentGradient(t *testing.T) {
	for _, kib := range []int{8, 32, 64} {
		t.Run(fmt.Sprintf("%dKiB", kib), func(t *testing.T) {
			s := newBF02Store(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			rng := mrand.New(mrand.NewPCG(uint64(kib), 64))
			docs, kbIDs := bf02Corpus(rng, fmt.Sprintf("g%d", kib), 10, 50, func() int { return kib << 10 })
			inline := s.repo(0, false)
			s.index(ctx, inline, docs)
			deferred := s.repo(0, true)
			query := bf02Vector(rng)
			for _, rt := range []types.RetrieverType{types.VectorRetrieverType, types.KeywordsRetrieverType} {
				p := types.RetrieveParams{
					Query: "vehicle parking policy", Embedding: query, KnowledgeBaseIDs: kbIDs,
					TopK: 500, RetrieverType: rt,
				}
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				off := bf02Retrieve(t, inline, p)
				runtime.ReadMemStats(&after)
				on := bf02Retrieve(t, deferred, p)
				n := min(len(off.hits), len(on.hits))
				sameRanking(t, string(rt), off.hits, on.hits, n)
				t.Logf("BF02 6.4 %2d KiB %-8s off: hits %3d searches %d truncated %-5t %9d B %7s alloc %5.1f MiB | "+
					"on: hits %3d %7d B %6s",
					kib, rt, len(off.hits), off.searches, off.truncated, off.bytes, off.elapsed.Round(time.Millisecond),
					float64(after.TotalAlloc-before.TotalAlloc)/(1<<20),
					len(on.hits), on.bytes, on.elapsed.Round(time.Millisecond))
				if len(on.hits) != 500 {
					t.Fatalf("deferred run returned %d hits, want 500", len(on.hits))
				}
			}
		})
	}
}
