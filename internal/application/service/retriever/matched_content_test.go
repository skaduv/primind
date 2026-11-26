package retriever

import (
	"context"
	"testing"

	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

// fetchingRepo is a repository with the optional MatchedContentFetcher
// capability; the embedded interface is nil because only the forwarded
// methods are called.
type fetchingRepo struct {
	interfaces.RetrieveEngineRepository
	deferContent bool
}

func (r *fetchingRepo) DefersMatchedContent() bool { return r.deferContent }

func (r *fetchingRepo) FetchMatchedContent(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		out[id] = "text of " + id
	}
	return out, nil
}

type plainRepo struct {
	interfaces.RetrieveEngineRepository
}

func TestKVHybridEngineForwardsMatchedContent(t *testing.T) {
	ctx := context.Background()
	engine := NewKVHybridRetrieveEngine(&fetchingRepo{deferContent: true}, types.OpenSearchRetrieverEngineType)
	f, ok := engine.(interfaces.MatchedContentFetcher)
	if !ok || !f.DefersMatchedContent() {
		t.Fatal("hybrid engine must expose the repository's deferred content")
	}
	got, err := f.FetchMatchedContent(ctx, []string{"c1"})
	if err != nil || got["c1"] != "text of c1" {
		t.Fatalf("FetchMatchedContent = %v, %v", got, err)
	}

	off := NewKVHybridRetrieveEngine(&fetchingRepo{}, types.OpenSearchRetrieverEngineType)
	if off.(interfaces.MatchedContentFetcher).DefersMatchedContent() {
		t.Fatal("switch off must not defer")
	}
	plain := NewKVHybridRetrieveEngine(&plainRepo{}, types.PostgresRetrieverEngineType)
	if plain.(interfaces.MatchedContentFetcher).DefersMatchedContent() {
		t.Fatal("a repository without the capability must not defer")
	}
	got, err = plain.(interfaces.MatchedContentFetcher).FetchMatchedContent(ctx, []string{"c1"})
	if err != nil || len(got) != 0 {
		t.Fatalf("plain FetchMatchedContent = %v, %v", got, err)
	}

	composite := &CompositeRetrieveEngine{engineInfos: []*engineInfo{
		{retrieveEngine: engine, retrieverType: []types.RetrieverType{types.VectorRetrieverType}},
		{retrieveEngine: engine, retrieverType: []types.RetrieverType{types.KeywordsRetrieverType}},
		{retrieveEngine: plain, retrieverType: []types.RetrieverType{types.KeywordsRetrieverType}},
	}}
	if !composite.DefersMatchedContent() || len(composite.deferringFetchers()) != 1 {
		t.Fatalf("composite deferring fetchers = %d, want 1 (deduplicated)", len(composite.deferringFetchers()))
	}
	if got, err := composite.FetchMatchedContent(ctx, []string{"a", "b"}); err != nil || len(got) != 2 {
		t.Fatalf("composite FetchMatchedContent = %v, %v", got, err)
	}
}
