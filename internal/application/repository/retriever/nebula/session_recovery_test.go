package nebula

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
	"github.com/vesoft-inc/fbthrift/thrift/lib/go/thrift"
	nv "github.com/vesoft-inc/nebula-go/v3/nebula"
	gr "github.com/vesoft-inc/nebula-go/v3/nebula/graph"
)

type recoveryConn struct {
	net.Conn
	once         sync.Once
	live, closed *atomic.Int32
}

func (c *recoveryConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.live.Add(-1); c.closed.Add(1) })
	return err
}

type recoveryHandler struct {
	*pipeGraphHandler
	authentications atomic.Int32
	wait            <-chan struct{}
	entered         chan struct{}
	stop            chan struct{}
}

func (h *recoveryHandler) Authenticate(ctx context.Context, u, p []byte) (*gr.AuthResponse, error) {
	h.authentications.Add(1)
	return h.pipeGraphHandler.Authenticate(ctx, u, p)
}

func (h *recoveryHandler) ExecuteWithParameter(ctx context.Context, id int64, stmt []byte,
	params map[string]*nv.Value,
) (*gr.ExecutionResponse, error) {
	if h.wait != nil && string(stmt) != "USE local_protocol_only;" {
		select {
		case h.entered <- struct{}{}:
		default:
		}
		select {
		case <-h.wait:
		case <-h.stop:
		}
	}
	return h.pipeGraphHandler.ExecuteWithParameter(ctx, id, stmt, params)
}

type recoveryPeer struct {
	local, remote net.Conn
	handler       *recoveryHandler
	done          chan struct{}
}

// Actual generated SDK frames on net.Pipe, with close accounting at net.Conn.
// This factory replaces only dialing; it never listens or contacts a database.
type recoveryWire struct {
	mu                 sync.Mutex
	peers              []*recoveryPeer
	live, peak, closed atomic.Int32
}

func recoveryDriver(t *testing.T, cfg Config, setup func(int, *recoveryHandler)) (*sdkDriver, *recoveryWire) {
	t.Helper()
	w := &recoveryWire{}
	d := &sdkDriver{cfg: cfg, sessions: make(chan sdkSession, cfg.Connections)}
	for i := 0; i < cfg.Connections; i++ {
		d.sessions <- nil
	}
	d.create = func(ctx context.Context) (sdkSession, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		local, remote := net.Pipe()
		tracked := &recoveryConn{Conn: local, live: &w.live, closed: &w.closed}
		n := w.live.Add(1)
		for old := w.peak.Load(); n > old; old = w.peak.Load() {
			if w.peak.CompareAndSwap(old, n) {
				break
			}
		}
		h := &recoveryHandler{
			pipeGraphHandler: &pipeGraphHandler{queries: make(chan string, 512)},
			entered:          make(chan struct{}, 1), stop: make(chan struct{}),
		}
		peer := &recoveryPeer{local: tracked, remote: remote, handler: h, done: make(chan struct{})}
		w.mu.Lock()
		setup(len(w.peers), h)
		w.peers = append(w.peers, peer)
		w.mu.Unlock()
		socket, err := thrift.NewSocket(thrift.SocketConn(remote), thrift.SocketTimeout(time.Second))
		if err != nil {
			_ = tracked.Close()
			_ = remote.Close()
			close(peer.done)
			return nil, err
		}
		transport := thrift.NewHeaderTransport(thrift.NewBufferedTransportFactory(128 << 10).GetTransport(socket))
		protocol := thrift.NewHeaderProtocolFactory().GetProtocol(transport)
		processor := gr.NewGraphServiceProcessor(h)
		go func() {
			defer close(peer.done)
			for {
				keep, err := thrift.ProcessContext(context.Background(), processor, protocol, protocol)
				if err != nil || !keep {
					return
				}
			}
		}()
		return newWireSessionOnConn(ctx, cfg, tracked)
	}
	t.Cleanup(func() {
		for _, p := range w.snapshot() {
			close(p.handler.stop)
			_ = p.local.Close()
			_ = p.remote.Close()
			waitSignal(t, p.done)
		}
	})
	return d, w
}

func (w *recoveryWire) snapshot() []*recoveryPeer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*recoveryPeer(nil), w.peers...)
}

func requireUnknownFirstBatch(t *testing.T, err error) {
	t.Helper()
	var op *OperationError
	if !errors.As(err, &op) || len(op.Batches) < 2 || op.Batches[0].State != "unknown" {
		t.Fatalf("sent failure lost its partial receipt: %v", err)
	}
	for _, b := range op.Batches[1:] {
		if b.State != "not_sent" {
			t.Fatalf("failed call continued dispatch: %+v", b)
		}
	}
}

func TestSessionRecoveryThriftDiscardsWithoutReplayWithinPoolBudget(t *testing.T) {
	for _, code := range []nv.ErrorCode{nv.ErrorCode_E_SESSION_INVALID, nv.ErrorCode_E_SESSION_TIMEOUT} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			cfg := testConfig()
			cfg.Connections, cfg.Writes = 2, 1
			d, w := recoveryDriver(t, cfg, func(i int, h *recoveryHandler) {
				if i < 2 {
					h.queryCode = code
				}
			})
			r := testRepo(cfg, d)
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			for i := 0; i < 2; i++ {
				err := r.AddGraph(context.Background(), testNS(), []*types.GraphData{sampleGraph()})
				requireUnknownFirstBatch(t, err)
				var execution *executionError
				if !errors.As(err, &execution) || execution.code != int(code) {
					t.Fatalf("lost server status: %v", err)
				}
			}
			peers := w.snapshot()
			if len(peers) != 2 || w.live.Load() != 0 || w.closed.Load() != 2 {
				t.Fatal("invalid sessions retained or automatically replaced")
			}
			for _, p := range peers {
				waitSignal(t, p.done)
				if len(p.handler.queries) != 2 || p.handler.authentications.Load() != 1 ||
					p.handler.signouts.Load() != 0 {
					t.Fatal("failed call replayed or invalid connection not discarded")
				}
			}
			// Multiple new callers recover through the same client/write budget.
			results := make(chan error, 8)
			for i := 0; i < cap(results); i++ {
				go func() { results <- r.AddGraph(context.Background(), testNS(), []*types.GraphData{sampleGraph()}) }()
			}
			for i := 0; i < cap(results); i++ {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			peers = w.snapshot()
			if len(peers) != 4 || w.live.Load() != 2 || w.peak.Load() > int32(cfg.Connections) || d.RetryCount() != 0 {
				t.Fatal("replacement escaped fixed slot/retry budget")
			}
			for _, p := range peers[2:] {
				if p.handler.authentications.Load() != 1 {
					t.Fatal("replacement not authenticated exactly once")
				}
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			m := r.Metrics()
			if w.live.Load() != 0 || w.closed.Load() != 4 || m.Active != 0 || m.Waiting != 0 ||
				m.PlanReservedBytes != 0 || m.Peak > cfg.Connections || m.PeakWrites > cfg.Writes {
				t.Fatalf("unbounded or undrained recovery: %+v", m)
			}
			t.Logf("connections created=%d closed=%d peak=%d; inflight peak=%d writes=%d retries=%d; "+
				"failed calls each sent exactly one batch",
				len(peers), w.closed.Load(), w.peak.Load(), m.Peak, m.PeakWrites, d.RetryCount())
		})
	}
}

func TestSessionRecoveryThriftKeepsOrdinaryServerErrors(t *testing.T) {
	for _, code := range []nv.ErrorCode{
		nv.ErrorCode_E_SYNTAX_ERROR, nv.ErrorCode_E_BAD_PERMISSION,
		nv.ErrorCode_E_PARTIAL_SUCCEEDED,
	} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			cfg := testConfig()
			cfg.Connections, cfg.Writes = 2, 1
			d, w := recoveryDriver(t, cfg, func(_ int, h *recoveryHandler) { h.queryCode = code })
			r := testRepo(cfg, d)
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			for i := 0; i < 4; i++ {
				out := d.Execute(context.Background(), "synthetic")
				var e *executionError
				if !out.Sent || !errors.As(out.Err, &e) || e.code != int(code) {
					t.Fatalf("ordinary failure changed: %+v", out)
				}
			}
			peers := w.snapshot()
			if len(peers) != 2 || w.closed.Load() != 0 || w.live.Load() != 2 {
				t.Fatal("ordinary error triggered reauthentication")
			}
			for _, p := range peers {
				if len(p.handler.queries) != 3 || p.handler.authentications.Load() != 1 {
					t.Fatal("unexpected replay or authentication")
				}
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if w.live.Load() != 0 || w.closed.Load() != 2 {
				t.Fatal("ordinary session shutdown leaked connections")
			}
			for _, p := range peers {
				waitSignal(t, p.done)
				if p.handler.signouts.Load() != 1 {
					t.Fatal("ordinary session shutdown missed signout")
				}
			}
			t.Log("4 failed requests reused 2 authenticated sessions; " +
				"no error-triggered reconnect; final close signed out both")
		})
	}
}

func TestSessionRecoveryThriftCancellationAndClose(t *testing.T) {
	for _, mode := range []string{"cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig()
			cfg.Connections, cfg.Writes = 2, 1
			cfg.SocketTimeout, cfg.CloseTimeout = time.Second, 3*time.Second
			release := make(chan struct{})
			d, w := recoveryDriver(t, cfg, func(i int, h *recoveryHandler) {
				if i == 0 {
					h.queryCode = nv.ErrorCode_E_SESSION_INVALID
				}
				if i == 1 {
					h.queryCode = nv.ErrorCode_E_SESSION_TIMEOUT
					h.wait = release
				}
			})
			r := testRepo(cfg, d)
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			requireUnknownFirstBatch(t, r.AddGraph(context.Background(), testNS(), []*types.GraphData{sampleGraph()}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- r.AddGraph(ctx, testNS(), []*types.GraphData{sampleGraph()}) }()
			deadline := time.Now().Add(time.Second)
			for len(w.snapshot()) < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			peers := w.snapshot()
			if len(peers) != 2 {
				t.Fatal("replacement not started")
			}
			waitSignal(t, peers[1].handler.entered)
			closed := make(chan error, 1)
			if mode == "cancel" {
				cancel()
			} else {
				go func() { closed <- r.Close(context.Background()) }()
				for !r.closed.Load() && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if !r.closed.Load() {
					t.Fatal("Close did not stop admission")
				}
				requireErr(t, r.AddGraph(context.Background(), testNS(), []*types.GraphData{sampleGraph()}), ErrClosed)
				select {
				case <-closed:
					t.Fatal("Close returned while RPC still active")
				default:
				}
				close(release)
			}
			err := <-result
			requireUnknownFirstBatch(t, err)
			if mode == "cancel" {
				requireErr(t, err, context.Canceled)
				if w.live.Load() != 0 {
					t.Fatal("cancel did not close actual connection")
				}
				close(release)
				if err := r.AddGraph(context.Background(), testNS(), []*types.GraphData{sampleGraph()}); err != nil {
					t.Fatal(err)
				}
				if err := r.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
			}
			m := r.Metrics()
			if w.live.Load() != 0 || w.peak.Load() > int32(cfg.Connections) || m.Active != 0 || m.ActiveWrites != 0 ||
				m.Waiting != 0 || m.PlanReservedBytes != 0 {
				t.Fatalf("recovery resources not drained: %+v", m)
			}
			for _, p := range w.snapshot() {
				waitSignal(t, p.done)
			}
			t.Logf("mode=%s created=%d closed=%d peak=%d active=%d waiting=%d plan_bytes=%d", mode, len(w.snapshot()),
				w.closed.Load(), w.peak.Load(), m.Active, m.Waiting, m.PlanReservedBytes)
		})
	}
}
