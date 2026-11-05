//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	adapter "github.com/primind/primind/internal/application/repository/retriever/opensearch"
	"github.com/primind/primind/internal/types"
)

// faultProxy forwards TCP to the real cluster and can be cut (listener and
// live connections closed) and restored on the same port.
type faultProxy struct {
	t      *testing.T
	addr   string
	target string
	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	t.Helper()
	p := &faultProxy{t: t, target: target, conns: map[net.Conn]struct{}{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.serve(ln)
	t.Cleanup(p.down)
	return p
}

func (p *faultProxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.pipe(c)
		}
	}()
}

func (p *faultProxy) pipe(c net.Conn) {
	u, err := net.Dial("tcp", p.target)
	if err != nil {
		c.Close()
		return
	}
	p.mu.Lock()
	p.conns[c], p.conns[u] = struct{}{}, struct{}{}
	p.mu.Unlock()
	go func() { _, _ = io.Copy(u, c); u.Close() }()
	_, _ = io.Copy(c, u)
	c.Close()
}

func (p *faultProxy) down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	for c := range p.conns {
		c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

func (p *faultProxy) up() {
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatal(err)
	}
	p.serve(ln)
}

func targetHostPort(t *testing.T) string {
	raw := os.Getenv("PRIMIND_OS_LOCAL_URL")
	if raw == "" {
		t.Skip("PRIMIND_OS_LOCAL_URL not set")
	}
	return strings.TrimPrefix(strings.TrimRight(raw, "/"), "http://")
}

// AC-OS03-05: HTTP 200 bulk with one rejected item must not be reported as success.
func TestLocalRealBulkPartialFailure(t *testing.T) {
	c := newLocalCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	chunks := sampleChunks("kb-partial-"+c.storeID, "docP", 3, 0)
	// A keyword term over Lucene's 32766-byte limit fails only this item (HTTP 200, errors=true).
	chunks[1].info.TagID = strings.Repeat("x", 40000)
	infos, params := batchParams(chunks)

	err := c.repo.BatchSave(ctx, infos, params)
	var be *adapter.BulkWriteError
	if !errors.As(err, &be) {
		t.Fatalf("want BulkWriteError, got %v", err)
	}
	if be.Result.Confirmed != 2 || be.Result.Failed != 1 || be.Result.Unknown != 0 {
		t.Fatalf("outcome = %+v", be.Result)
	}
	failedAt := -1
	for _, it := range be.Result.Items {
		if it.State == "failed" {
			failedAt = it.Position
		}
	}
	if failedAt != 1 {
		t.Fatalf("failed item position %d, want 1 (items %+v, types %v)", failedAt, be.Result.Items, be.ErrorTypes)
	}
	if n := c.count(c.ownIndices()[0], ""); n != 2 {
		t.Fatalf("indexed %d, want the 2 confirmed items", n)
	}
	t.Logf("partial bulk: %v", err)
}

// AC-OS03-06: a cut connection gives an explicit transport error; after
// recovery the same batch is rewritten and counts reconcile without duplicates.
func TestLocalRealConnectionLossAndReconcile(t *testing.T) {
	proxy := newFaultProxy(t, targetHostPort(t))
	c := newLocalClusterVia(t, "http://"+proxy.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	kb := "kb-conn-" + c.storeID
	first := sampleChunks(kb, "docC", 4, 0)
	infos, params := batchParams(first[:2])
	if err := c.repo.BatchSave(ctx, infos, params); err != nil {
		t.Fatal(err)
	}

	proxy.down()
	infos, params = batchParams(first)
	err := c.repo.BatchSave(ctx, infos, params)
	if err == nil {
		t.Fatal("write during outage reported success")
	}
	var be *adapter.BulkWriteError
	if !errors.As(err, &be) || be.Result.Confirmed != 0 || be.Result.Unknown+be.Result.NotSent != 4 {
		t.Fatalf("outage error not explicit: %v", err)
	}
	if !errors.Is(err, adapter.ErrTransport) {
		t.Fatalf("outage error not classified as transport: %v", err)
	}
	if _, err := c.repo.Retrieve(ctx, types.RetrieveParams{
		RetrieverType: types.KeywordsRetrieverType,
		Query:         "sample", KnowledgeBaseIDs: []string{kb}, TopK: 5, AdditionalParams: map[string]any{"dim": localDim},
	}); err == nil {
		t.Fatal("retrieve during outage reported success")
	}
	t.Logf("outage error: %v", err)

	proxy.up()
	if err := c.repo.BatchSave(ctx, infos, params); err != nil {
		t.Fatalf("rewrite after recovery: %v", err)
	}
	if n := c.count(c.ownIndices()[0], ""); n != 4 {
		t.Fatalf("after recovery %d docs, want 4", n)
	}
	for i, ch := range first {
		if got := c.source(c.ownIndices()[0], ch.info.ChunkID)["content"]; got != ch.info.Content {
			t.Fatalf("chunk %d content %v", i, got)
		}
	}
}

// AC-OS03-06 characterisation: the adapter is last-writer-wins on _id =
// chunk_id with no version guard, so a delayed replay of an older write
// overwrites newer content and re-creates a deleted chunk. Ordering must be
// guaranteed by the caller (service/task layer); see opensearch-progress.md.
func TestLocalRealStaleReplayIsLastWriterWins(t *testing.T) {
	c := newLocalCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	idx := c.ownIndices()[0]
	ch := sampleChunks("kb-replay-"+c.storeID, "docR", 1, 0)[0]
	save := func(content string) {
		info := *ch.info
		info.Content = content
		if err := c.repo.Save(ctx, &info, map[string]any{"embedding": map[string][]float32{info.SourceID: ch.emb}}); err != nil {
			t.Fatal(err)
		}
	}
	save("v1 old")
	save("v2 new")
	save("v1 old") // delayed replay of the first write
	if got := c.source(idx, ch.info.ChunkID)["content"]; got != "v1 old" {
		t.Fatalf("expected last-writer-wins, got %v", got)
	}
	if err := c.repo.DeleteByChunkIDList(ctx, []string{ch.info.ChunkID}, localDim, "manual"); err != nil {
		t.Fatal(err)
	}
	save("v1 old") // replay after delete
	if n := c.count(idx, fmt.Sprintf(`{"query":{"ids":{"values":[%q]}}}`, ch.info.ChunkID)); n != 1 {
		t.Fatalf("expected replay to re-create the chunk, count %d", n)
	}
}
