package integration

import (
	"context"
	"errors"
	"net"
	"net/http/httptrace"
	"os"
	"sync"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	adapter "github.com/primind/primind/internal/application/repository/retriever/opensearch"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

type connectionStats struct {
	mu           sync.Mutex
	seen         map[net.Conn]bool
	Gets, Reused int
}

func (s *connectionStats) context(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.seen == nil {
			s.seen = map[net.Conn]bool{}
		}
		s.seen[i.Conn] = true
		s.Gets++
		if i.Reused {
			s.Reused++
		}
	}})
}
func (s *connectionStats) snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]int{"distinct_connections_seen": len(s.seen), "got_connection": s.Gets, "reused": s.Reused}
}

type session struct {
	reader
	repo interfaces.RetrieveEngineRepository
}

func clientConfig(c Config) (*types.ConnectionConfig, error) {
	config := &types.ConnectionConfig{Addr: c.Endpoint, InsecureSkipVerify: c.InsecureSkipVerify}
	if !c.AnonymousAuthorized {
		config.Username = os.Getenv(c.UsernameEnv)
		config.Password = os.Getenv(c.PasswordEnv)
		if config.Username == "" || config.Password == "" {
			return nil, errors.New("dedicated credential reference unresolved")
		}
	}
	return config, nil
}

func openSession(ctx context.Context, c Config, s Scope, budget *adapter.RequestBudget, observer func(adapter.WriteObservation)) (*session, error) {
	config, err := clientConfig(c)
	if err != nil {
		return nil, err
	}
	client, err := adapter.NewOpenSearchClient(config, adapter.WithRequestBudget(budget))
	if err != nil {
		return nil, errors.New("formal client configuration rejected")
	}
	r := reader{client, c}
	var info struct {
		ClusterUUID string `json:"cluster_uuid"`
		Version     struct {
			Number       string `json:"number"`
			Distribution string `json:"distribution"`
		} `json:"version"`
	}
	if _, err = r.json(ctx, "GET", "/", nil, &info); err == nil && (info.ClusterUUID != c.ClusterUUID || info.Version.Number != c.Version || info.Version.Distribution != "opensearch") {
		err = errors.New("authorized cluster/version mismatch")
	}
	if err != nil {
		adapter.CloseIdleConnections(client)
		return nil, err
	}
	repo, err := adapter.NewRepository(ctx, client, s.StoreID, &s.Index, adapter.WithWriteObserver(observer))
	if err != nil {
		adapter.CloseIdleConnections(client)
		return nil, errors.New("formal repository probe failed")
	}
	return &session{reader: r, repo: repo}, nil
}

func closeClients(clients []*osapi.Client) {
	for _, c := range clients {
		adapter.CloseIdleConnections(c)
	}
}

func parameters(docs []Document) ([]*types.IndexInfo, map[string]any) {
	infos := make([]*types.IndexInfo, 0, len(docs))
	embedding := map[string][]float32{}
	enabled := map[string]bool{}
	for _, d := range docs {
		infos = append(infos, d.info())
		if len(d.Embedding) > 0 {
			embedding[d.SourceID] = d.Embedding
		}
		enabled[d.ChunkID] = d.Enabled
	}
	return infos, map[string]any{"embedding": embedding, "chunk_enabled": enabled}
}

type ReplayStep struct {
	Action         string         `json:"action"`
	Status         string         `json:"status"`
	ElapsedSeconds float64        `json:"elapsed_seconds"`
	Alias          string         `json:"alias"`
	Physical       string         `json:"physical"`
	Records        []RecordDigest `json:"records"`
	KeywordCopies  int            `json:"keyword_copies"`
	BeforeRevision *Revision      `json:"before_revision,omitempty"`
	AfterRevision  *Revision      `json:"after_revision,omitempty"`
	SearchRevision *Revision      `json:"search_revision,omitempty"`
}

type ReplayResult struct {
	RunID       string                 `json:"run_id"`
	Status      string                 `json:"status"`
	Steps       []ReplayStep           `json:"steps"`
	Budget      adapter.BudgetSnapshot `json:"budget"`
	Connections map[string]int         `json:"connections"`
}

func runReplay(parent context.Context, c Config) (result ReplayResult, err error) {
	result = ReplayResult{RunID: c.RunID, Status: "FAIL"}
	if err = c.validate(time.Now()); err != nil {
		return result, err
	}
	if c.Mode != "replay" {
		return result, errors.New("replay mode required")
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(c.MaxSeconds)*time.Second)
	defer cancel()
	stats := &connectionStats{}
	ctx = stats.context(ctx)
	budget, _ := adapter.NewRequestBudget(c.TotalRequests, c.WriteRequests, c.Waiters)
	defer func() { result.Budget = budget.Snapshot(); result.Connections = stats.snapshot() }()
	s := *c.Replay
	sess, err := openSession(ctx, c, s, budget, nil)
	if err != nil {
		return result, err
	}
	defer adapter.CloseIdleConnections(sess.client)
	if err = sess.settings(ctx, s); err != nil {
		return result, err
	}
	if err = sess.alias(ctx, s); err != nil {
		return result, err
	}
	d, baseline, err := sess.realtime(ctx, s)
	if err != nil {
		return result, err
	}
	before, visible, err := sess.visibleRevision(ctx, s, d, baseline)
	if err != nil {
		return result, err
	}
	result.Steps = append(result.Steps, ReplayStep{Action: "baseline", Status: "PASS", Alias: s.Alias, Physical: s.Physical, Records: manifest(before), AfterRevision: &baseline, SearchRevision: &visible})
	// Only this existing real document supplies its vector. There is no synthetic
	// fallback. SourceID and ChunkID may differ and remain separate keys.
	previous := baseline
	for _, action := range []string{"Save-1", "Save-2", "BatchSave-1", "BatchSave-2"} {
		step, next, e := replayStep(ctx, sess, s, d, previous, action)
		result.Steps = append(result.Steps, step)
		if e != nil {
			return result, e
		}
		previous = next
	}
	result.Status = "PASS"
	return result, nil
}

func replayStep(ctx context.Context, sess *session, s Scope, original Document, previous Revision, action string) (step ReplayStep, next Revision, err error) {
	started := time.Now()
	step = ReplayStep{Action: action, Status: "FAIL", Alias: s.Alias, Physical: s.Physical}
	defer func() { step.ElapsedSeconds = time.Since(started).Seconds() }()
	if err = sess.alias(ctx, s); err != nil {
		return
	}
	d, pre, e := sess.realtime(ctx, s)
	if e != nil {
		err = e
		return
	}
	step.BeforeRevision = &pre
	if pre != previous || d.hash() != original.hash() {
		err = errors.New("target changed before replay write")
		return
	}
	infos, params := parameters([]Document{original})
	if action == "Save-1" || action == "Save-2" {
		err = sess.repo.Save(ctx, infos[0], params)
	} else {
		err = sess.repo.BatchSave(ctx, infos, params)
	}
	if err != nil {
		err = errors.New("replay stopped on write failure; outcome may be unknown")
		return
	}
	d, post, e := sess.realtime(ctx, s)
	if e != nil {
		err = e
		return
	}
	step.AfterRevision = &post
	if !post.advanced(pre) {
		err = errors.New("acknowledged write did not advance the exact target revision")
		return
	}
	if d.hash() != original.hash() {
		err = errors.New("post-write realtime fields/vector mismatch")
		return
	}
	actual, searched, e := sess.visibleRevision(ctx, s, original, post)
	step.SearchRevision = &searched
	if e != nil {
		err = e
		return
	}
	step.Status = "PASS"
	step.Records = manifest(actual)
	next = post
	return
}
