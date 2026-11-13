package nebula

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vesoft-inc/fbthrift/thrift/lib/go/thrift"
	ng "github.com/vesoft-inc/nebula-go/v3"
	nv "github.com/vesoft-inc/nebula-go/v3/nebula"
	gr "github.com/vesoft-inc/nebula-go/v3/nebula/graph"
)

type executionError struct {
	code  int
	cause error
}

func (e *executionError) Error() string {
	return fmt.Sprintf("nebula execution failed (code=%d; result may be unknown)", e.code)
}
func (e *executionError) Unwrap() error { return e.cause }

// The public v3.8.0 generated client is used directly. Session.executeWithReconnect
// removes an old connection without closing it; retaining socket ownership here
// avoids relying on that path for a physical connection bound. There is no retry.
type sdkSession interface {
	Execute(ctx context.Context, stmt string, params map[string]string) (*ng.ResultSet, error)
	Release()
}
type sdkDriver struct {
	cfg      Config
	sessions chan sdkSession
	next     atomic.Uint64
	create   func(context.Context) (sdkSession, error)
}

func newSDKDriver(ctx context.Context, cfg Config) (wireDriver, error) {
	d := &sdkDriver{cfg: cfg, sessions: make(chan sdkSession, cfg.Connections)}
	d.create = func(ctx context.Context) (sdkSession, error) {
		index := int(d.next.Add(1)-1) % len(cfg.Addresses)
		return openWireSession(ctx, cfg, cfg.Addresses[index])
	}
	for i := 0; i < cfg.Connections; i++ {
		d.sessions <- nil
	}
	s, err := d.create(ctx)
	if err != nil {
		d.Close()
		return nil, err
	}
	<-d.sessions
	d.sessions <- s
	return d, nil
}

func resultCode(rs *ng.ResultSet) int {
	if rs == nil {
		return -1
	}
	return int(rs.GetErrorCode())
}

func (d *sdkDriver) Execute(ctx context.Context, stmt string, params map[string]string) wireOutcome {
	var s sdkSession
	select {
	case <-ctx.Done():
		return wireOutcome{Err: ctx.Err()}
	case s = <-d.sessions:
	}
	if s == nil {
		var err error
		s, err = d.create(ctx)
		if err != nil {
			d.sessions <- nil
			return wireOutcome{Err: err}
		}
	}
	broken := false
	defer func() {
		if broken {
			if x, ok := s.(interface{ Discard() }); ok {
				x.Discard()
			} else {
				s.Release()
			}
			d.sessions <- nil
		} else {
			d.sessions <- s
		}
	}()
	if err := ctx.Err(); err != nil {
		return wireOutcome{Err: err}
	}
	rs, err := s.Execute(ctx, stmt, params)
	if err != nil {
		broken = true
		return wireOutcome{Sent: true, Err: &executionError{cause: err}}
	}
	if rs == nil || !rs.IsSucceed() {
		code := resultCode(rs)
		// These sessions cannot serve the next caller. Close the connection
		// before returning its empty slot, but never replay this failed request.
		broken = code == int(nv.ErrorCode_E_SESSION_INVALID) || code == int(nv.ErrorCode_E_SESSION_TIMEOUT)
		return wireOutcome{Sent: true, Err: &executionError{code: code}}
	}
	table, err := decodeSDKTable(rs, d.cfg)
	return wireOutcome{Table: table, Sent: true, Err: err}
}

func (d *sdkDriver) Close() {
	var wg sync.WaitGroup
	for i := 0; i < cap(d.sessions); i++ {
		s := <-d.sessions
		if s != nil {
			wg.Add(1)
			go func() { defer wg.Done(); s.Release() }()
		}
	}
	wg.Wait()
}
func (*sdkDriver) RetryCount() uint64 { return 0 }

// thrift.Socket refreshes deadlines per Read/Write. Clamp those refreshes to a
// fixed RPC deadline, so a slowly streamed response cannot renew the time budget.
type deadlineConn struct {
	net.Conn
	limit time.Time
}

func (c *deadlineConn) capDeadline(t time.Time) time.Time {
	if t.IsZero() || t.After(c.limit) {
		return c.limit
	}
	return t
}
func (c *deadlineConn) SetDeadline(t time.Time) error { return c.Conn.SetDeadline(c.capDeadline(t)) }
func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(c.capDeadline(t))
}

func (c *deadlineConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(c.capDeadline(t))
}

func (c *deadlineConn) begin(ctx context.Context, timeout time.Duration) error {
	c.limit = time.Now().Add(timeout)
	if t, ok := ctx.Deadline(); ok && t.Before(c.limit) {
		c.limit = t
	}
	return c.Conn.SetDeadline(c.limit)
}

type graphRPC interface {
	VerifyClientVersion(*gr.VerifyClientVersionReq) (*gr.VerifyClientVersionResp, error)
	Authenticate([]byte, []byte) (*gr.AuthResponse, error)
	ExecuteWithParameter(int64, []byte, map[string]*nv.Value) (*gr.ExecutionResponse, error)
	Signout(int64) error
}
type wireSession struct {
	conn          *deadlineConn
	transport     thrift.Transport
	rpc           graphRPC
	id            int64
	authenticated bool
	timeout       time.Duration
	closeOnce     sync.Once
}

// rpcCall actually closes the underlying net.Conn on cancellation. It still
// waits for the generated SDK call to return before releasing its admission slot.
func (s *wireSession) rpcCall(ctx context.Context, call func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.conn.begin(ctx, s.timeout); err != nil {
		return err
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = s.conn.Close(); close(stopped) })
	err := call()
	if !stop() {
		<-stopped
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func openWireSession(ctx context.Context, cfg Config, address string) (sdkSession, error) {
	host, port, err := net.SplitHostPort(address)
	number, e := strconv.Atoi(port)
	if err != nil || e != nil || host == "" || number < 1 || number > 65535 {
		return nil, ErrInvalid
	}
	dialer := &net.Dialer{Timeout: cfg.SocketTimeout}
	var conn net.Conn
	if cfg.TLS {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.ServerName}
		if cfg.CACertificatePEM != "" {
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM([]byte(cfg.CACertificatePEM)) {
				return nil, ErrInvalid
			}
			tlsConfig.RootCAs = roots
		}
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, &executionError{cause: err}
	}
	return newWireSessionOnConn(ctx, cfg, conn)
}

func newWireSessionOnConn(ctx context.Context, cfg Config, conn net.Conn) (sdkSession, error) {
	bounded := &deadlineConn{Conn: conn}
	socket, err := thrift.NewSocket(thrift.SocketConn(bounded), thrift.SocketTimeout(cfg.SocketTimeout))
	if err != nil {
		_ = conn.Close()
		return nil, &executionError{cause: err}
	}
	transport := thrift.NewHeaderTransport(thrift.NewBufferedTransportFactory(128 << 10).GetTransport(socket))
	rpc := gr.NewGraphServiceClientFactory(transport, thrift.NewHeaderProtocolFactory())
	s := &wireSession{conn: bounded, transport: transport, rpc: rpc, timeout: cfg.SocketTimeout}
	fail := func(err error) (sdkSession, error) { s.Discard(); return nil, err }
	var version *gr.VerifyClientVersionResp
	err = s.rpcCall(ctx, func() error {
		var e error
		version, e = rpc.VerifyClientVersion(gr.NewVerifyClientVersionReq())
		return e
	})
	if err != nil {
		return fail(&executionError{cause: err})
	}
	if version == nil || version.GetErrorCode() != nv.ErrorCode_SUCCEEDED {
		return fail(&executionError{code: -1})
	}
	var auth *gr.AuthResponse
	err = s.rpcCall(ctx, func() error {
		var e error
		auth, e = rpc.Authenticate([]byte(cfg.Username), []byte(cfg.Password))
		return e
	})
	if err != nil {
		return fail(&executionError{cause: err})
	}
	if auth == nil || auth.GetErrorCode() != nv.ErrorCode_SUCCEEDED {
		code := -1
		if auth != nil {
			code = int(auth.GetErrorCode())
		}
		return fail(&executionError{code: code})
	}
	s.id = auth.GetSessionID()
	s.authenticated = true
	rs, err := s.Execute(ctx, "USE "+cfg.Space+";", nil)
	if err != nil {
		return fail(&executionError{cause: err})
	}
	if !rs.IsSucceed() {
		s.Release()
		return nil, &executionError{code: resultCode(rs)}
	}
	return s, nil
}

func (s *wireSession) Execute(ctx context.Context, stmt string, params map[string]string) (*ng.ResultSet, error) {
	values := make(map[string]*nv.Value, len(params))
	for k, v := range params {
		values[k] = &nv.Value{SVal: []byte(v)}
	}
	var response *gr.ExecutionResponse
	err := s.rpcCall(ctx, func() error {
		var e error
		response, e = s.rpc.ExecuteWithParameter(s.id, []byte(stmt), values)
		return e
	})
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, ErrInvalid
	}
	return ng.GenResultSet(response)
}

func (s *wireSession) Discard() {
	s.closeOnce.Do(func() { _ = s.transport.Close(); _ = s.conn.Close() })
}

func (s *wireSession) Release() {
	s.closeOnce.Do(func() {
		if s.authenticated {
			_ = s.rpcCall(context.Background(), func() error { return s.rpc.Signout(s.id) })
		}
		_ = s.transport.Close()
		_ = s.conn.Close()
	})
}

func decodeSDKTable(rs *ng.ResultSet, cfg Config) (wireTable, error) {
	t := wireTable{Columns: rs.GetColNames()}
	if rs.IsSetPlanDesc() {
		t.PlanJSON, _ = json.Marshal(rs.GetPlanDesc())
		if len(t.PlanJSON) > cfg.ReadBytes {
			return wireTable{}, ErrBudget
		}
	}
	if rs.GetRowSize() > cfg.SnapshotRecords+1 {
		return wireTable{}, ErrBudget
	}
	used := 0
	for i := 0; i < rs.GetRowSize(); i++ {
		record, err := rs.GetRowValuesByIndex(i)
		if err != nil {
			return wireTable{}, fmt.Errorf("%w: row", ErrInvalid)
		}
		row := make([]string, len(t.Columns))
		for j := range row {
			v, err := record.GetValueByIndex(j)
			if err != nil {
				return wireTable{}, fmt.Errorf("%w: cell", ErrInvalid)
			}
			switch {
			case v.IsString():
				row[j], err = v.AsString()
			case v.IsInt():
				var n int64
				n, err = v.AsInt()
				row[j] = strconv.FormatInt(n, 10)
			case v.IsBool():
				var value bool
				value, err = v.AsBool()
				row[j] = strconv.FormatBool(value)
			case v.IsNull():
				row[j] = ""
			case v.IsEmpty() && optionalSchemaCell(t.Columns, j):
				// 3.8.0 returns an unset Value union for absent metadata.
				// Do not permit EMPTY for required metadata or graph properties.
				row[j] = ""
			case v.IsList() && indexColumnsCell(t.Columns, j):
				row[j], err = decodeIndexColumns(v, cfg.ReadBytes-used)
				if err != nil {
					return wireTable{}, err
				}
			default:
				return wireTable{}, fmt.Errorf("%w: unsupported result type (column=%q type=%s)", ErrInvalid,
					t.Columns[j], v.GetType())
			}
			if err != nil {
				return wireTable{}, fmt.Errorf("%w: result value", ErrInvalid)
			}
			used += len(row[j])
			if used > cfg.ReadBytes {
				return wireTable{}, ErrBudget
			}
		}
		t.Rows = append(t.Rows, row)
	}
	return t, nil
}

func optionalSchemaCell(columns []string, index int) bool {
	if slices.Equal(columns, []string{
		"ID", "Name", "Partition Number", "Replica Factor", "Charset", "Collate",
		"Vid Type", "Comment",
	}) {
		return index == 7
	}
	if slices.Equal(columns, []string{"Field", "Type", "Null", "Default", "Comment"}) {
		return index == 3 || index == 4
	}
	return false
}

func indexColumnsCell(columns []string, index int) bool {
	return index == 2 && (slices.Equal(columns, []string{"Index Name", "By Tag", "Columns"}) || slices.Equal(columns,
		[]string{"Index Name", "By Edge", "Columns"}))
}

// SHOW TAG/EDGE INDEXES returns a list of property names, not a graph value.
// Keep a lossless JSON representation and charge delimiters/escaping to the
// remaining read budget. Unknown or nested element types remain invalid.
func decodeIndexColumns(value *ng.ValueWrapper, budget int) (string, error) {
	if budget < 2 {
		return "", ErrBudget
	}
	items, err := value.AsList()
	if err != nil {
		return "", ErrInvalid
	}
	var out strings.Builder
	out.WriteByte('[')
	for i, item := range items {
		if !item.IsString() {
			return "", fmt.Errorf("%w: index column must be string", ErrInvalid)
		}
		s, err := item.AsString()
		if err != nil {
			return "", ErrInvalid
		}
		if len(s) > budget-out.Len()-1 {
			return "", ErrBudget
		}
		encoded, err := json.Marshal(s)
		if err != nil {
			return "", ErrInvalid
		}
		comma := 0
		if i > 0 {
			comma = 1
		}
		if out.Len()+comma+len(encoded)+1 > budget {
			return "", ErrBudget
		}
		if comma != 0 {
			out.WriteByte(',')
		}
		out.Write(encoded)
	}
	out.WriteByte(']')
	return out.String(), nil
}
