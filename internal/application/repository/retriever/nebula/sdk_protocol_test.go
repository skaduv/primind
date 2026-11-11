package nebula

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vesoft-inc/fbthrift/thrift/lib/go/thrift"
	nv "github.com/vesoft-inc/nebula-go/v3/nebula"
	gr "github.com/vesoft-inc/nebula-go/v3/nebula/graph"
)

// Only a net.Pipe is used: no listening port, real database, or successful
// Repository substitute. The SDK serializes and decodes actual Thrift frames.
type pipeGraphHandler struct {
	gr.GraphService
	authCode  nv.ErrorCode
	queryCode nv.ErrorCode
	queries   chan string
	signouts  atomic.Int32
}

func (h *pipeGraphHandler) VerifyClientVersion(context.Context,
	*gr.VerifyClientVersionReq,
) (*gr.VerifyClientVersionResp, error) {
	return &gr.VerifyClientVersionResp{ErrorCode: nv.ErrorCode_SUCCEEDED}, nil
}

func (h *pipeGraphHandler) Authenticate(_ context.Context, u, p []byte) (*gr.AuthResponse, error) {
	if string(u) != "synthetic" || string(p) != "synthetic" {
		return &gr.AuthResponse{ErrorCode: nv.ErrorCode_E_BAD_USERNAME_PASSWORD}, nil
	}
	id := int64(42)
	return &gr.AuthResponse{ErrorCode: h.authCode, SessionID: &id}, nil
}

func (h *pipeGraphHandler) Signout(_ context.Context, id int64) error {
	if id == 42 {
		h.signouts.Add(1)
	}
	return nil
}

func (h *pipeGraphHandler) ExecuteWithParameter(_ context.Context, id int64, stmt []byte,
	_ map[string]*nv.Value,
) (*gr.ExecutionResponse, error) {
	h.queries <- string(stmt)
	code := h.queryCode
	if string(stmt) == "USE local_protocol_only;" {
		code = nv.ErrorCode_SUCCEEDED
	}
	if id != 42 {
		code = nv.ErrorCode_E_SESSION_INVALID
	}
	return &gr.ExecutionResponse{ErrorCode: code}, nil
}

func TestGeneratedSDKProtocolFramesAndHalfInitialization(t *testing.T) {
	for _, mode := range []string{"success", "auth-failure", "partial"} {
		t.Run(mode, func(t *testing.T) {
			local, remote := net.Pipe()
			defer func() { _ = local.Close() }()
			defer func() { _ = remote.Close() }()
			h := &pipeGraphHandler{queries: make(chan string, 4)}
			if mode == "auth-failure" {
				h.authCode = nv.ErrorCode_E_BAD_USERNAME_PASSWORD
			}
			if mode == "partial" {
				h.queryCode = nv.ErrorCode_E_PARTIAL_SUCCEEDED
			}
			socket, err := thrift.NewSocket(thrift.SocketConn(remote), thrift.SocketTimeout(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			transport := thrift.NewHeaderTransport(thrift.NewBufferedTransportFactory(128 << 10).GetTransport(socket))
			protocol := thrift.NewHeaderProtocolFactory().GetProtocol(transport)
			processor := gr.NewGraphServiceProcessor(h)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					keep, err := thrift.ProcessContext(context.Background(), processor, protocol, protocol)
					if err != nil || !keep {
						return
					}
				}
			}()
			cfg := testConfig()
			cfg.SocketTimeout = time.Second
			s, err := newWireSessionOnConn(context.Background(), cfg, local)
			if mode == "auth-failure" {
				if err == nil || s != nil {
					t.Fatal("authentication accepted")
				}
				waitSignal(t, done)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := <-h.queries; got != "USE local_protocol_only;" {
				t.Fatalf("wrong initial scope %q", got)
			}
			d := &sdkDriver{cfg: cfg, sessions: make(chan sdkSession, 1)}
			d.sessions <- s
			out := d.Execute(context.Background(), "RETURN \"literal\";")
			if (mode == "success") != (out.Err == nil) || !out.Sent {
				t.Fatalf("protocol status lost: %v", out.Err)
			}
			if got := <-h.queries; got != "RETURN \"literal\";" {
				t.Fatal("statement changed")
			}
			d.Close()
			waitSignal(t, done)
			if h.signouts.Load() != 1 {
				t.Fatal("session not signed out")
			}
		})
	}
}
