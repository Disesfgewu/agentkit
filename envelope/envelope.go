// Package envelope is the one response format every service in the system
// speaks, and the only code that should write or read it.
//
// Every HTTP reply is an Envelope: success and failure have the same shape, so
// a caller always unwraps it the same way and never has to guess what an
// unexpected status, a panic, or a proxy error page looks like. Handle wraps a
// typed function as a server endpoint; Call and Stream are the matching client.
// Both ends record the exchange in the agentlog audit session automatically and
// cross-reference each other, so a contributor who uses this package gets the
// audit trail without writing any logging code.
package envelope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Disesfgewu/agentkit/agentlog"
)

// Version is the wire format version carried by every Envelope.
const Version = "1"

// CodeOK is the Code of every successful Envelope.
const CodeOK = "ok"

// Problem codes produced by this package. Applications add their own.
const (
	CodeInvalidJSON      = "invalid_json"
	CodeBodyTooLarge     = "body_too_large"
	CodeTimeout          = "timeout"
	CodeCanceled         = "canceled"
	CodeInternal         = "internal"
	CodeInternalPanic    = "internal_panic"
	CodeAuditUnavailable = "audit_unavailable"
	CodeTransport        = "transport"
	CodeBadEnvelope      = "bad_envelope"
	CodeStreamTruncated  = "stream_truncated"
	CodeStreamGap        = "stream_gap"
	CodeStreamCorrupt    = "stream_corrupt"
)

// StatusClientClosedRequest is the conventional status for a request the
// client abandoned. net/http has no constant for it.
const StatusClientClosedRequest = 499

// Headers that carry the caller's audit position to the server, so the two
// hash chains reference each other.
const (
	HeaderParentSession = "Audit-Parent-Session"
	HeaderParentCallID  = "Audit-Parent-Call-Id"
)

// Envelope is the body of every response. Exactly one of Data and Error is
// meaningful: OK is true and Error is nil, or OK is false and Data is null.
type Envelope struct {
	Version string `json:"version"`
	OK      bool   `json:"ok"`
	// Code is CodeOK on success and the Problem code on failure.
	Code string `json:"code"`
	// Message is empty on success and a human-readable summary on failure.
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Error   *Problem        `json:"error"`
	Meta    Meta            `json:"meta"`
}

// Meta is response metadata that is not part of the payload.
type Meta struct {
	// Audit locates the server-side audit record of this response. It is set
	// only after that record is durable.
	Audit *agentlog.Receipt `json:"audit,omitempty"`
	Time  time.Time         `json:"t"`
	// Events and EventsSHA256 describe the progress events sent before a
	// streamed result, so the receiver can check that it saw all of them.
	Events       uint64 `json:"events,omitempty"`
	EventsSHA256 string `json:"events_sha256,omitempty"`
}

// FieldError reports one invalid input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Problem is the error half of an Envelope. Its first five members are the
// RFC 9457 problem details members, so it converts to application/problem+json
// at an external boundary without renaming anything.
type Problem struct {
	Type     string `json:"type,omitempty"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	// Code is a stable machine-readable identifier; branch on it, not on Detail.
	Code string `json:"code"`
	// Retryable reports whether repeating the same request may succeed.
	Retryable bool         `json:"retryable"`
	Errors    []FieldError `json:"errors,omitempty"`
	cause     error
}

// NewProblem builds a Problem whose Title is the standard text for status.
func NewProblem(status int, code, detail string) *Problem {
	title := http.StatusText(status)
	if status == StatusClientClosedRequest {
		title = "Client Closed Request"
	}
	if title == "" {
		title = "Error"
	}
	return &Problem{Title: title, Status: status, Detail: detail, Code: code, Retryable: retryableStatus(status)}
}

// Wrap records the underlying error so errors.Is and errors.As see through p.
// The cause is never serialized.
func (p *Problem) Wrap(err error) *Problem {
	p.cause = err
	return p
}

func (p *Problem) Error() string {
	if p == nil {
		return "nil problem"
	}
	if p.Detail != "" {
		return p.Code + ": " + p.Detail
	}
	return p.Code + ": " + p.Title
}

func (p *Problem) Unwrap() error {
	if p == nil {
		return nil
	}
	return p.cause
}

// nilProblem replaces a nil *Problem that was returned as a non-nil error,
// which is a bug in the caller and must not be mistaken for success.
func nilProblem() *Problem {
	return NewProblem(http.StatusInternalServerError, CodeInternal, "a nil *Problem was returned as an error")
}

// auditProblem reports that the audit session could not record a call.
func auditProblem(err error) *Problem {
	return NewProblem(http.StatusServiceUnavailable, CodeAuditUnavailable, err.Error()).Wrap(err)
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// AsProblem converts any error to a Problem, so nothing reaches the wire or
// the caller in another shape. A nil error returns nil.
func AsProblem(err error) *Problem {
	if err == nil {
		return nil
	}
	var p *Problem
	if errors.As(err, &p) {
		if p == nil {
			return nilProblem()
		}
		return p
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return NewProblem(http.StatusRequestEntityTooLarge, CodeBodyTooLarge, fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit)).Wrap(err)
	case errors.Is(err, agentlog.ErrClosed):
		return auditProblem(err)
	case errors.Is(err, context.DeadlineExceeded):
		return NewProblem(http.StatusGatewayTimeout, CodeTimeout, err.Error()).Wrap(err)
	case errors.Is(err, context.Canceled):
		return NewProblem(StatusClientClosedRequest, CodeCanceled, err.Error()).Wrap(err)
	}
	return NewProblem(http.StatusInternalServerError, CodeInternal, err.Error()).Wrap(err)
}

// Success builds the Envelope for a result. A value that cannot be encoded
// becomes a failure Envelope rather than a broken response.
func Success(data any) Envelope {
	raw, err := json.Marshal(data)
	if err != nil {
		return Failure(NewProblem(http.StatusInternalServerError, CodeInternal, "result is not JSON-serializable: "+err.Error()))
	}
	return Envelope{Version: Version, OK: true, Code: CodeOK, Data: raw, Meta: Meta{Time: time.Now().UTC()}}
}

// Failure builds the Envelope for an error.
func Failure(p *Problem) Envelope {
	if p == nil {
		p = NewProblem(http.StatusInternalServerError, CodeInternal, "failure without a problem")
	}
	return Envelope{Version: Version, Code: p.Code, Message: p.Error(), Error: p, Meta: Meta{Time: time.Now().UTC()}}
}

// Status is the HTTP status an Envelope is sent with.
func (e Envelope) Status() int {
	if e.OK {
		return http.StatusOK
	}
	if e.Error == nil || e.Error.Status < 400 || e.Error.Status > 599 {
		return http.StatusInternalServerError
	}
	return e.Error.Status
}

// Problem returns the Problem of a failed Envelope, or nil. It is the single
// check a receiver needs: a malformed Envelope is also reported as a Problem.
func (e Envelope) Problem() *Problem {
	switch {
	case e.Version == "":
		return NewProblem(http.StatusBadGateway, CodeBadEnvelope, "response is not an envelope")
	case e.OK && e.Error == nil:
		return nil
	case e.Error != nil:
		return e.Error
	}
	p := NewProblem(http.StatusBadGateway, CodeBadEnvelope, "envelope reports failure without an error")
	if e.Code != "" && e.Code != CodeOK {
		p.Code, p.Detail = e.Code, e.Message
	}
	return p
}

// Decode unwraps e and decodes its data into T.
func Decode[T any](e Envelope) (T, error) {
	var out T
	if p := e.Problem(); p != nil {
		return out, p
	}
	if len(e.Data) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(e.Data, &out); err != nil {
		return out, NewProblem(http.StatusBadGateway, CodeBadEnvelope, "envelope data does not match the expected type: "+err.Error()).Wrap(err)
	}
	return out, nil
}

// Event is one progress message sent before a streamed result. The stream ends
// with an event of type EventResult, which carries the Envelope.
type Event struct {
	Type string `json:"type"`
	// Seq starts at 1 and increases by one, so a receiver detects a lost event.
	Seq  uint64          `json:"seq"`
	Time time.Time       `json:"t"`
	Data json.RawMessage `json:"data,omitempty"`
	// Meta is set only on the EventResult a client receives.
	Meta *Meta `json:"-"`
}

// EventResult is the reserved type of the terminal event.
const EventResult = "result"
