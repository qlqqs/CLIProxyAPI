package wsrelay

import (
	"context"
	"testing"
	"testing/synctest"
)

func TestSessionTerminalPreservesBufferedFramesAndDoesNotBlockReader(t *testing.T) {
	for _, terminalType := range []string{MessageTypeStreamEnd, MessageTypeHTTPResp} {
		t.Run(terminalType, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess := &session{closed: make(chan struct{})}
				slow := newPendingRequest(context.Background())
				other := newPendingRequest(context.Background())
				sess.pending.Store("slow", slow)
				sess.pending.Store("other", other)
				for i := 0; i < pendingChannelBuffer; i++ {
					sess.dispatch(Message{ID: "slow", Type: MessageTypeStreamChunk, Payload: map[string]any{"seq": i}})
				}
				readerDone := make(chan struct{})
				go func() {
					sess.dispatch(Message{ID: "slow", Type: terminalType})
					sess.dispatch(Message{ID: "other", Type: MessageTypeHTTPResp})
					close(readerDone)
				}()
				synctest.Wait()
				select {
				case <-readerDone:
				default:
					t.Fatal("normal terminal blocked the shared reader")
				}
				for msg := range other.ch {
					if msg.Type != MessageTypeHTTPResp {
						t.Fatalf("other request received %s", msg.Type)
					}
				}
				index := 0
				for msg := range slow.ch {
					if index < pendingChannelBuffer {
						if msg.Type != MessageTypeStreamChunk || msg.Payload["seq"] != index {
							t.Fatalf("frame %d was lost or reordered: %+v", index, msg)
						}
					} else if msg.Type != terminalType {
						t.Fatalf("last frame = %s, want %s", msg.Type, terminalType)
					}
					index++
				}
				if index != pendingChannelBuffer+1 {
					t.Fatalf("received %d frames, want %d", index, pendingChannelBuffer+1)
				}
			})
		})
	}
}

func TestSessionPendingTerminalUnblocksOnCancellation(t *testing.T) {
	for _, stopSession := range []bool{false, true} {
		name := "request_cancel"
		if stopSession {
			name = "session_close"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				sess := &session{closed: make(chan struct{})}
				req := newPendingRequest(ctx)
				sess.pending.Store("slow", req)
				for i := 0; i < pendingChannelBuffer; i++ {
					sess.dispatch(Message{ID: "slow", Type: MessageTypeStreamChunk})
				}
				sess.dispatch(Message{ID: "slow", Type: MessageTypeStreamEnd})
				synctest.Wait()
				if stopSession {
					sess.cleanup(errClosed)
				} else {
					cancel()
				}
				synctest.Wait()
				var last Message
				for msg := range req.ch {
					if msg.Type == MessageTypeStreamEnd {
						t.Fatal("canceled pending terminal incorrectly reported success")
					}
					last = msg
				}
				if last.Type != MessageTypeError {
					t.Fatalf("last frame = %s, want cancellation error", last.Type)
				}
			})
		})
	}
}

func TestSessionErrorTerminalStillPrioritizesFailure(t *testing.T) {
	sess := &session{closed: make(chan struct{})}
	req := newPendingRequest(context.Background())
	sess.pending.Store("failed", req)
	for i := 0; i < pendingChannelBuffer; i++ {
		sess.dispatch(Message{ID: "failed", Type: MessageTypeStreamChunk})
	}
	sess.dispatch(Message{ID: "failed", Type: MessageTypeError, Payload: map[string]any{"error": "upstream failed"}})
	var last Message
	for msg := range req.ch {
		last = msg
	}
	if last.Type != MessageTypeError || last.Payload["error"] != "upstream failed" {
		t.Fatalf("explicit failure was not preserved: %+v", last)
	}
}

func TestSessionOldTerminalCancellationDoesNotCancelReusedRequestID(t *testing.T) {
	// Keep the HTTP test server outside the bubble; only the session's request
	// and terminal lifecycle needs deterministic scheduling.
	sess, _, cleanup := newTestWebsocketPair(t)
	defer cleanup()
	synctest.Test(t, func(t *testing.T) {
		sess.closed = make(chan struct{})
		defer sess.cleanup(errClosed)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		old, errRequest := sess.request(ctx, Message{ID: "reused", Type: MessageTypeHTTPReq})
		if errRequest != nil {
			t.Fatal(errRequest)
		}
		for range pendingChannelBuffer {
			sess.dispatch(Message{ID: "reused", Type: MessageTypeStreamChunk})
		}
		sess.dispatch(Message{ID: "reused", Type: MessageTypeStreamEnd})
		synctest.Wait()
		replacement, errReplacement := sess.request(context.Background(), Message{ID: "reused", Type: MessageTypeHTTPReq})
		if errReplacement != nil {
			t.Fatal(errReplacement)
		}
		cancel()
		synctest.Wait()
		if _, exists := sess.pending.Load("reused"); !exists {
			t.Fatal("old request cancellation removed the replacement request")
		}
		sess.dispatch(Message{ID: "reused", Type: MessageTypeHTTPResp})
		synctest.Wait()
		if msg, ok := <-replacement; !ok || msg.Type != MessageTypeHTTPResp {
			t.Fatalf("replacement lost response: %+v, open=%v", msg, ok)
		}
		for range old {
		}
	})
}
