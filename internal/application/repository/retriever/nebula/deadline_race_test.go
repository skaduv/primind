package nebula

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
)

// forceIndexOnly makes r treat ns as a large namespace, so Search takes the
// prefix fallback without timing the in-store filter first. A tiny
// SearchScanTimeout alone does not guarantee that: on Windows the monotonic
// clock advances in steps of about 0.5 ms, a 1 ns timeout stays live until
// the next step, and a filter over a small namespace can finish before it.
func forceIndexOnly(r *Repository, ns types.NameSpace) {
	(&searchRun{r: r, ns: ns}).markLargeScope()
}

// overdueCtx is a context whose deadline is over while Err is still nil: the
// state between a socket deadline (taken from the context) failing an RPC and
// the context's own timer marking it done.
type overdueCtx struct{ context.Context }

func (overdueCtx) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }

// An RPC that fails after its context's deadline is a deadline error, whether
// or not the context's timer has fired yet.
func TestExecuteReportsDeadlineWhenSocketDeadlineWinsTheRace(t *testing.T) {
	cfg := testConfig()
	wire := goldenWire(t, cfg, loadGolden(t).Baseline, testNS())
	wire.fn = func(context.Context, string) wireOutcome {
		return wireOutcome{Sent: true, Err: &executionError{cause: os.ErrDeadlineExceeded}}
	}
	r := testRepo(cfg, wire)
	out := r.c.execute(overdueCtx{context.Background()}, batch{Kind: "probe", Statement: "YIELD 1;"}, false)
	requireErr(t, out.Err, context.DeadlineExceeded)
	if !out.Sent {
		t.Fatal("a statement that reached the store must stay marked as sent")
	}
	// Without a deadline, or before it, the error is reported as it is.
	out = r.c.execute(context.Background(), batch{Kind: "probe", Statement: "YIELD 1;"}, false)
	var sdkErr *executionError
	if !errors.As(out.Err, &sdkErr) || errors.Is(out.Err, context.DeadlineExceeded) {
		t.Fatalf("error without a deadline: %v", out.Err)
	}
}

// The in-store filter gets a socket deadline equal to its scan budget. When
// that deadline fails the RPC a moment before the scan context is marked
// done, search must still fall back to prefix matching instead of failing.
func TestSearchFallsBackWhenSocketDeadlineBeatsScanContext(t *testing.T) {
	cfg := testConfig()
	cfg.SearchScanTimeout = time.Millisecond
	ns := testNS()
	baseline := loadGolden(t).Baseline
	for i := 0; i < 300; i++ {
		wire := goldenWire(t, cfg, baseline, ns)
		read := wire.fn
		wire.fn = func(ctx context.Context, s string) wireOutcome {
			if strings.Contains(s, " CONTAINS ") {
				// Return the socket's timeout error at the deadline itself,
				// racing the context's timer.
				d, _ := ctx.Deadline()
				for time.Now().Before(d) {
					runtime.Gosched()
				}
				return wireOutcome{Sent: true, Err: &executionError{cause: os.ErrDeadlineExceeded}}
			}
			return read(ctx, s)
		}
		g, d, err := testRepo(cfg, wire).Search(context.Background(), ns, []string{"主泵A"})
		if err != nil || !d.IndexOnly || len(g.Relation) == 0 {
			t.Fatalf("iteration %d: scan timeout not turned into a prefix fallback: %v %+v", i, err, d)
		}
	}
}
