package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	adapter "github.com/primind/primind/internal/application/repository/retriever/opensearch"
)

// Revision identifies a document operation, not just its unchanged payload.
// Sequence zero is valid; absent/null metadata is not evidence of revision zero.
type Revision struct {
	Version     int64 `json:"version"`
	SeqNo       int64 `json:"seq_no"`
	PrimaryTerm int64 `json:"primary_term"`
}

func revision(version, seq, term *int64) (Revision, error) {
	if version == nil || seq == nil || term == nil || *version < 1 || *seq < 0 || *term < 1 {
		return Revision{}, errors.New("missing or invalid document revision")
	}
	return Revision{*version, *seq, *term}, nil
}

func (after Revision) advanced(before Revision) bool {
	// Retries may increment more than once; do not assume exactly +1. A lower
	// primary term is inconsistent with the same quiescent index/shard history.
	return after.Version > before.Version && after.SeqNo > before.SeqNo && after.PrimaryTerm >= before.PrimaryTerm
}

func (after Revision) reached(target Revision) (bool, error) {
	if after == target {
		return true, nil
	}
	if after.Version < target.Version && after.SeqNo < target.SeqNo && after.PrimaryTerm <= target.PrimaryTerm {
		return false, nil
	}
	return false, errors.New("search revision changed beyond or conflicts with quiescent replay target")
}

func (r reader) realtime(ctx context.Context, s Scope) (Document, Revision, error) {
	var response struct {
		ID          string          `json:"_id"`
		Index       string          `json:"_index"`
		Found       *bool           `json:"found"`
		Version     *int64          `json:"_version"`
		SeqNo       *int64          `json:"_seq_no"`
		PrimaryTerm *int64          `json:"_primary_term"`
		Source      json.RawMessage `json:"_source"`
	}
	path := "/" + s.Alias + "/_doc/" + s.ChunkID + "?realtime=true&refresh=false&preference=" + url.QueryEscape(
		r.config.RunID,
	)
	if _, err := r.json(ctx, "GET", path, nil, &response); err != nil {
		return Document{}, Revision{}, err
	}
	if response.Found == nil || !*response.Found || response.ID != s.ChunkID || response.Index != s.Physical {
		return Document{}, Revision{}, errors.New("realtime identity/physical scope mismatch")
	}
	v, err := revision(response.Version, response.SeqNo, response.PrimaryTerm)
	if err != nil {
		return Document{}, v, err
	}
	d, err := decodeDocument(response.Source, s.Dimension)
	if err != nil {
		return d, v, err
	}
	if d.ChunkID != s.ChunkID || d.KnowledgeID != s.KnowledgeID || d.KnowledgeBaseID != s.KnowledgeBaseID {
		return d, v, errors.New("realtime business scope mismatch")
	}
	return d, v, nil
}

func (r reader) noKeywordCopy(ctx context.Context, s Scope) error {
	// A wrong vector->keyword route must not hide behind that index's refresh
	// lag either. This exact-ID GET is absence evidence, followed by the scoped
	// search count below; it never refreshes or writes to either index.
	status, err := r.json(ctx, "GET", "/"+s.keywordIndex()+"/_doc/"+s.ChunkID+"?realtime=true&refresh=false", nil, nil)
	if status != 404 {
		if err != nil {
			return err
		}
		return errors.New("unexpected realtime keyword-only copy")
	}
	docs, err := r.scan(ctx, s, s.keywordIndex(), 0, true, true)
	if err != nil {
		return err
	}
	if len(docs) != 0 {
		return errors.New("unexpected keyword-only copy")
	}
	return nil
}

func (sess *session) visibleRevision(
	ctx context.Context,
	s Scope,
	expected Document,
	target Revision,
) (docs map[string]Document, observed Revision, err error) {
	err = adapter.WaitForVisibility(
		ctx,
		time.Duration(sess.config.VisibilitySeconds)*time.Second,
		time.Duration(sess.config.PollMilliseconds)*time.Millisecond,
		func(check context.Context) (bool, error) {
			if e := sess.alias(check, s); e != nil {
				return false, e
			}
			revisions := map[string]Revision{}
			var e error
			docs, e = sess.scanObserved(check, s, s.Alias, s.Dimension, true, false, revisions)
			if e != nil {
				return false, e
			}
			if len(docs) == 0 {
				return false, nil
			}
			if len(docs) != 1 {
				return false, errors.New("exact Chunk must occur once")
			}
			observed = revisions[s.ChunkID]
			fresh, e := observed.reached(target)
			if e != nil || !fresh {
				return false, e
			}
			if e = reconcile(map[string]Document{s.ChunkID: expected}, docs); e != nil {
				return false, e
			}
			if e = sess.noKeywordCopy(check, s); e != nil {
				return false, e
			}
			// Detect an intervening operation (including an equal-payload one) before
			// accepting the search receipt. Scope is explicitly required to be quiet.
			d, v, e := sess.realtime(check, s)
			if e != nil {
				return false, e
			}
			if v != target || d.hash() != expected.hash() {
				return false, errors.New("realtime state changed during visibility check")
			}
			return true, nil
		},
	)
	return
}
