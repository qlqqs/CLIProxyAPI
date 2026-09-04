# Error Handling

> How errors are handled in this project.

---

## Overview

Errors are ordinary Go values. Add context at package boundaries, preserve the
cause, classify the error once, and convert it to a protocol response at the
HTTP/stream boundary. The service has several providers and API protocols, so
the error's machine-readable status and retry semantics must survive wrapping.
Never expose an arbitrary upstream error or secret directly to a client or an
ordinary log line.

---

## Error Types

- sdk/cliproxy/auth.Error is the provider-agnostic authentication error. It
  carries Code, Message, Retryable, and optional HTTPStatus, and exposes
  StatusCode() and IsRequestScoped().
- Use auth.WithCause when an auth error needs to retain an upstream cause. The
  wrapper supports errors.Is/errors.As while keeping the canonical auth fields
  available.
- internal/interfaces.ErrorMessage is the internal HTTP hand-off: it carries a
  status, an error, optional upstream/add-on headers, or an explicitly trusted
  direct body (DirectResponse, Body, Headers).
- Errors that implement StatusCode() int, RetryAfter() *time.Duration, or
  SafeResponseHeaders() http.Header may add transport metadata. Implement these
  interfaces only when the behavior is part of the error contract.
- sdk/cliproxy/executor.RequestTerminatedError is reserved for a trusted
  in-process component (for example a plugin interceptor) that intentionally
  supplies a complete downstream response without contacting a provider.
- internal/clienterror owns status extraction and request-fault classification.
  HTTPStatusFromError prefers an explicit status, maps context.Canceled to 499,
  and maps context.DeadlineExceeded to 504.

---

## Error Handling Patterns

Wrap errors with operation context and the %w verb:

~~~go
data, err := os.ReadFile(path)
if err != nil {
    return fmt.Errorf("auth filestore: read %s: %w", path, err)
}
~~~

Callers should inspect wrapped errors with errors.Is or errors.As, not by parsing
the formatted string. Keep the original cause when adding an auth classification:

~~~go
classified := &auth.Error{
    Code:       "auth_unavailable",
    Message:    "provider credentials unavailable",
    Retryable:  true,
    HTTPStatus: http.StatusServiceUnavailable,
}
return auth.WithCause(classified, err)
~~~

At the execution boundary, use the shared conversion in
sdk/api/handlers/handlers_execution.go (executionErrorMessage). It first honors
a RequestTerminatedError or another explicitly marked direct response, then
uses clienterror.HTTPStatusFromError, and finally falls back to 500. Request
faults from internal/clienterror.IsRequestFault must not rotate or penalize a
credential; payment (402), rate-limit (429), and provider authentication
failures remain credential failures even when the body uses a generic
invalid-request shape.

Always propagate the request context through I/O and honor cancellation. The
repository permits timeouts only while acquiring credentials, plus the explicit
Codex WebSocket, wsrelay, management APICall, and model-fetch utility
exceptions documented in AGENTS.md. Do not add a timeout around behavior after
an upstream connection has been established.

Clean up resources on every path. Close rows, files, response bodies, gzip
readers, and tickers; roll back failed transactions. If cleanup itself fails,
preserve the primary error and join or report the cleanup error rather than
silently replacing the operation failure. Return errors from helpers instead of
calling log.Fatal/log.Fatalf or panicking in an HTTP handler.

For diagnostics, log a safe classification (logging.SafeErrorDiagnostic or
logging.SafeDiagnosticForLog) and structured context, never an unbounded raw
upstream error. Do not log credentials, authorization headers, request bodies,
or tokens.

---

## API Error Responses

The common response path is:

1. A provider/client/executor returns an error.
2. handlers.executionErrorMessage converts it to interfaces.ErrorMessage.
3. BaseAPIHandler.WriteErrorResponse in
   sdk/api/handlers/handlers_errors.go chooses the status, safe Retry-After and
   allowed passthrough headers.
4. BuildErrorResponseBody in sdk/api/handlers/handlers.go emits the
   OpenAI-compatible JSON envelope.

For ordinary errors, the body is shaped as:

~~~json
{"error":{"message":"...","type":"invalid_request_error","code":"..."}}
~~~

BuildErrorResponseBody preserves an already-valid JSON error body so a provider
payload is not double-encoded. Status-specific defaults include
authentication_error/invalid_api_key for 401, permission_error/insufficient_quota
for 403, rate_limit_error/rate_limit_exceeded for 429, model_not_found for 404,
and server_error/internal_server_error for 5xx.

Only errors explicitly marked by a trusted adapter may set DirectResponse.
Current examples are a plugin RequestTerminatedError and the internal
home-refresh adapter when it carries an explicitly represented upstream
response. Their body and headers are copied and filtered by WriteErrorResponse;
an arbitrary error that merely contains JSON must not be treated as a direct
response. Add-on upstream headers are forwarded only when the configured
passthrough option allows it, and reserved CPA headers are removed.

Streaming handlers use the shared ForwardStream path. Once headers or a first
chunk have been sent, write one sanitized terminal event (provider-specific
where required), flush it, and cancel the execution; do not attempt a second
HTTP status or retry after bytes have reached the client. OpenAI Responses
sanitizes terminal stream errors in
sdk/api/handlers/openai/openai_responses_handlers.go. Claude handlers use their
Claude-compatible error shape in sdk/api/handlers/claude/code_handlers.go.
Management, OAuth callback, health, and other special routes may intentionally
use direct c.JSON/c.String responses when their route contract requires it.

A handler example:

~~~go
resp, headers, errMsg := h.ExecuteWithAuthManager(ctx, h.HandlerType(), model, body, alt)
if errMsg != nil {
    h.WriteErrorResponse(c, errMsg)
    cancel(errMsg.Error)
    return
}
handlers.WriteUpstreamHeaders(c.Writer.Header(), headers)
_, _ = c.Writer.Write(resp)
~~~

---

## Common Mistakes

- Returning fmt.Errorf with %v (or replacing the cause) so errors.Is/errors.As
  and status extraction stop working.
- Logging err.Error() directly when it may contain a token, DSN, cookie,
  authorization header, raw auth JSON, or user prompt.
- Treating every 4xx or a generic invalid_request_error body as a bad credential.
  Use IsRequestFault and its 402/429/DeepSeek authentication exceptions.
- Writing a normal JSON error after a stream has committed headers, or retrying
  after the first byte; use ForwardStream's terminal-error hooks.
- Marking untrusted upstream JSON as DirectResponse, forwarding reserved headers,
  or allowing a plugin/direct body to mutate shared buffers.
- Ignoring context cancellation, close/rollback errors, or using log.Fatal in a
  library/server path.
- Adding a network timeout after connection establishment, which can terminate
  long-lived streams and WebSockets.

References: internal/clienterror/client_error.go,
internal/runtime/executor/helps/home_refresh.go, sdk/cliproxy/auth/errors.go,
sdk/api/handlers/handlers_execution.go, sdk/api/handlers/handlers_errors.go,
sdk/api/handlers/stream_forwarder.go, and the focused tests
sdk/api/handlers/handlers_error_response_test.go.
