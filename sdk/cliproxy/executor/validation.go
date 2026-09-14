package executor

import (
	"context"
	"encoding/json"
	"errors"
)

type requestValidatorKey struct{}

// RequestValidator validates the final server-selected request immediately before execution.
// It must not mutate the request or broaden its credential scope.
type RequestValidator func(context.Context, string, Request) error

// WithRequestValidator installs a server-owned validator that nested execution cannot replace.
func WithRequestValidator(ctx context.Context, validator RequestValidator) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Value(requestValidatorKey{}) != nil || validator == nil {
		return ctx
	}
	return context.WithValue(ctx, requestValidatorKey{}, validator)
}

// ValidateRequest is a no-op for legacy requests without a validator.
func ValidateRequest(ctx context.Context, provider string, req Request) error {
	if ctx == nil {
		return nil
	}
	if validate, ok := ctx.Value(requestValidatorKey{}).(RequestValidator); ok {
		return validate(ctx, provider, req)
	}
	return nil
}

// RequestValidationError is a local request rejection, never an upstream credential failure.
type RequestValidationError struct {
	Code, Message string
	HTTPStatus    int
}

func (e *RequestValidationError) Error() string {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": e.Code, "message": e.Message, "type": "invalid_request_error"}})
	return string(body)
}
func (e *RequestValidationError) StatusCode() int       { return e.HTTPStatus }
func (e *RequestValidationError) IsRequestScoped() bool { return true }

// IsRequestValidationError identifies local rejections through execution wrappers.
func IsRequestValidationError(err error) bool {
	var target *RequestValidationError
	return errors.As(err, &target)
}
