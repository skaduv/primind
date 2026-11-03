package nebula

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrClosed   = errors.New("nebula client is closed")
	ErrCapacity = errors.New("nebula admission capacity exceeded")
	ErrBudget   = errors.New("nebula bounded operation budget exceeded")
	ErrInvalid  = errors.New("invalid nebula input")
)
var schemaName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// Config has no implicit environment loading. All limits are finite, including
// read snapshots. DefaultConfig supplies engineering starting values, not SLAs.
type Config struct {
	Addresses        []string
	Space            string
	Username         string `json:"-"`
	Password         string `json:"-"`
	TLS              bool
	ServerName       string
	CACertificatePEM string `json:"-"`
	Connections      int
	Writes           int
	Waiters          int
	PendingBytes     int
	PlanBytes        int
	BatchRecords     int
	StatementBytes   int
	OperationBytes   int
	SnapshotRecords  int
	ReadBytes        int
	VIDBytes         int
	AcquireTimeout   time.Duration
	SocketTimeout    time.Duration
	OperationTimeout time.Duration
	CloseTimeout     time.Duration
	ReadyTimeout     time.Duration
	PollInterval     time.Duration
}

func DefaultConfig() Config {
	return Config{Connections: 4, Writes: 2, Waiters: 32, PendingBytes: 8 << 20, PlanBytes: 32 << 20, BatchRecords: 100, StatementBytes: 64 << 10, OperationBytes: 8 << 20, SnapshotRecords: 10000, ReadBytes: 16 << 20, VIDBytes: 1024, AcquireTimeout: 5 * time.Second, SocketTimeout: 5 * time.Second, OperationTimeout: 30 * time.Second, CloseTimeout: 30 * time.Second, ReadyTimeout: 20 * time.Second, PollInterval: 100 * time.Millisecond}
}
func (Config) String() string { return "NebulaGraph configuration (credentials redacted)" }
func (c Config) validate() error {
	if len(c.Addresses) == 0 || !schemaName.MatchString(c.Space) || c.Username == "" || c.Password == "" {
		return fmt.Errorf("%w: explicit endpoint/space/credentials required", ErrInvalid)
	}
	for _, a := range c.Addresses {
		host, port, err := net.SplitHostPort(a)
		if err != nil || host == "" || port == "" {
			return fmt.Errorf("%w: host:port required", ErrInvalid)
		}
	}
	if c.Connections < 2 || c.Connections > 64 || c.Writes < 1 || c.Writes >= c.Connections || c.Waiters < 1 || c.Waiters > 1024 {
		return fmt.Errorf("%w: connection/write/wait limits", ErrInvalid)
	}
	if c.BatchRecords < 1 || c.BatchRecords > 10000 || c.StatementBytes < 256 || c.StatementBytes > 4<<20 || c.OperationBytes < c.StatementBytes || c.OperationBytes > 64<<20 || c.PendingBytes < c.StatementBytes || c.PendingBytes > 64<<20 || c.SnapshotRecords < 1 || c.SnapshotRecords > 100000 || c.ReadBytes < 256 || c.ReadBytes > 128<<20 || c.VIDBytes < 128 || c.VIDBytes > 4096 {
		return fmt.Errorf("%w: byte/record/VID limits", ErrInvalid)
	}
	for _, d := range []time.Duration{c.AcquireTimeout, c.SocketTimeout, c.OperationTimeout, c.CloseTimeout, c.ReadyTimeout, c.PollInterval} {
		if d <= 0 || d > 5*time.Minute {
			return fmt.Errorf("%w: positive bounded timeout required", ErrInvalid)
		}
	}
	if c.PollInterval > c.ReadyTimeout {
		return fmt.Errorf("%w: poll exceeds ready timeout", ErrInvalid)
	}
	if c.CloseTimeout < 2*c.SocketTimeout {
		return fmt.Errorf("%w: close budget must cover active RPC and parallel signout", ErrInvalid)
	}
	if c.PlanBytes < c.OperationBytes || c.PlanBytes > 256<<20 {
		return fmt.Errorf("%w: planned statement memory budget", ErrInvalid)
	}
	return nil
}
func configKey(c Config) [32]byte {
	b, _ := json.Marshal(c)
	b = append(b, 0)
	b = append(b, c.Username...)
	b = append(b, 0)
	b = append(b, c.Password...)
	b = append(b, 0)
	b = append(b, c.CACertificatePEM...)
	return sha256.Sum256(b)
}

type wireTable struct {
	Columns  []string
	Rows     [][]string
	PlanJSON []byte
}
type wireOutcome struct {
	Table wireTable
	Sent  bool
	Err   error
}
type wireDriver interface {
	Execute(context.Context, string) wireOutcome
	Close()
	RetryCount() uint64
}
type driverFactory func(context.Context, Config) (wireDriver, error)

// Only statement metadata is retained; no query text, endpoint or credential.
type Metric struct {
	ResultRows      int    `json:"result_rows"`
	StatementDigest string `json:"statement_digest"`
	ErrorCode       int    `json:"error_code"`
	Kind            string `json:"kind"`
	Records         int    `json:"records"`
	Bytes           int    `json:"bytes"`
	WaitNS          int64  `json:"wait_ns"`
	RPCNS           int64  `json:"rpc_ns"`
	Failed          bool   `json:"failed"`
}
type Metrics struct {
	PlanReservedBytes int      `json:"plan_reserved_bytes"`
	PlanWaiters       int      `json:"plan_waiters"`
	AddCalls          uint64   `json:"add_calls"`
	AddWallNS         int64    `json:"add_wall_ns"`
	SearchTruncations uint64   `json:"search_truncations"`
	Active            int      `json:"active"`
	ActiveWrites      int      `json:"active_writes"`
	Peak              int      `json:"peak"`
	PeakWrites        int      `json:"peak_writes"`
	Waiting           int      `json:"waiting"`
	StatementCalls    uint64   `json:"statement_calls"`
	Errors            uint64   `json:"errors"`
	SDKReconnects     uint64   `json:"sdk_reconnects"`
	DroppedSamples    uint64   `json:"dropped_samples"`
	Samples           []Metric `json:"samples"`
}
type client struct {
	planBytes, planWaiters           int
	cfg                              Config
	driver                           wireDriver
	mu                               sync.Mutex
	changed                          chan struct{}
	done                             chan struct{}
	closing                          bool
	finalizing                       bool
	active, writes, waiting, pending int
	metrics                          Metrics
}

func newClient(c Config, d wireDriver) *client {
	return &client{cfg: c, driver: d, changed: make(chan struct{}), done: make(chan struct{})}
}
func (c *client) signalLocked() { close(c.changed); c.changed = make(chan struct{}) }
func (c *client) admit(ctx context.Context, write bool, size int) (func(), time.Duration, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, c.cfg.AcquireTimeout)
	defer cancel()
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil, 0, ErrClosed
	}
	if c.waiting+c.planWaiters >= c.cfg.Waiters || size > c.cfg.PendingBytes-c.pending {
		c.mu.Unlock()
		return nil, 0, ErrCapacity
	}
	c.waiting++
	c.pending += size
	for {
		if err := ctx.Err(); err != nil {
			c.waiting--
			c.pending -= size
			c.mu.Unlock()
			return nil, time.Since(start), err
		}
		if c.closing {
			c.waiting--
			c.pending -= size
			c.mu.Unlock()
			return nil, time.Since(start), ErrClosed
		}
		if c.active < c.cfg.Connections && (!write || c.writes < c.cfg.Writes) {
			c.waiting--
			c.pending -= size
			c.active++
			if write {
				c.writes++
			}
			c.metrics.Peak = max(c.metrics.Peak, c.active)
			c.metrics.PeakWrites = max(c.metrics.PeakWrites, c.writes)
			c.mu.Unlock()
			return func() { c.release(write) }, time.Since(start), nil
		}
		wake := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-wake:
		}
		c.mu.Lock()
	}
}
func (c *client) release(write bool) {
	c.mu.Lock()
	c.active--
	if write {
		c.writes--
	}
	c.signalLocked()
	finish := c.closing && c.active == 0 && c.planBytes == 0 && !c.finalizing
	if finish {
		c.finalizing = true
	}
	c.mu.Unlock()
	if finish {
		go func() { c.driver.Close(); close(c.done) }()
	}
}
func (c *client) close(ctx context.Context) error {
	c.mu.Lock()
	c.closing = true
	c.signalLocked()
	finish := c.active == 0 && c.planBytes == 0 && !c.finalizing
	if finish {
		c.finalizing = true
	}
	c.mu.Unlock()
	if finish {
		go func() { c.driver.Close(); close(c.done) }()
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *client) execute(ctx context.Context, b batch, write bool) wireOutcome {
	release, wait, err := c.admit(ctx, write, len(b.Statement))
	if err != nil {
		return wireOutcome{Err: err}
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return wireOutcome{Err: err}
	}
	start := time.Now()
	out := c.driver.Execute(ctx, b.Statement)
	if ctx.Err() != nil {
		out.Err = ctx.Err()
	}
	m := Metric{Kind: b.Kind, Records: len(b.IDs), Bytes: len(b.Statement), WaitNS: int64(wait), RPCNS: int64(time.Since(start)), Failed: out.Err != nil}
	m.StatementDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(b.Statement)))
	m.ResultRows = len(out.Table.Rows)
	var sdkErr *executionError
	if errors.As(out.Err, &sdkErr) {
		m.ErrorCode = sdkErr.code
	}
	c.mu.Lock()
	c.metrics.StatementCalls++
	if m.Failed {
		c.metrics.Errors++
	}
	if len(c.metrics.Samples) < 10000 {
		c.metrics.Samples = append(c.metrics.Samples, m)
	} else {
		c.metrics.DroppedSamples++
	}
	c.mu.Unlock()
	return out
}
func (c *client) snapshotMetrics() Metrics {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.metrics
	m.Active = c.active
	m.ActiveWrites = c.writes
	m.Waiting = c.waiting
	m.PlanReservedBytes = c.planBytes
	m.PlanWaiters = c.planWaiters
	m.SDKReconnects = c.driver.RetryCount()
	m.Samples = append([]Metric(nil), m.Samples...)
	return m
}

type poolEntry struct {
	c       *client
	refs    int
	closing bool
	ready   chan struct{}
}
type registry struct {
	mu      sync.Mutex
	entries map[[32]byte]*poolEntry
}

var pools = registry{entries: make(map[[32]byte]*poolEntry)}

// NewRepository shares the same bounded client for identical configurations in
// this process. A handle owns one reference; the last Close drains the pool.
func NewRepository(ctx context.Context, cfg Config) (*Repository, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.OperationTimeout)
	defer cancel()
	r, err := pools.open(ctx, cfg, newSDKDriver)
	if err != nil {
		return nil, err
	}
	if err := r.CheckSchema(ctx); err != nil {
		_ = r.Close(context.Background())
		return nil, err
	}
	return r, nil
}
func (r *registry) open(ctx context.Context, cfg Config, factory driverFactory) (*Repository, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	key := configKey(cfg)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.mu.Lock()
		if entry := r.entries[key]; entry != nil {
			if entry.ready != nil {
				ready := entry.ready
				r.mu.Unlock()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-ready:
					continue
				}
			}
			if entry.closing {
				select {
				case <-entry.c.done:
					delete(r.entries, key)
				default:
					r.mu.Unlock()
					return nil, ErrClosed
				}
			} else {
				entry.refs++
				r.mu.Unlock()
				return &Repository{c: entry.c, registry: r, key: key}, nil
			}
		}
		ready := make(chan struct{})
		entry := &poolEntry{ready: ready}
		r.entries[key] = entry
		r.mu.Unlock()
		// Only this caller initializes this configuration. Other callers can cancel
		// their bounded wait; no registry mutex is held over SDK network operations.
		d, err := factory(ctx, cfg)
		if err == nil {
			err = ctx.Err()
		}
		if d == nil && err == nil {
			err = ErrInvalid
		}
		if err != nil && d != nil {
			d.Close()
		}
		r.mu.Lock()
		if err != nil {
			delete(r.entries, key)
		} else {
			entry.c = newClient(cfg, d)
			entry.refs = 1
			entry.ready = nil
		}
		close(ready)
		r.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &Repository{c: entry.c, registry: r, key: key}, nil
	}
}

type Repository struct {
	c         *client
	registry  *registry
	key       [32]byte
	closed    atomic.Bool
	closeOnce sync.Once
	lastClose bool
}

func (r *Repository) check(ctx context.Context) error {
	if r == nil || r.c == nil || r.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
func (r *Repository) Close(ctx context.Context) error {
	if r == nil || r.c == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		if r.registry == nil {
			r.lastClose = true
			return
		}
		r.registry.mu.Lock()
		defer r.registry.mu.Unlock()
		e := r.registry.entries[r.key]
		e.refs--
		if e.refs == 0 {
			e.closing = true
			r.lastClose = true
		}
	})
	if !r.lastClose {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, r.c.cfg.CloseTimeout)
	defer cancel()
	return r.c.close(bounded)
}
func (r *Repository) Metrics() Metrics { return r.c.snapshotMetrics() }

// TakeSamples drains bounded diagnostic samples. Counters remain cumulative.
// A single authorized measurement consumer owns sampling during a run.
func (r *Repository) TakeSamples() []Metric {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	out := r.c.metrics.Samples
	r.c.metrics.Samples = nil
	return out
}

func (c *client) reservePlan(ctx context.Context) (func(), error) {
	bounded, cancel := context.WithTimeout(ctx, c.cfg.AcquireTimeout)
	defer cancel()
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if c.waiting+c.planWaiters >= c.cfg.Waiters {
		c.mu.Unlock()
		return nil, ErrCapacity
	}
	c.planWaiters++
	for {
		if err := bounded.Err(); err != nil {
			c.planWaiters--
			c.mu.Unlock()
			return nil, err
		}
		if c.closing {
			c.planWaiters--
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if c.planBytes+c.cfg.OperationBytes <= c.cfg.PlanBytes {
			c.planWaiters--
			c.planBytes += c.cfg.OperationBytes
			c.mu.Unlock()
			return func() {
				c.mu.Lock()
				c.planBytes -= c.cfg.OperationBytes
				c.signalLocked()
				finish := c.closing && c.active == 0 && c.planBytes == 0 && !c.finalizing
				if finish {
					c.finalizing = true
				}
				c.mu.Unlock()
				if finish {
					go func() { c.driver.Close(); close(c.done) }()
				}
			}, nil
		}
		wake := c.changed
		c.mu.Unlock()
		select {
		case <-bounded.Done():
		case <-wake:
		}
		c.mu.Lock()
	}
}
