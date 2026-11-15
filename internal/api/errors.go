package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/kamboj-sajan/ledgerline/internal/payments"
)

// Error types, as in the JSON "type" field.
const (
	typeInvalidRequest = "invalid_request_error"
	typeAuthentication = "authentication_error"
	typeIdempotency    = "idempotency_error"
	typeCard           = "card_error"
	typeAPI            = "api_error"
)

// apiError is an error response: {"error": {"type", "code", "message"}}.
//
// stored says whether the response belongs to a request that started
// executing. Those are saved under the idempotency key and replayed like a
// success. Errors raised before execution (authentication, validation, key in
// use, not found, transient failures) are not stored, so they never use up
// the key.
type apiError struct {
	Status      int    `json:"-"`
	Type        string `json:"type"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Param       string `json:"param,omitempty"`
	DeclineCode string `json:"decline_code,omitempty"`
	PaymentID   string `json:"payment_id,omitempty"`
	stored      bool
	retryAfter  int // seconds; sets Retry-After when > 0
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

type errorBody struct {
	Error *apiError `json:"error"`
}

func badRequest(code, param, format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Type: typeInvalidRequest, Code: code, Param: param, Message: fmt.Sprintf(format, args...)}
}

func paramMissing(param string) *apiError {
	return badRequest("parameter_missing", param, "missing required parameter %q", param)
}

func paramInvalid(param, format string, args ...any) *apiError {
	return badRequest("parameter_invalid", param, format, args...)
}

var (
	errUnauthenticated = &apiError{Status: http.StatusUnauthorized, Type: typeAuthentication, Code: "invalid_api_key",
		Message: "provide a valid API key as 'Authorization: Bearer <key>'"}
	errKeyInUse = &apiError{Status: http.StatusConflict, Type: typeIdempotency, Code: "idempotency_key_in_use",
		Message: "another request with this Idempotency-Key is still running; retry after the Retry-After delay", retryAfter: 1}
	errKeyReused = &apiError{Status: http.StatusConflict, Type: typeIdempotency, Code: "idempotency_key_reused",
		Message: "this Idempotency-Key was already used for a different request"}
	errNotFound = &apiError{Status: http.StatusNotFound, Type: typeInvalidRequest, Code: "resource_missing",
		Message: "no such resource"}
	errInternal = &apiError{Status: http.StatusInternalServerError, Type: typeAPI, Code: "internal_error",
		Message: "an internal error occurred; it is safe to retry with the same Idempotency-Key"}
)

// fromDomain maps an error from the domain packages to an API error.
// Business-rule rejections are stored; anything unrecognised is an
// unstored 500.
func fromDomain(err error) *apiError {
	var ae *apiError
	var te *payments.TransitionError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.As(err, &te):
		return &apiError{Status: http.StatusConflict, Type: typeInvalidRequest, Code: "invalid_state_transition", Message: te.Error(), stored: true}
	case errors.Is(err, payments.ErrAmountTooLarge):
		return &apiError{Status: http.StatusBadRequest, Type: typeInvalidRequest, Code: "amount_too_large", Param: "amount", Message: err.Error(), stored: true}
	case errors.Is(err, payments.ErrInvalidAmount):
		return &apiError{Status: http.StatusBadRequest, Type: typeInvalidRequest, Code: "parameter_invalid", Param: "amount", Message: err.Error(), stored: true}
	case errors.Is(err, payments.ErrNotFound):
		return errNotFound
	default:
		return errInternal
	}
}
