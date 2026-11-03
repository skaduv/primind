package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/primind/primind/internal/types"
	secutils "github.com/primind/primind/internal/utils"
)

type Query struct {
	ID              string `json:"id"`
	Text            string `json:"text"`
	DisableVector   bool   `json:"disable_vector_match"`
	DisableKeywords bool   `json:"disable_keywords_match"`
}

var fixedQueries = []Query{
	{"Q-KW", "OSFIXA20261022", true, false},
	{"Q-VEC", "泵 PUMP_A 发生轴封渗漏时，工单要求更换什么？", false, true},
	{"Q-HYB", "OSFIXA20261022，WO_A_001 要更换的部件号是什么？", false, false},
	{"Q-QA", "给出泵 PUMP_A 轴封渗漏应更换的部件号、工单号并注明来源。", false, false},
	{"Q-NEG", "资料是否给出了压缩机 COMP_C 的齿轮部件号？", false, false},
}

type ApplicationEvidence struct {
	QueryID        string   `json:"query_id"`
	RequestSHA256  string   `json:"request_sha256"`
	ResponseSHA256 string   `json:"response_sha256"`
	ReturnedIDs    []string `json:"returned_ids"`
	EvidenceAIDs   []string `json:"evidence_a_ids"`
}

type appProbe struct {
	client *http.Client
	config Application
	token  string
	pool   *http.Transport
}

func newAppProbe(a Application) (*appProbe, error) {
	token := os.Getenv(a.TokenEnv)
	if token == "" {
		return nil, errors.New("dedicated application credential unresolved")
	}
	cfg := secutils.DefaultSSRFSafeHTTPClientConfig()
	pool := secutils.NewSSRFSafeTransport(cfg)
	pool.MaxConnsPerHost = 2
	pool.MaxIdleConns = 2
	pool.MaxIdleConnsPerHost = 2
	pool.IdleConnTimeout = 90 * time.Second
	client := secutils.NewSSRFSafeHTTPClientWithTransport(cfg, pool)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect forbidden in scoped probe") }
	return &appProbe{client: client, config: a, token: token, pool: pool}, nil
}

func (p *appProbe) close() {
	if p.pool != nil {
		p.pool.CloseIdleConnections()
	} else {
		p.client.CloseIdleConnections()
	}
}

func oracleRetrieval(a Application, results []*types.SearchResult) ([]string, []string, error) {
	allowed := map[string]bool{}
	for _, id := range a.EvidenceAChunkIDs {
		allowed[id] = true
	}
	seen := map[string]bool{}
	returned := []string{}
	evidence := []string{}
	for _, hit := range results {
		if hit == nil || hit.KnowledgeBaseID != a.KnowledgeBaseID || hit.ID == "" {
			return nil, nil, errors.New("application result outside frozen KB")
		}
		ids := hit.SubChunkID
		if len(ids) == 0 {
			ids = []string{hit.ID}
		}
		for _, id := range ids {
			if seen[id] {
				return nil, nil, errors.New("duplicate effective retrieval chunk")
			}
			seen[id] = true
			returned = append(returned, id)
			if hit.KnowledgeID == a.KnowledgeAID && allowed[id] {
				evidence = append(evidence, id)
			}
		}
	}
	if len(evidence) == 0 {
		return returned, evidence, errors.New("independent A evidence oracle not met")
	}
	return returned, evidence, nil
}

func (p *appProbe) run(ctx context.Context, q Query) (ApplicationEvidence, error) {
	evidence := ApplicationEvidence{QueryID: q.ID}
	a := p.config
	body, _ := json.Marshal(types.SearchParams{QueryText: q.Text, KnowledgeBaseIDs: []string{a.KnowledgeBaseID}, MatchCount: a.MatchCount,
		VectorThreshold: a.VectorThreshold, KeywordThreshold: a.KeywordThreshold, DisableVectorMatch: q.DisableVector, DisableKeywordsMatch: q.DisableKeywords})
	evidence.RequestSHA256 = digest(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(a.Endpoint, "/")+"/api/v1/knowledge-bases/"+a.KnowledgeBaseID+"/hybrid-search", bytes.NewReader(body))
	if err != nil {
		return evidence, errors.New("invalid application request")
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return evidence, errors.New("application transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return evidence, errors.New("application HTTP failure; stop load")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return evidence, errors.New("application response incomplete/oversized")
	}
	evidence.ResponseSHA256 = digest(raw)
	var result struct {
		Success bool                  `json:"success"`
		Data    []*types.SearchResult `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.Success {
		return evidence, errors.New("application unsuccessful response")
	}
	evidence.ReturnedIDs, evidence.EvidenceAIDs, err = oracleRetrieval(a, result.Data)
	return evidence, err
}
