package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestForwardResponsesWebsocketDataClosePreservesPendingError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	for _, test := range []struct {
		name     string
		status   int
		suppress bool
	}{
		{name: "request fault", status: http.StatusBadRequest},
		{name: "upstream transport fault", status: http.StatusBadGateway},
		{name: "suppressed pinned auth failure", status: http.StatusUnauthorized, suppress: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Both channels are ready, as they are after a stream producer sends its
			// buffered error and exits. Neither select outcome may lose that error.
			for iteration := 0; iteration < 100; iteration++ {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
				data := make(chan []byte)
				errs := make(chan *interfaces.ErrorMessage, 1)
				original := &interfaces.ErrorMessage{StatusCode: test.status, Error: errors.New("original upstream error")}
				errs <- original
				close(errs)
				close(data)
				cancelCalls := 0
				var canceledWith error
				_, _, _, got, errForward := h.forwardResponsesWebsocket(c, nil, func(values ...interface{}) {
					cancelCalls++
					if len(values) == 1 {
						canceledWith, _ = values[0].(error)
					}
				}, data, errs, nil, "pending-error", responsesWebsocketForwardOptions{
					suppressError: func(*interfaces.ErrorMessage) bool { return test.suppress },
				})
				if got != original || canceledWith != original.Error || cancelCalls != 1 {
					t.Fatalf("error lost: returned=%v, canceled=%v, cancel calls=%d", got, canceledWith, cancelCalls)
				}
				if test.suppress && errForward != nil {
					t.Fatalf("suppressed error closed the session: %v", errForward)
				}
				if !test.suppress && !errors.Is(errForward, websocket.ErrCloseSent) {
					t.Fatalf("terminal error did not close the session: %v", errForward)
				}
			}
		})
	}
}

type blockedResponsesWebsocketExecutor struct {
	websocketCaptureExecutor
	stage   string
	started chan context.Context
	release chan struct{}
}

func (*blockedResponsesWebsocketExecutor) Identifier() string { return "codex" }

func (e *blockedResponsesWebsocketExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.started <- ctx
	wait := func() {
		select {
		case <-ctx.Done():
		case <-e.release:
		}
	}
	if e.stage == "execute" {
		wait()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("test cleanup")
	}
	out := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(out)
		if e.stage == "forward" {
			select {
			case out <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.created","response":{"id":"blocked-turn"}}`)}:
			case <-ctx.Done():
				return
			}
		}
		wait()
	}()
	return &coreexecutor.StreamResult{Chunks: out}, nil
}

func newBlockedResponsesWebsocket(t *testing.T, stage string) (*websocket.Conn, *blockedResponsesWebsocketExecutor, <-chan struct{}) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	exec := &blockedResponsesWebsocketExecutor{
		stage: stage, started: make(chan context.Context, 1), release: make(chan struct{}),
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth := &coreauth.Auth{ID: "ws-disconnect-auth", Provider: "codex", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "ws-disconnect-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	done := make(chan struct{})
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		defer close(done)
		h.ResponsesWebsocket(c)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if errDial != nil {
		t.Fatal(errDial)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		close(exec.release)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("handler did not stop during cleanup")
		}
	})
	return conn, exec, done
}

func TestResponsesWebsocketDisconnectCancelsExecution(t *testing.T) {
	for _, stage := range []string{"execute", "bootstrap", "forward"} {
		for _, queued := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/queued_%t", stage, queued), func(t *testing.T) {
				conn, exec, done := newBlockedResponsesWebsocket(t, stage)
				request := []byte(`{"type":"response.create","model":"ws-disconnect-model","input":[]}`)
				if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
					t.Fatal(errWrite)
				}
				var execCtx context.Context
				select {
				case execCtx = <-exec.started:
				case <-time.After(5 * time.Second):
					t.Fatal("executor not invoked")
				}
				if stage == "forward" {
					read := make(chan error, 1)
					go func() {
						_, _, errRead := conn.ReadMessage()
						read <- errRead
					}()
					select {
					case errRead := <-read:
						if errRead != nil {
							t.Fatal(errRead)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("first stream payload was not forwarded")
					}
				}
				if queued {
					if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
						t.Fatal(errWrite)
					}
				}
				_ = conn.Close()
				select {
				case <-execCtx.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("client disconnect did not cancel upstream execution")
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("handler did not release its reader and execution session")
				}
			})
		}
	}
}

func TestResponsesWebsocketQueuedRequestsAreBounded(t *testing.T) {
	conn, exec, done := newBlockedResponsesWebsocket(t, "bootstrap")
	request := []byte(`{"type":"response.create","model":"ws-disconnect-model","input":[]}`)
	if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
		t.Fatal(errWrite)
	}
	var execCtx context.Context
	select {
	case execCtx = <-exec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("executor not invoked")
	}
	for i := 0; i <= wsMaxQueuedRequests; i++ {
		if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	select {
	case <-execCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("full request queue blocked the reader instead of canceling the session")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queue overflow leaked the handler")
	}
}
