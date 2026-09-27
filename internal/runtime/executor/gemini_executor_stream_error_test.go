package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestGeminiExecutorStreamReadFailureDoesNotEmitCompletion(t *testing.T) {
	const partialSSE = "data: {\"responseId\":\"stream-read-test\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"
	for _, test := range []struct {
		name          string
		body          func() io.Reader
		wantError     error
		wantCompleted int
	}{
		{
			name:      "read failure after partial output",
			body:      func() io.Reader { return io.MultiReader(strings.NewReader(partialSSE), unexpectedEOFReader{}) },
			wantError: io.ErrUnexpectedEOF,
		},
		{
			name:      "read failure before output",
			body:      func() io.Reader { return unexpectedEOFReader{} },
			wantError: io.ErrUnexpectedEOF,
		},
		{
			name:          "clean EOF retains successful finalization",
			body:          func() io.Reader { return strings.NewReader(partialSSE) },
			wantCompleted: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			closed := make(chan struct{})
			rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       &closeSignalReadCloser{ReadCloser: io.NopCloser(test.body()), closed: closed},
					Request:    r,
				}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
			exec := NewGeminiExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: "gemini-stream-read", Provider: "gemini", Attributes: map[string]string{"api_key": "test-key"}}
			req := cliproxyexecutor.Request{
				Model:   "gemini-2.5-flash",
				Payload: []byte(`{"model":"gemini-2.5-flash","input":"hi","stream":true}`),
			}
			result, errStream := exec.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
			if errStream != nil {
				t.Fatal(errStream)
			}
			completed, failures := 0, 0
			for chunk := range result.Chunks {
				if bytes.Contains(chunk.Payload, []byte(`"type":"response.completed"`)) {
					completed++
				}
				if chunk.Err != nil {
					failures++
					if !errors.Is(chunk.Err, test.wantError) {
						t.Errorf("stream error = %v, want %v", chunk.Err, test.wantError)
					}
				}
			}
			wantFailures := 0
			if test.wantError != nil {
				wantFailures = 1
			}
			if failures != wantFailures || completed != test.wantCompleted {
				t.Fatalf("completed=%d, failures=%d; want completed=%d, failures=%d", completed, failures, test.wantCompleted, wantFailures)
			}
			select {
			case <-closed:
			default:
				t.Fatal("stream response body was not closed")
			}
		})
	}
}
