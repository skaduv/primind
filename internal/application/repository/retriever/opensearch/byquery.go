package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/primind/primind/internal/logger"
)

// inspectByQueryResponse parses an _update_by_query / _delete_by_query
// response and surfaces partial failures using bounded, safe error types.
// Server-provided IDs, reasons and unknown types are never logged or returned.
// A non-zero version_conflicts count with no hard failures
// is logged as a warning but not treated as an error.
func inspectByQueryResponse(body io.Reader) error {
	return inspectByQueryResult(body, false)
}

func inspectByQueryResult(body io.Reader, requireComplete bool) error {
	var r struct {
		Total            *int64 `json:"total"`
		Updated          *int64 `json:"updated"`
		TimedOut         bool   `json:"timed_out"`
		VersionConflicts int    `json:"version_conflicts"`
		Failures         []struct {
			Cause struct {
				Type string `json:"type"`
			} `json:"cause"`
		} `json:"failures"`
	}
	if err := json.NewDecoder(body).Decode(&r); err != nil {
		return fmt.Errorf("opensearch: parse by-query response: %w", ErrTransport)
	}
	if requireComplete && (r.TimedOut || r.VersionConflicts != 0) {
		return fmt.Errorf("opensearch: incomplete move (timed_out=%t, version_conflicts=%d): %w",
			r.TimedOut, r.VersionConflicts, ErrTransport)
	}
	if requireComplete && (r.Total == nil || r.Updated == nil || *r.Total < 0 || *r.Updated != *r.Total) {
		return fmt.Errorf("opensearch: incomplete move document counts: %w", ErrTransport)
	}
	log := logger.GetLogger(context.Background())
	if len(r.Failures) == 0 {
		if r.VersionConflicts > 0 {
			log.Warnf("[OpenSearch] by-query had %d version conflicts (proceeded)", r.VersionConflicts)
		}
		return nil
	}
	var msgs []string
	for _, f := range r.Failures {
		if len(msgs) < 5 {
			msgs = append(msgs, safeBulkErrorType(f.Cause.Type))
		}
	}
	return fmt.Errorf("opensearch: by-query partial failure (%d failed, first 5: %s): %w",
		len(r.Failures), strings.Join(msgs, "; "), ErrTransport)
}

// A delete has its own completion counter; update/move's updated field is not
// evidence that a deletion completed. HTTP 200 alone is insufficient.
func inspectDeleteByQueryResponse(body io.Reader) error {
	var result struct {
		Total            *int64            `json:"total"`
		Deleted          *int64            `json:"deleted"`
		TimedOut         bool              `json:"timed_out"`
		VersionConflicts int64             `json:"version_conflicts"`
		Failures         []json.RawMessage `json:"failures"`
	}
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		return ErrTransport
	}
	if result.Total == nil || result.Deleted == nil || *result.Total < 0 || *result.Deleted != *result.Total ||
		result.TimedOut ||
		result.VersionConflicts != 0 ||
		len(result.Failures) != 0 {
		return fmt.Errorf("opensearch: incomplete delete: %w", ErrTransport)
	}
	return nil
}

// refreshBeforeByQuery makes writes acknowledged so far visible to a
// following _delete_by_query / _update_by_query / scan. Those operations
// read a search snapshot, so without this a chunk written moments earlier
// (bulk uses refresh=false) is silently skipped: a delete leaves residue, a
// disable leaves the chunk retrievable, a move or copy misses documents.
// Missing indices are not an error; the by-query call reports those itself.
func (r *Repository) refreshBeforeByQuery(ctx context.Context, indices ...string) error {
	allow, ignore := true, true
	resp, err := r.client.Indices.Refresh(ctx, &osapi.IndicesRefreshReq{
		Indices: indices,
		Params:  osapi.IndicesRefreshParams{AllowNoIndices: &allow, IgnoreUnavailable: &ignore},
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("opensearch: refresh before by-query: %w", wrapTransport(err))
	}
	if resp != nil {
		drainAndClose(resp.Inspect().Response.Body)
	}
	return nil
}
