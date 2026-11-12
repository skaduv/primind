// Package nebula implements the NebulaGraph knowledge-graph repository.
package nebula

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

type (
	// record is one statement fragment. Value is spliced into the statement
	// (VID literals and fixed syntax only); Vals, when present, become
	// parameters rendered as ":($pN_0,$pN_1,...)" after Value.
	record struct {
		ID, Value string
		Vals      []string
	}
	batch struct {
		Kind, Statement string
		Params          map[string]string
		IDs             []string
	}
)

// size is what a batch costs on the wire and against byte budgets:
// statement text plus parameter names and values.
func (b batch) size() int {
	n := len(b.Statement)
	for k, v := range b.Params {
		n += len(k) + len(v)
	}
	return n
}

// digest covers the statement and its parameters in a stable order.
func (b batch) digest() string {
	h := sha256.New()
	h.Write([]byte(b.Statement))
	keys := make([]string, 0, len(b.Params))
	for k := range b.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(b.Params[k]))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (r record) cost() int {
	n := len(r.Value)
	for _, v := range r.Vals {
		n += len(v) + 16 // placeholder text + parameter name
	}
	return n
}

func (r record) render(row int, params map[string]string) string {
	if len(r.Vals) == 0 {
		return r.Value
	}
	ph := make([]string, len(r.Vals))
	for i, v := range r.Vals {
		name := fmt.Sprintf("p%d_%d", row, i)
		params[name] = v
		ph[i] = "$" + name
	}
	return r.Value + ":(" + strings.Join(ph, ",") + ")"
}

// BatchResult reports the outcome of one write statement without its text.
type BatchResult struct {
	StatementDigest string   `json:"statement_digest"`
	Index           int      `json:"index"`
	Kind            string   `json:"kind"`
	IdentityDigests []string `json:"identity_digests"`
	State           string   `json:"state"`
}

// OperationError is returned when a multi-batch operation stops early; it lists every batch's state.
type OperationError struct {
	Operation string
	Batches   []BatchResult
	Cause     error
}

func (e *OperationError) Error() string {
	return fmt.Sprintf("nebula %s incomplete: %v", e.Operation, e.Cause)
}
func (e *OperationError) Unwrap() error { return e.Cause }
func splitBatches(cfg Config, kind, prefix, suffix string, records []record) ([]batch, error) {
	var result []batch
	var parts, ids []string
	var params map[string]string
	size := len(prefix) + len(suffix)
	flush := func() {
		if len(parts) > 0 {
			result = append(result, batch{
				Kind: kind, Statement: prefix + strings.Join(parts, ",") + suffix, Params: params, IDs: ids,
			})
			parts, ids, params = nil, nil, nil
			size = len(prefix) + len(suffix)
		}
	}
	for _, r := range records {
		cost := r.cost()
		if len(prefix)+cost+len(suffix) > cfg.StatementBytes {
			return nil, fmt.Errorf("%w: oversized %s record %x", ErrBudget, kind, sha256.Sum256([]byte(r.ID)))
		}
		sep := 0
		if len(parts) > 0 {
			sep = 1
		}
		if len(parts) >= cfg.BatchRecords || size+sep+cost > cfg.StatementBytes {
			flush()
			sep = 0
		}
		if params == nil && len(r.Vals) > 0 {
			params = map[string]string{}
		}
		parts = append(parts, r.render(len(parts), params))
		ids = append(ids, r.ID)
		size += sep + cost
	}
	flush()
	return result, nil
}

func planBytes(cfg Config, plan []batch) error {
	n := 0
	for _, b := range plan {
		n += b.size()
		if n > cfg.OperationBytes {
			return fmt.Errorf("%w: encoded operation", ErrBudget)
		}
	}
	return nil
}

func receipts(plan []batch) []BatchResult {
	out := make([]BatchResult, len(plan))
	for i, b := range plan {
		out[i] = BatchResult{Index: i, Kind: b.Kind, State: "not_sent"}
		out[i].StatementDigest = b.digest()
		for _, id := range b.IDs {
			out[i].IdentityDigests = append(out[i].IdentityDigests, fmt.Sprintf("%x", sha256.Sum256([]byte(id))))
		}
	}
	return out
}

func (r *Repository) runPlan(ctx context.Context, op string, plan []batch) error {
	if err := r.check(ctx); err != nil {
		return err
	}
	if err := planBytes(r.c.cfg, plan); err != nil {
		return err
	}
	states := receipts(plan)
	for i, b := range plan {
		if err := r.check(ctx); err != nil {
			return &OperationError{op, states, err}
		}
		out := r.c.execute(ctx, b, true)
		if out.Sent {
			states[i].State = "unknown"
		}
		if out.Err != nil {
			return &OperationError{op, states, out.Err}
		}
		states[i].State = "confirmed"
	}
	return nil
}
