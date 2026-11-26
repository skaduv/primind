package types

import (
	"context"
	"sync/atomic"
)

// RetrievalStats collects transport figures of one search request across its
// store groups, so the request can log a single summary. Drivers that know
// their response sizes add to it; others leave it untouched. Safe for
// concurrent use by the fan-out goroutines.
type RetrievalStats struct {
	// Searches counts search requests sent to a store, retries included.
	Searches atomic.Int64
	// ResponseBytes sums the response bodies of those requests.
	ResponseBytes atomic.Int64
	// TruncatedSets counts result sets cut short to fit a response cap.
	TruncatedSets atomic.Int64
}

type retrievalStatsKey struct{}

// WithRetrievalStats returns a context carrying a fresh RetrievalStats.
func WithRetrievalStats(ctx context.Context) (context.Context, *RetrievalStats) {
	stats := &RetrievalStats{}
	return context.WithValue(ctx, retrievalStatsKey{}, stats), stats
}

// RetrievalStatsFromContext returns the request's RetrievalStats, or nil when
// the caller did not ask for them.
func RetrievalStatsFromContext(ctx context.Context) *RetrievalStats {
	if ctx == nil {
		return nil
	}
	stats, _ := ctx.Value(retrievalStatsKey{}).(*RetrievalStats)
	return stats
}
