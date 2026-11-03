package nebula

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
)

type record struct{ ID, Value string }
type batch struct {
	Kind, Statement string
	IDs             []string
}
type BatchResult struct {
	StatementDigest string   `json:"statement_digest"`
	Index           int      `json:"index"`
	Kind            string   `json:"kind"`
	IdentityDigests []string `json:"identity_digests"`
	State           string   `json:"state"`
}
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
	size := len(prefix) + len(suffix)
	flush := func() {
		if len(parts) > 0 {
			result = append(result, batch{Kind: kind, Statement: prefix + strings.Join(parts, ",") + suffix, IDs: ids})
			parts = nil
			ids = nil
			size = len(prefix) + len(suffix)
		}
	}
	for _, r := range records {
		if len(prefix)+len(r.Value)+len(suffix) > cfg.StatementBytes {
			return nil, fmt.Errorf("%w: oversized %s record %x", ErrBudget, kind, sha256.Sum256([]byte(r.ID)))
		}
		sep := 0
		if len(parts) > 0 {
			sep = 1
		}
		if len(parts) >= cfg.BatchRecords || size+sep+len(r.Value) > cfg.StatementBytes {
			flush()
			sep = 0
		}
		parts = append(parts, r.Value)
		ids = append(ids, r.ID)
		size += sep + len(r.Value)
	}
	flush()
	return result, nil
}
func planBytes(cfg Config, plan []batch) error {
	n := 0
	for _, b := range plan {
		n += len(b.Statement)
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
		out[i].StatementDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(b.Statement)))
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
