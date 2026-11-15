package envelope

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/Disesfgewu/agentkit/agentlog"
)

const defaultMaxEventBytes = 4 << 20

// Client calls endpoints served by Handle. A nil *Client is not usable; the
// zero value needs BaseURL.
type Client struct {
	BaseURL string
	// Bearer is sent as the Authorization bearer credential when set.
	Bearer string
	// HTTP defaults to a client without a total timeout, because a total
	// timeout also cuts off a stream that is still delivering. Bound a call
	// with its context instead.
	HTTP *http.Client
	// MaxBodyBytes bounds a non-streamed response; zero uses 16 MiB.
	MaxBodyBytes int64
	// MaxEventBytes bounds one streamed event; zero uses 4 MiB.
	MaxEventBytes int
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) request(ctx context.Context, method, path, accept string, in any, parent agentlog.Receipt) (*http.Request, *Problem) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, NewProblem(http.StatusBadRequest, CodeInvalidJSON, "request is not JSON-serializable: "+err.Error()).Wrap(err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return nil, NewProblem(0, CodeTransport, err.Error()).Wrap(err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", accept)
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	}
	if parent.Session != "" {
		req.Header.Set(HeaderParentSession, parent.Session)
		req.Header.Set(HeaderParentCallID, strconv.FormatUint(parent.CallID, 10))
	}
	return req, nil
}

func transportProblem(ctx context.Context, err error) *Problem {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return AsProblem(ctxErr)
	}
	p := NewProblem(0, CodeTransport, err.Error()).Wrap(err)
	p.Title, p.Retryable = "No Response", true
	return p
}

// parseEnvelope reads a non-streamed reply. Anything that is not an Envelope,
// such as a proxy error page or a reply from an older service, becomes a
// bad_envelope Problem that keeps the HTTP status.
func parseEnvelope(status int, raw []byte) (Envelope, *Problem) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Version == "" {
		var legacy struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &legacy)
		detail := fmt.Sprintf("HTTP %d with a body that is not an envelope (%d bytes)", status, len(raw))
		if legacy.Error != "" {
			detail += ": " + legacy.Error
		}
		p := NewProblem(status, CodeBadEnvelope, detail)
		if status < 400 {
			p = NewProblem(http.StatusBadGateway, CodeBadEnvelope, detail)
		}
		return Envelope{}, p
	}
	return env, env.Problem()
}

func auditOutput(status int, env Envelope) map[string]any {
	out := map[string]any{"status": status, "code": env.Code}
	if env.Meta.Audit != nil {
		out["server_audit"] = env.Meta.Audit
	}
	if env.OK {
		out["data"] = env.Data
	}
	return out
}

// endOnPanic closes an audit call that a panic would otherwise leave open,
// which would make Recorder.Close wait forever. Defer it directly.
func endOnPanic(span *agentlog.Span, output func() any) {
	if p := recover(); p != nil {
		_ = span.End(output(), nil, fmt.Errorf("panic: %v\n%s", p, debug.Stack()))
		panic(p)
	}
}

// Call sends one request and unwraps the reply. The error is nil or a
// *Problem, whatever went wrong: refused connection, timeout, malformed reply,
// a failure the server reported, or an audit session that could not record
// the call. The call is recorded in the audit session carried by ctx, with a
// reference to the server's own audit record, before the result is returned.
func Call[Out any](ctx context.Context, c *Client, method, path string, in any) (Out, Meta, error) {
	var out Out
	status, env := 0, Envelope{}
	output := func() any { return auditOutput(status, env) }
	ctx, span := agentlog.Start(ctx, "envelope.Call", map[string]any{"method": method, "url": strings.TrimRight(c.BaseURL, "/") + path, "body": in})
	if startErr := span.Err(); startErr != nil {
		return out, Meta{}, auditProblem(startErr)
	}
	defer endOnPanic(span, output)
	problem := func() *Problem {
		req, p := c.request(ctx, method, path, "application/json", in, span.Receipt())
		if p != nil {
			return p
		}
		resp, err := c.http().Do(req)
		if err != nil {
			return transportProblem(ctx, err)
		}
		defer resp.Body.Close()
		status = resp.StatusCode
		raw, p := readBody(ctx, resp.Body, c.MaxBodyBytes)
		if p != nil {
			return p
		}
		if env, p = parseEnvelope(status, raw); p != nil {
			return p
		}
		if out, err = Decode[Out](env); err != nil {
			return AsProblem(err)
		}
		return nil
	}()
	var failure error
	if problem != nil {
		failure = problem
	}
	if auditErr := span.End(output(), nil, failure); auditErr != nil {
		var zero Out
		return zero, env.Meta, auditProblem(auditErr)
	}
	return out, env.Meta, failure
}

// readBody reads a non-streamed reply and rejects one larger than limit.
func readBody(ctx context.Context, body io.Reader, limit int64) ([]byte, *Problem) {
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, transportProblem(ctx, err)
	}
	if int64(len(raw)) > limit {
		return nil, NewProblem(http.StatusBadGateway, CodeBadEnvelope, fmt.Sprintf("response exceeds %d bytes", limit))
	}
	return raw, nil
}

// Stream sends one request and yields the events the server publishes,
// followed by one event of type EventResult whose Data is the result and whose
// Meta is the response metadata. On failure the last yield carries a *Problem
// instead. A stream that ends without a result, skips a sequence number, or
// does not match the digest the server reports is a failure, never a silently
// shorter stream.
//
// The stream is one audit call, open only while the caller ranges over it;
// leaving the loop early cancels the request. Use Collect to get the typed
// result. With iter.Pull2, always call stop.
func Stream(ctx context.Context, c *Client, method, path string, in any) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		status, env, events, terminal := 0, Envelope{}, uint64(0), "none"
		digest := sha256.New()
		output := func() any {
			out := auditOutput(status, env)
			out["events"], out["terminal"] = events, terminal
			if events > 0 {
				out["events_sha256"] = fmt.Sprintf("%x", digest.Sum(nil))
			}
			return out
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		ctx, span := agentlog.Start(ctx, "envelope.Stream", map[string]any{"method": method, "url": strings.TrimRight(c.BaseURL, "/") + path, "body": in})
		if startErr := span.Err(); startErr != nil {
			yield(Event{Type: EventResult, Seq: 1, Meta: &env.Meta}, auditProblem(startErr))
			return
		}
		// The call is still open here only if the caller left the loop or
		// panicked inside it; every other path has already recorded its outcome.
		defer func() {
			p := recover()
			var failure error
			if p != nil {
				failure = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
			}
			_ = span.End(output(), nil, failure)
			if p != nil {
				panic(p)
			}
		}()
		// fail records the failure before reporting it.
		fail := func(p *Problem) {
			terminal = "failed"
			if auditErr := span.End(output(), nil, p); auditErr != nil {
				p = auditProblem(auditErr)
			}
			yield(Event{Type: EventResult, Seq: events + 1, Meta: &env.Meta}, p)
		}
		req, reqErr := c.request(ctx, method, path, "text/event-stream, application/json", in, span.Receipt())
		if reqErr != nil {
			fail(reqErr)
			return
		}
		resp, doErr := c.http().Do(req)
		if doErr != nil {
			fail(transportProblem(ctx, doErr))
			return
		}
		defer resp.Body.Close()
		status = resp.StatusCode

		// finish releases the result only after this side has recorded it.
		finish := func() {
			if p := env.Problem(); p != nil {
				fail(p)
				return
			}
			if env.Meta.Events != events || (events > 0 && env.Meta.EventsSHA256 != fmt.Sprintf("%x", digest.Sum(nil))) {
				fail(NewProblem(http.StatusBadGateway, CodeStreamCorrupt, fmt.Sprintf("received %d events, server reports %d with a different digest", events, env.Meta.Events)))
				return
			}
			terminal = "completed"
			if auditErr := span.End(output(), nil, nil); auditErr != nil {
				terminal = "failed"
				yield(Event{Type: EventResult, Seq: events + 1, Meta: &env.Meta}, auditProblem(auditErr))
				return
			}
			yield(Event{Type: EventResult, Seq: events + 1, Time: env.Meta.Time, Data: env.Data, Meta: &env.Meta}, nil)
		}

		if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "text/event-stream" {
			raw, problem := readBody(ctx, resp.Body, c.MaxBodyBytes)
			if problem != nil {
				fail(problem)
				return
			}
			if env, problem = parseEnvelope(status, raw); problem != nil {
				fail(problem)
				return
			}
			finish()
			return
		}

		maxEvent := c.MaxEventBytes
		if maxEvent <= 0 {
			maxEvent = defaultMaxEventBytes
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), maxEvent)
		var name string
		var data []string
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					name = value
				case "data":
					data = append(data, value)
				}
				continue // comments and unknown fields are ignored
			}
			payload := strings.Join(data, "\n")
			eventName := name
			name, data = "", nil
			if payload == "" {
				continue // the SSE rules do not dispatch an event without data
			}
			if eventName == EventResult {
				if decodeErr := json.Unmarshal([]byte(payload), &env); decodeErr != nil || env.Version == "" {
					env = Envelope{}
					fail(NewProblem(http.StatusBadGateway, CodeBadEnvelope, "stream result is not an envelope"))
					return
				}
				finish()
				return
			}
			var event Event
			if decodeErr := json.Unmarshal([]byte(payload), &event); decodeErr != nil {
				fail(NewProblem(http.StatusBadGateway, CodeBadEnvelope, "stream event is not valid JSON: "+decodeErr.Error()))
				return
			}
			if event.Seq != events+1 {
				fail(NewProblem(http.StatusBadGateway, CodeStreamGap, fmt.Sprintf("expected event %d, received %d", events+1, event.Seq)))
				return
			}
			events++
			digest.Write([]byte(payload))
			digest.Write([]byte{'\n'})
			if !yield(event, nil) {
				terminal = "canceled"
				return
			}
		}
		if scanErr := scanner.Err(); scanErr != nil && !errors.Is(scanErr, bufio.ErrTooLong) {
			fail(transportProblem(ctx, scanErr))
			return
		} else if scanErr != nil {
			fail(NewProblem(http.StatusBadGateway, CodeBadEnvelope, fmt.Sprintf("stream event exceeds %d bytes", maxEvent)))
			return
		}
		truncated := NewProblem(http.StatusBadGateway, CodeStreamTruncated, fmt.Sprintf("stream ended after %d events without a result", events))
		truncated.Retryable = true
		fail(truncated)
	}
}

// Collect drains a Stream and returns its typed result. onEvent, when not nil,
// sees each progress event; returning an error from it stops the stream.
func Collect[Out any](stream iter.Seq2[Event, error], onEvent func(Event) error) (out Out, meta Meta, err error) {
	for event, streamErr := range stream {
		if event.Meta != nil {
			meta = *event.Meta
		}
		if streamErr != nil {
			return out, meta, streamErr
		}
		if event.Type != EventResult {
			if onEvent != nil {
				if err = onEvent(event); err != nil {
					return out, meta, err
				}
			}
			continue
		}
		if len(event.Data) > 0 {
			if err = json.Unmarshal(event.Data, &out); err != nil {
				return out, meta, NewProblem(http.StatusBadGateway, CodeBadEnvelope, "result does not match the expected type: "+err.Error()).Wrap(err)
			}
		}
		return out, meta, nil
	}
	return out, meta, NewProblem(http.StatusBadGateway, CodeStreamTruncated, "stream ended without a result")
}
