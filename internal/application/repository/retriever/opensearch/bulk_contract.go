package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/primind/primind/internal/types"
)

const maxBulkBytes = 10 << 20
const maxBulkItems = 1000

// Invalid UTF-8 replacement is encoder-version dependent. Determine its
// constant width once, avoiding a tiny allocation for every invalid byte.
var invalidUTF8JSONSize = func() int {
	b, _ := json.Marshal("\xff")
	return len(b) - 2
}()

type ItemOutcome struct {
	Position int
	State    string
	Status   int
}

// WriteObservation contains no content, vectors, credentials or server error
// reasons. Positions refer to the submitted input; a rejected oversized batch
// uses NotSent=Total without allocating an outcome entry for every input.
type WriteObservation struct {
	Total, Bytes, Confirmed, Failed, Unknown, NotSent int
	Attempts                                          int64
	Duration                                          time.Duration
	Items                                             []ItemOutcome
}

type BulkWriteError struct {
	Result     WriteObservation
	Cause      error
	ErrorTypes []string
}

func (e *BulkWriteError) Error() string {
	return fmt.Sprintf("opensearch: bulk outcome (%d confirmed, %d items failed, %d unknown, %d not sent; types=%s): %v", e.Result.Confirmed, e.Result.Failed, e.Result.Unknown, e.Result.NotSent, strings.Join(e.ErrorTypes, ","), e.Cause)
}
func (e *BulkWriteError) Unwrap() error { return e.Cause }

// WithWriteObserver supplies synchronous, concurrency-safe-by-caller telemetry.
// Keep callbacks fast; only bounded, non-sensitive measurements are provided.
func WithWriteObserver(f func(WriteObservation)) Option {
	return func(r *Repository) { r.writeObserver = f }
}

func bulkUnconfirmed(n int, sent bool, cause error) *BulkWriteError {
	e := &BulkWriteError{Result: WriteObservation{Total: n}, Cause: cause}
	if sent {
		e.Result.Unknown = n
		e.Result.Items = make([]ItemOutcome, n)
		for i := range e.Result.Items {
			e.Result.Items[i] = ItemOutcome{Position: i, State: "unknown"}
		}
	} else {
		e.Result.NotSent = n
	}
	return e
}

// jsonSize checks the exact encoding/json size before it can allocate an
// arbitrary string-sized buffer. This supports only the driver's document
// projection (strings, bounded vectors, bools and integers).
func jsonSize(v any, remaining int) (int, error) {
	if remaining < 0 {
		return 0, ErrBatchTooLarge
	}
	n := 0
	add := func(k int) error {
		if k > remaining-n {
			return ErrBatchTooLarge
		}
		n += k
		return nil
	}
	switch x := v.(type) {
	case string:
		if err := add(2); err != nil {
			return 0, err
		}
		for i := 0; i < len(x); {
			c := x[i]
			size, step := 1, 1
			switch {
			case c == '"' || c == '\\' || c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t':
				size = 2
			case c < 0x20 || c == '<' || c == '>' || c == '&':
				size = 6
			case c >= utf8.RuneSelf:
				r, s := utf8.DecodeRuneInString(x[i:])
				step = s
				size = s
				if r == utf8.RuneError && s == 1 {
					size = invalidUTF8JSONSize
				} else if r == '\u2028' || r == '\u2029' {
					size = 6
				}
			}
			if err := add(size); err != nil {
				return 0, err
			}
			i += step
		}
	case map[string]any:
		if err := add(2); err != nil {
			return 0, err
		}
		first := true
		for key, value := range x {
			if !first {
				if err := add(1); err != nil {
					return 0, err
				}
			}
			first = false
			k, err := jsonSize(key, remaining-n)
			if err != nil {
				return 0, err
			}
			if err = add(k + 1); err != nil {
				return 0, err
			}
			k, err = jsonSize(value, remaining-n)
			if err != nil {
				return 0, err
			}
			if err = add(k); err != nil {
				return 0, err
			}
		}
	case []float32:
		if len(x) > 16000 {
			return 0, ErrDimensionMismatch
		}
		b, err := json.Marshal(x)
		if err != nil {
			return 0, fmt.Errorf("opensearch: invalid vector encoding: %w", ErrConfigInvalid)
		}
		if err = add(len(b)); err != nil {
			return 0, err
		}
	case int, bool:
		b, err := json.Marshal(x)
		if err != nil {
			return 0, err
		}
		if err = add(len(b)); err != nil {
			return 0, err
		}
	default:
		return 0, ErrConfigInvalid
	}
	return n, nil
}

// splitForBulk partitions infos into consecutive groups that each satisfy
// BatchSave's limits: at most maxBulkItems documents, the pre-marshal
// estimate (estimateBulkBodyBytes) and the exact encoded NDJSON size
// (maxBulkBytes). Sizes come from the same projection BatchSave sends, so the
// split follows the actual vector dimension instead of a fixed count. A
// document that alone exceeds a limit gets a group of its own; BatchSave then
// rejects it explicitly.
func splitForBulk(alias string, infos []*types.IndexInfo, embBySource map[string][]float32) ([][]*types.IndexInfo, error) {
	var groups [][]*types.IndexInfo
	start, size, dim := 0, 0, 0
	for i, info := range infos {
		emb := embBySource[info.SourceID]
		action := map[string]any{"index": map[string]any{"_index": alias, "_id": info.ChunkID}}
		n := 0
		for _, value := range []any{action, toDoc(info, emb, info.IsEnabled)} {
			k, err := jsonSize(value, maxBulkBytes)
			if err != nil && !errors.Is(err, ErrBatchTooLarge) {
				return nil, err
			}
			n += k + 1
		}
		count := i - start + 1
		if count > 1 && (count > maxBulkItems || size+n > maxBulkBytes ||
			estimateBulkBodyBytes(count, max(dim, len(emb))) > maxBulkBytes) {
			groups = append(groups, infos[start:i])
			start, size, dim = i, 0, 0
		}
		size += n
		dim = max(dim, len(emb))
	}
	if start < len(infos) {
		groups = append(groups, infos[start:])
	}
	return groups, nil
}

func buildBulkBody(alias string, infos []*types.IndexInfo, emb [][]float32, params map[string]any) ([]byte, error) {
	return buildBulkBodyContext(context.Background(), alias, infos, emb, params)
}

func buildBulkBodyContext(ctx context.Context, alias string, infos []*types.IndexInfo, emb [][]float32, params map[string]any) ([]byte, error) {
	if len(infos) > maxBulkItems {
		return nil, ErrBatchTooLarge
	}
	if len(emb) != len(infos) {
		return nil, ErrConfigInvalid
	}
	var buf bytes.Buffer
	for i, info := range infos {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if info == nil {
			return nil, ErrConfigInvalid
		}
		action := map[string]any{"index": map[string]any{"_index": alias, "_id": info.ChunkID}}
		doc := toDoc(info, emb[i], lookupChunkEnabled(params, info.ChunkID, info.IsEnabled))
		for _, value := range []any{action, doc} {
			n, err := jsonSize(value, maxBulkBytes-buf.Len()-1)
			if err != nil {
				return nil, err
			}
			b, err := json.Marshal(value)
			if err != nil {
				return nil, ErrConfigInvalid
			}
			if len(b) != n || len(b)+1 > maxBulkBytes-buf.Len() {
				return nil, ErrBatchTooLarge
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes(), nil
}

type bulkResponseItem struct {
	ID     string          `json:"_id"`
	Status int             `json:"status"`
	Error  json.RawMessage `json:"error"`
}

// expected is the actual wire order, including duplicate stable IDs if supplied.
// The no-expected form is retained for existing parser unit tests only.
func inspectBulkResponse(body io.Reader, expected ...string) error {
	var response struct {
		Errors *bool                         `json:"errors"`
		Items  []map[string]bulkResponseItem `json:"items"`
	}
	dec := json.NewDecoder(body)
	if err := dec.Decode(&response); err != nil {
		return bulkUnconfirmed(len(expected), true, ErrTransport)
	}
	if expected == nil {
		expected = make([]string, len(response.Items))
		for i, item := range response.Items {
			expected[i] = item["index"].ID
		}
	}
	result := bulkUnconfirmed(len(expected), true, ErrTransport)
	invalid := response.Errors == nil || len(response.Items) != len(expected)
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		invalid = true
	}
	for i, item := range response.Items {
		if i >= len(expected) {
			break
		}
		det, ok := item["index"]
		if !ok || len(item) != 1 || det.ID != expected[i] {
			invalid = true
			continue
		}
		hasError := len(det.Error) > 0 && string(det.Error) != "null"
		switch {
		case (det.Status == 200 || det.Status == 201) && !hasError:
			result.Result.Confirmed++
			result.Result.Unknown--
			result.Result.Items[i] = ItemOutcome{i, "confirmed", det.Status}
		case det.Status >= 400 && det.Status <= 599:
			result.Result.Failed++
			result.Result.Unknown--
			result.Result.Items[i] = ItemOutcome{i, "failed", det.Status}
			var detail struct {
				Type string `json:"type"`
			}
			if len(result.ErrorTypes) < 5 && json.Unmarshal(det.Error, &detail) == nil {
				typeName := safeBulkErrorType(detail.Type)
				found := false
				for _, v := range result.ErrorTypes {
					if v == typeName {
						found = true
					}
				}
				if !found {
					result.ErrorTypes = append(result.ErrorTypes, typeName)
				}
			}
			if !hasError {
				invalid = true
			}
		default:
			invalid = true
		}
	}
	if response.Errors != nil && *response.Errors != (result.Result.Failed > 0) {
		invalid = true
	}
	if invalid || result.Result.Failed > 0 || result.Result.Unknown > 0 {
		return result
	}
	return nil
}

func safeBulkErrorType(s string) string {
	switch s {
	case "mapper_parsing_exception", "version_conflict_engine_exception", "es_rejected_execution_exception", "security_exception", "illegal_argument_exception", "document_missing_exception", "cluster_block_exception", "knn_circuit_breaker_exception":
		return s
	}
	return "remote_error"
}
