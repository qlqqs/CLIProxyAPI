# Logging Guidelines

> How logging is done in this project.

---

## Overview

Application logging uses logrus throughout the Go server. The shared setup lives
in internal/logging/global_logger.go and is initialized by cmd/server/main.go
with SetupBaseLogger. Keep logs useful for operators while treating provider
credentials, user content, and upstream payloads as secrets. Request capture is
a separate, opt-in facility and has stricter lifecycle rules than ordinary
diagnostic logs.

---

## Log Levels

- Debug is for high-volume diagnostics that are useful while investigating a
  state transition but are not normally actionable (for example config reload
  details in internal/watcher/config_reload.go or model-updater decisions).
- Info is for successful lifecycle events: startup, configuration reload,
  provider connection, route/service changes, and normal request summaries.
- Warn is for recoverable degradation or skipped work, such as an invalid
  optional record, an unavailable optional integration, or a cleanup failure
  that does not prevent the request from completing.
- Error is for an operation failure, a 5xx request, or a recovered panic that
  needs operator attention. Do not use Error merely to print a client 4xx.

GinLogrusLogger maps response statuses to these levels: 5xx to Error, 4xx to
Warn, and all other statuses to Info. Use WithError, WithField, and WithFields
instead of concatenating unstructured context.

---

## Structured Logging and Output

The custom logging.LogFormatter emits a timestamp, request ID, padded level,
caller file/line, message, and only fields in a fixed allowlist. Common fields
include provider, model, plugin_id, version, credential, connection, mode,
budget, state, and reason. Fields that can contain identifiers are quoted where
configured. Add a field only when it is bounded, non-secret, and useful for
correlation.

By default logs go to stdout. When logging-to-file is enabled,
ConfigureLogOutput writes rotating logs/main.log files through lumberjack
(10 MB per file); logs-max-total-size-mb controls the background total-size
cleaner. Use the existing setup rather than replacing the global logger or
writing ad-hoc files.

AI API paths (/v1, /v1beta, /openai/v1, and /backend-api/codex) receive an
8-character request ID. Other paths use the sentinel --------. Query
strings are sanitized with util.MaskSensitiveQuery before the Gin request
summary is emitted. GinLogrusRecovery submits the panic, stack, and path as
logrus fields and returns HTTP 500 (while allowing net/http ErrAbortHandler to
abort normally). The standard formatter currently omits those non-allowlisted
fields from rendered output; keep recovery values non-secret if that allowlist
changes.

Example:

~~~go
log.WithFields(log.Fields{
    "provider": provider,
    "model": model,
    "request_id": requestID,
}).Info("provider request completed")
~~~

---

## What to Log

Record enough context to answer what happened and where:

- lifecycle transitions (startup/shutdown, config reload, provider auth or
  connection state);
- bounded provider/model identifiers, HTTP status, retry/cooldown state, and
  request ID/trace ID when available;
- safe failure signals from logging.SafeErrorDiagnostic(err) or a bounded
  logging.SafeDiagnosticForLog(message);
- request-log cleanup or queue saturation at Warn/Debug as appropriate.

For request-level capture, use RequestLoggingMiddleware with a
logging.RequestLogger. It masks sensitive headers with
util.MaskSensitiveHeaderValue, captures API/WebSocket sections, and can spool
large bodies to temporary FileBodySource files. The response wrapper writes to
the client first; streaming chunks are copied and sent through a bounded,
non-blocking asynchronous channel, so logging I/O cannot hold up an HTTP
response. Temporary sources must be finalized and cleaned up on every path.
Home mode forwards assembled request logs through Redis after capture.

Clone mutable headers, byte slices, and maps before asynchronous publication.
Keep request IDs and trace IDs consistent across middleware, executor metadata,
and request logs.

---

## What Not to Log

Ordinary logs must never include:

- access/refresh/ID tokens, API keys, client secrets, private keys, JWTs,
  passwords, cookies, authorization/proxy-authorization headers, or raw auth
  JSON;
- DSNs or URL userinfo (including proxy credentials);
- raw user prompts, complete request/response bodies, or arbitrary upstream
  error strings;
- unbounded values, multiline user-controlled text, or query parameters that
  may carry credentials.

Use SafeDiagnosticForLog for a bounded one-line excerpt; it redacts sensitive
assignments, Bearer/Basic values, URL userinfo, and preserves the
access-token-expired signal. Use SafeErrorDiagnostic when only allowlisted
signals (timeout, cancellation, DNS/proxy/TLS failure, status, OAuth error
category, EOF) are needed. These helpers are not a license to log secrets in a
new field.

Request logging is intentionally opt-in because it can persist payloads. Enable
it only for an approved diagnostic window, rely on the existing masking and
body-source cleanup, and avoid copying captured bodies into ordinary logrus
messages.

---

## Common Mistakes

- Calling fmt.Printf, log.Printf, or a second logger instead of logrus, which
  bypasses the formatter, request ID, and output rotation.
- Logging err.Error(), headers, DSNs, or upstream JSON directly.
- Adding arbitrary fields to the formatter or putting secrets in an otherwise
  safe field such as credential.
- Blocking a handler on request-log disk/Redis I/O or using an unbounded stream
  channel.
- Forgetting to clone data sent to an async logger, or leaving temporary body
  sources behind after a request.
- Logging every client 4xx at Error, making operator alerts noisy.
- Logging panic values that contain request credentials without considering
  redaction; use the recovery path and safe diagnostics.

References: internal/logging/global_logger.go, internal/logging/gin_logger.go,
internal/logging/diagnostic.go, internal/api/middleware/request_logging.go,
internal/api/middleware/response_writer.go, internal/logging/request_logger.go,
and internal/logging/request_logger_streaming.go.
