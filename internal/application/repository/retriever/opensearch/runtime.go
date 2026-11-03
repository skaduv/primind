package opensearch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/opensearch-project/opensearch-go/v4/opensearchtransport"
)

// RuntimeOptions bounds a client independently of index/embedding settings.
// Start with DefaultRuntimeOptions; zero/negative limits are invalid, not unlimited.
type RuntimeOptions struct {
	RequestTimeout      time.Duration
	MaxResponseBytes    int64
	MaxConnsPerHost     int
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
}

func DefaultRuntimeOptions() RuntimeOptions {
	return RuntimeOptions{30 * time.Second, 16 << 20, 32, 32, 32, 90 * time.Second}
}

func (o RuntimeOptions) validate() error {
	if o.RequestTimeout <= 0 || o.RequestTimeout > 5*time.Minute || o.MaxResponseBytes <= 0 || o.MaxResponseBytes > 64<<20 ||
		o.MaxConnsPerHost <= 0 || o.MaxConnsPerHost > 1024 || o.MaxIdleConns <= 0 || o.MaxIdleConns > 1024 ||
		o.MaxIdleConnsPerHost <= 0 || o.MaxIdleConnsPerHost > o.MaxConnsPerHost || o.MaxIdleConnsPerHost > o.MaxIdleConns ||
		o.IdleConnTimeout <= 0 || o.IdleConnTimeout > 10*time.Minute {
		return ErrConfigInvalid
	}
	return nil
}

type clientOptions struct {
	runtime RuntimeOptions
	budget  *RequestBudget
}
type ClientOption func(*clientOptions) error

func WithRuntimeOptions(o RuntimeOptions) ClientOption {
	return func(c *clientOptions) error {
		if err := o.validate(); err != nil {
			return err
		}
		c.runtime = o
		return nil
	}
}

// WithRequestBudget allows explicitly isolated runtimes to share a different
// budget. All clients in that runtime MUST receive the same budget; omitted
// options use the package's process-wide budget (16 total, 12 writes, 64 waiters).
func WithRequestBudget(b *RequestBudget) ClientOption {
	return func(c *clientOptions) error {
		if b == nil || b.total < 2 || b.writes < 1 || b.waiting < 1 {
			return ErrConfigInvalid
		}
		c.budget = b
		return nil
	}
}

// RequestBudget owns only permits/counters, never authenticated connections.
// One permit covers an SDK request including its sequential retries, backoff,
// and complete response-body read. Read requests have reserved capacity.
type RequestBudget struct {
	mu                                              sync.Mutex
	total, writes, waiting                          int
	inflight, writing, queued                       int
	changed                                         chan struct{}
	peak, writePeak, queuePeak                      int
	attempts, requestBytes, responseBytes, rejected int64
	waitNanos                                       int64
}

type BudgetSnapshot struct {
	InFlight, Writing, Queued, Peak, WritePeak, QueuePeak int
	Attempts, RequestBytes, ResponseBytes, Rejected       int64
	Wait                                                  time.Duration
}

func NewRequestBudget(total, writes, waiting int) (*RequestBudget, error) {
	if total < 2 || total > 1024 || writes < 1 || writes >= total || waiting < 1 || waiting > 4096 {
		return nil, ErrConfigInvalid
	}
	return &RequestBudget{total: total, writes: writes, waiting: waiting, changed: make(chan struct{})}, nil
}

var processRequestBudget = func() *RequestBudget { b, _ := NewRequestBudget(16, 12, 64); return b }()

func (b *RequestBudget) Snapshot() BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetSnapshot{b.inflight, b.writing, b.queued, b.peak, b.writePeak, b.queuePeak, b.attempts, b.requestBytes, b.responseBytes, b.rejected, time.Duration(b.waitNanos)}
}

func (b *RequestBudget) acquire(ctx context.Context, write bool) (func(), error) {
	started := time.Now()
	b.mu.Lock()
	waited := false
	defer func() {
		if waited {
			b.queued--
		}
		b.waitNanos += int64(time.Since(started))
		b.mu.Unlock()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if b.inflight < b.total && (!write || b.writing < b.writes) {
			b.inflight++
			if write {
				b.writing++
			}
			b.peak = max(b.peak, b.inflight)
			b.writePeak = max(b.writePeak, b.writing)
			return func() {
				b.mu.Lock()
				b.inflight--
				if write {
					b.writing--
				}
				close(b.changed)
				b.changed = make(chan struct{})
				b.mu.Unlock()
			}, nil
		}
		if !waited {
			if b.queued >= b.waiting {
				b.rejected++
				return nil, ErrRequestBudgetFull
			}
			b.queued++
			waited = true
			b.queuePeak = max(b.queuePeak, b.queued)
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		b.mu.Lock()
	}
}

// Conservative classification: only known read APIs may use reserved slots.
func isWriteRequest(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return false
	}
	for _, suffix := range []string{"/_search", "/_msearch", "/_count", "/_field_caps", "/_search/scroll"} {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, suffix) {
			return false
		}
	}
	return true
}

type requestTraceKey struct{}
type operationLeaseKey struct{}
type operationLease struct {
	budget *RequestBudget
	write  bool
}
type requestTrace struct{ attempts atomic.Int64 }

type runtimeTransport struct {
	base    opensearchtransport.Interface
	pool    *http.Transport
	options RuntimeOptions
	budget  *RequestBudget
}

func (t *runtimeTransport) Perform(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), t.options.RequestTimeout)
	defer cancel()
	if lease, ok := ctx.Value(operationLeaseKey{}).(operationLease); ok && lease.budget == t.budget && (lease.write || !isWriteRequest(req)) {
		return t.base.Perform(req.Clone(ctx))
	}
	release, err := t.budget.acquire(ctx, isWriteRequest(req))
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	defer release()
	// SDK v4.6.0 reads the complete wire response before returning. The inner
	// round tripper makes that read finite and propagates read errors the SDK
	// otherwise discards. Keep the permit and context alive until it finishes.
	return t.base.Perform(req.Clone(ctx))
}

// Batch admission happens before encoding. Initialization owners also acquire
// this lease BEFORE publishing an attempt that admitted batches might wait on.
// Nested initialization/SDK calls borrow it instead of acquiring another slot.
func admitWriteOperation(ctx context.Context, client *osapi.Client) (context.Context, func(), error) {
	if client != nil && client.Client != nil {
		if t, ok := client.Client.Transport.(*runtimeTransport); ok {
			if lease, ok := ctx.Value(operationLeaseKey{}).(operationLease); ok && lease.budget == t.budget && lease.write {
				return ctx, func() {}, nil // borrowing must not release the outer owner's permit
			}
			release, err := t.budget.acquire(ctx, true)
			if err != nil {
				return ctx, func() {}, err
			}
			return context.WithValue(ctx, operationLeaseKey{}, operationLease{t.budget, true}), release, nil
		}
	}
	return ctx, func() {}, nil
}

type boundedRoundTripper struct {
	base     http.RoundTripper
	budget   *RequestBudget
	maxBytes int64
}

func (t *boundedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	t.budget.mu.Lock()
	t.budget.attempts++
	t.budget.requestBytes += max(0, req.ContentLength)
	t.budget.mu.Unlock()
	if trace, ok := req.Context().Value(requestTraceKey{}).(*requestTrace); ok {
		trace.attempts.Add(1)
	}
	res, err := t.base.RoundTrip(req)
	if err != nil {
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		return nil, err
	}
	if res.Body == nil {
		return res, nil
	}
	// Do not merely LimitReader after SDK buffering, or drain an oversized
	// response. Closing here releases the socket on every exit, including EOF.
	body, readErr := io.ReadAll(io.LimitReader(res.Body, t.maxBytes+1))
	closeErr := res.Body.Close()
	t.budget.mu.Lock()
	t.budget.responseBytes += int64(len(body))
	t.budget.mu.Unlock()
	if readErr != nil {
		return nil, readErr
	}
	if int64(len(body)) > t.maxBytes {
		return nil, ErrResponseTooLarge
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}

// CloseIdleConnections is called by the owner of an exclusive client. It holds
// the real transport handle, not an optional method on the SSRF wrapper.
// It does not retire in-flight work; registry retirement/shutdown is OS-03.
func CloseIdleConnections(client *osapi.Client) {
	if client != nil && client.Client != nil {
		if t, ok := client.Client.Transport.(*runtimeTransport); ok {
			t.pool.CloseIdleConnections()
		}
	}
}

func operationContext(ctx context.Context, client *osapi.Client) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	d := DefaultRuntimeOptions().RequestTimeout
	if client != nil && client.Client != nil {
		if t, ok := client.Client.Transport.(*runtimeTransport); ok {
			d = t.options.RequestTimeout
		}
	}
	return context.WithTimeout(ctx, d)
}

// WaitForVisibility is a finite polling helper for later isolated acceptance.
// The caller supplies a read-only visibility check; it never forces a refresh.
func WaitForVisibility(ctx context.Context, timeout, interval time.Duration, check func(context.Context) (bool, error)) error {
	if ctx == nil || timeout <= 0 || timeout > 5*time.Minute || interval <= 0 || interval > timeout || check == nil {
		return ErrConfigInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := check(ctx)
		if err != nil {
			return err
		}
		if ok {
			return ctx.Err()
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func runtimeConfigError(err error) error {
	return fmt.Errorf("opensearch: invalid runtime options: %w", err)
}
