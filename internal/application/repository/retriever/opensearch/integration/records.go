package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/primind/primind/internal/types"
)

// Document is an independent oracle of the persisted contract, not a call to
// adapter toDoc. Unknown or missing fields fail replay rather than being lost.
type Document struct {
	ChunkID         string    `json:"chunk_id"`
	KnowledgeID     string    `json:"knowledge_id"`
	KnowledgeBaseID string    `json:"knowledge_base_id"`
	SourceID        string    `json:"source_id"`
	SourceType      int       `json:"source_type"`
	TagID           string    `json:"tag_id"`
	Content         string    `json:"content"`
	Enabled         bool      `json:"is_enabled"`
	Recommended     bool      `json:"is_recommended"`
	Embedding       []float32 `json:"embedding,omitempty"`
}

func (d Document) info() *types.IndexInfo {
	return &types.IndexInfo{
		ChunkID: d.ChunkID, SourceID: d.SourceID, SourceType: types.SourceType(d.SourceType),
		KnowledgeID: d.KnowledgeID, KnowledgeBaseID: d.KnowledgeBaseID, Content: d.Content,
		TagID: d.TagID, IsEnabled: d.Enabled, IsRecommended: d.Recommended,
	}
}

func decodeDocument(raw json.RawMessage, dim int) (Document, error) {
	var d Document
	var fields map[string]json.RawMessage
	if err := strictJSON(raw, &d); err != nil {
		return d, errors.New("unknown or malformed indexed source")
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return d, errors.New("invalid indexed source")
	}
	for _, k := range []string{
		"chunk_id", "knowledge_id", "knowledge_base_id", "source_id", "source_type",
		"tag_id", "content", "is_enabled", "is_recommended",
	} {
		v, ok := fields[k]
		if !ok || bytes.Equal(v, []byte("null")) {
			return d, errors.New("missing indexed source field")
		}
	}
	if !safeID.MatchString(d.ChunkID) || !safeID.MatchString(d.SourceID) || !safeID.MatchString(d.KnowledgeID) ||
		!safeID.MatchString(
			d.KnowledgeBaseID,
		) || d.SourceType < 0 || d.SourceType > int(types.ImageSourceType) || len(d.Embedding) != dim {
		return d, errors.New("invalid indexed identity/source type/dimension")
	}
	if dim == 0 {
		if _, ok := fields["embedding"]; ok {
			return d, errors.New("unexpected keyword embedding")
		}
	}
	if dim > 0 {
		nonzero := false
		for _, v := range d.Embedding {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return d, errors.New("non-finite vector")
			}
			nonzero = nonzero || v != 0
		}
		if !nonzero {
			return d, errors.New("zero vector")
		}
	}
	return d, nil
}

func digest(b []byte) string    { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (d Document) hash() string { b, _ := json.Marshal(d); return digest(b) }
func (d Document) vectorHash() string {
	h := sha256.New()
	var b [4]byte
	for _, f := range d.Embedding {
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(f))
		_, _ = h.Write(b[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RecordDigest is a content-free fingerprint of one indexed record.
type RecordDigest struct {
	ID            string `json:"id"`
	SourceID      string `json:"source_id"`
	SourceSHA256  string `json:"source_sha256"`
	ContentSHA256 string `json:"content_sha256"`
	VectorSHA256  string `json:"vector_float32_le_sha256"`
	Dimension     int    `json:"dimension"`
}

func manifest(docs map[string]Document) []RecordDigest {
	ids := make([]string, 0, len(docs))
	for id := range docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]RecordDigest, 0, len(ids))
	for _, id := range ids {
		d := docs[id]
		out = append(
			out,
			RecordDigest{id, d.SourceID, d.hash(), digest([]byte(d.Content)), d.vectorHash(), len(d.Embedding)},
		)
	}
	return out
}

func reconcile(expected, actual map[string]Document) error {
	if len(expected) != len(actual) {
		return errors.New("full reconciliation count mismatch")
	}
	for id, d := range expected {
		a, ok := actual[id]
		if !ok || a.hash() != d.hash() {
			return errors.New("full reconciliation ID/fields/vector mismatch")
		}
	}
	return nil
}

type reader struct {
	client *osapi.Client
	config Config
}

// All raw operations here are finite, exact-index evidence reads through the
// same managed client. Writes are exclusively Repository.Save/BatchSave.
func (r reader) json(ctx context.Context, method, path string, body any, out any) (int, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return 0, errors.New("invalid request body")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(payload))
	if err != nil {
		return 0, errors.New("invalid evidence request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Client.Perform(req)
	if err != nil {
		return 0, errors.New("evidence transport failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("evidence HTTP status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
	if err != nil || len(b) > 16<<20 {
		return resp.StatusCode, errors.New("evidence response incomplete or too large")
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return resp.StatusCode, errors.New("invalid evidence response JSON")
	}
	return resp.StatusCode, nil
}

func (r reader) alias(ctx context.Context, s Scope) error {
	if s.Dimension == 0 {
		return nil
	}
	var result map[string]struct {
		Aliases map[string]json.RawMessage `json:"aliases"`
	}
	if _, err := r.json(ctx, "GET", "/_alias/"+s.Alias, nil, &result); err != nil {
		return err
	}
	entry, ok := result[s.Physical]
	if !ok || len(result) != 1 {
		return errors.New("alias physical target changed or ambiguous")
	}
	raw, ok := entry.Aliases[s.Alias]
	if !ok {
		return errors.New("alias absent")
	}
	var attributes map[string]any
	if json.Unmarshal(raw, &attributes) != nil {
		return errors.New("invalid alias attributes")
	}
	for key, value := range attributes {
		if key != "is_write_index" || value != true {
			return errors.New("filtered/routed/non-writable alias not supported for replay")
		}
	}
	return nil
}

func (r reader) settings(ctx context.Context, s Scope) error {
	var settings map[string]struct {
		Settings map[string]json.RawMessage `json:"settings"`
		Defaults map[string]json.RawMessage `json:"defaults"`
	}
	if _, err := r.json(ctx, "GET", "/"+s.Physical+"/_settings?flat_settings=true&include_defaults=true", nil,
		&settings); err != nil {
		return err
	}
	x, ok := settings[s.Physical]
	if !ok || len(settings) != 1 {
		return errors.New("settings scope differs")
	}
	want := map[string]string{
		"index.number_of_shards":   fmt.Sprint(s.Index.NumberOfShards),
		"index.number_of_replicas": fmt.Sprint(s.Index.NumberOfReplicas),
		"index.refresh_interval":   s.RefreshInterval,
	}
	if s.Dimension > 0 {
		want["index.knn"] = "true"
		want["index.knn.algo_param.ef_search"] = fmt.Sprint(s.Index.HNSWEFSearch)
	}
	for key, value := range want {
		raw, ok := x.Settings[key]
		if !ok {
			raw, ok = x.Defaults[key]
		}
		var actual string
		if !ok || json.Unmarshal(raw, &actual) != nil || actual != value {
			return errors.New("effective index settings differ from frozen profile")
		}
	}
	var mappings map[string]struct {
		Mappings struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Source     struct {
				Enabled *bool `json:"enabled"`
			} `json:"_source"`
		} `json:"mappings"`
	}
	if _, err := r.json(ctx, "GET", "/"+s.Physical+"/_mapping", nil, &mappings); err != nil {
		return err
	}
	m, ok := mappings[s.Physical]
	if !ok || len(mappings) != 1 || (m.Mappings.Source.Enabled != nil && !*m.Mappings.Source.Enabled) {
		return errors.New("mapping/source scope differs")
	}
	if s.Dimension > 0 {
		var e struct {
			Type      string `json:"type"`
			Dimension int    `json:"dimension"`
			Method    struct {
				Name       string `json:"name"`
				Engine     string `json:"engine"`
				Space      string `json:"space_type"`
				Parameters struct {
					M  int `json:"m"`
					EF int `json:"ef_construction"`
				} `json:"parameters"`
			} `json:"method"`
		}
		if json.Unmarshal(m.Mappings.Properties["embedding"], &e) != nil || e.Type != "knn_vector" ||
			e.Dimension != s.Dimension || e.Method.Name != "hnsw" ||
			e.Method.Engine != s.Index.KNNEngine || e.Method.Space != "cosinesimil" ||
			e.Method.Parameters.M != s.Index.HNSWM ||
			e.Method.Parameters.EF != s.Index.HNSWEFConstruction {
			return errors.New("vector mapping differs from frozen profile")
		}
	}
	return nil
}

func (r reader) scan(
	ctx context.Context,
	s Scope,
	index string,
	dim int,
	exactChunk bool,
	missingOK bool,
) (map[string]Document, error) {
	return r.scanObserved(ctx, s, index, dim, exactChunk, missingOK, nil)
}

func (r reader) scanObserved(
	ctx context.Context,
	s Scope,
	index string,
	dim int,
	exactChunk bool,
	missingOK bool,
	revisions map[string]Revision,
) (map[string]Document, error) {
	query := map[string]any{"match_all": map[string]any{}}
	if exactChunk {
		query = map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"knowledge_base_id": s.KnowledgeBaseID}},
			map[string]any{"term": map[string]any{"knowledge_id": s.KnowledgeID}},
			map[string]any{"term": map[string]any{"chunk_id": s.ChunkID}},
		}}}
	}
	out := map[string]Document{}
	after := ""
	total := -1
	for page := 0; page <= r.config.MaxRecords/r.config.PageSize+1; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body := map[string]any{
			"size":             r.config.PageSize,
			"track_total_hits": true,
			"query":            query,
			"sort":             []any{map[string]any{"chunk_id": "asc"}},
		}
		if revisions != nil {
			body["version"] = true
			body["seq_no_primary_term"] = true
		}
		if after != "" {
			body["search_after"] = []string{after}
		}
		var response struct {
			TimedOut bool `json:"timed_out"`
			Shards   struct {
				Total      int `json:"total"`
				Successful int `json:"successful"`
				Failed     int `json:"failed"`
			} `json:"_shards"`
			Hits struct {
				Total struct {
					Value    int    `json:"value"`
					Relation string `json:"relation"`
				} `json:"total"`
				Hits []struct {
					ID          string          `json:"_id"`
					Index       string          `json:"_index"`
					Source      json.RawMessage `json:"_source"`
					Sort        []string        `json:"sort"`
					Version     *int64          `json:"_version"`
					SeqNo       *int64          `json:"_seq_no"`
					PrimaryTerm *int64          `json:"_primary_term"`
				} `json:"hits"`
			} `json:"hits"`
		}
		status, err := r.json(ctx, "POST", "/"+index+"/_search", body, &response)
		if status == 404 && missingOK && page == 0 {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if response.TimedOut || response.Shards.Total == 0 || response.Shards.Failed != 0 ||
			response.Shards.Successful != response.Shards.Total ||
			response.Hits.Total.Relation != "eq" ||
			response.Hits.Total.Value < 0 ||
			response.Hits.Total.Value > r.config.MaxRecords {
			return nil, errors.New("incomplete or over-budget scan")
		}
		if total < 0 {
			total = response.Hits.Total.Value
		}
		if response.Hits.Total.Value != total {
			return nil, errors.New("scan changed during reconciliation; quiescence violated")
		}
		if len(response.Hits.Hits) == 0 {
			if len(out) != total {
				return nil, errors.New("truncated scan")
			}
			return out, nil
		}
		for _, hit := range response.Hits.Hits {
			d, err := decodeDocument(hit.Source, dim)
			if err != nil {
				return nil, err
			}
			expectedIndex := s.Physical
			if index == s.keywordIndex() {
				expectedIndex = index
			}
			if hit.Index != expectedIndex || hit.ID != d.ChunkID || len(hit.Sort) != 1 || hit.Sort[0] != d.ChunkID ||
				hit.Sort[0] <= after ||
				(exactChunk && (d.ChunkID != s.ChunkID || d.KnowledgeBaseID != s.KnowledgeBaseID ||
					d.KnowledgeID != s.KnowledgeID)) {
				return nil, errors.New("scan identity/scope/order mismatch")
			}
			if _, duplicate := out[hit.ID]; duplicate {
				return nil, errors.New("duplicate ID in full scan")
			}
			out[hit.ID] = d
			if revisions != nil {
				v, e := revision(hit.Version, hit.SeqNo, hit.PrimaryTerm)
				if e != nil {
					return nil, e
				}
				revisions[hit.ID] = v
			}
			after = hit.Sort[0]
			if len(out) > total || len(out) > r.config.MaxRecords {
				return nil, errors.New("scan exceeded total/cap")
			}
		}
	}
	return nil, errors.New("scan page budget exhausted")
}
