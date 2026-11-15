package envelope

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Disesfgewu/agentkit/agentlog"
)

const (
	defaultMaxBodyBytes = 16 << 20
	defaultKeepAlive    = 15 * time.Second
	defaultWriteTimeout = 30 * time.Second
)

// Server holds what every endpoint of one service shares. The zero value and a
// nil *Server are usable.
type Server struct {
	// Recorder is the audit session requests are recorded in. Nil gives each
	// request its own session, which agentlog creates automatically.
	Recorder *agentlog.Recorder
	// MaxBodyBytes bounds a request body; zero uses 16 MiB.
	MaxBodyBytes int64
	// Timeout bounds one request, including a stream; zero means no limit.
	Timeout time.Duration
	// Map converts application errors to Problems. It runs before AsProblem
	// and may return nil to decline.
	Map func(error) *Problem
	// KeepAlive is the idle interval after which a stream sends a comment
	// line; zero uses 15 seconds.
	KeepAlive time.Duration
	// WriteTimeout bounds each write to a streaming client, so a stalled
	// reader cannot hold an audit span open; zero uses 30 seconds.
	WriteTimeout time.Duration
}

func (s *Server) options() Server {
	var o Server
	if s != nil {
		o = Server{Recorder: s.Recorder, MaxBodyBytes: s.MaxBodyBytes, Timeout: s.Timeout, Map: s.Map, KeepAlive: s.KeepAlive, WriteTimeout: s.WriteTimeout}
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = defaultMaxBodyBytes
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = defaultKeepAlive
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = defaultWriteTimeout
	}
	return o
}

func (o Server) problem(err error) *Problem {
	if err == nil {
		return nil
	}
	var p *Problem
	if errors.As(err, &p) && p != nil {
		return p
	}
	if o.Map != nil {
		if p = o.Map(err); p != nil {
			return p
		}
	}
	return AsProblem(err)
}

// Handle serves fn as an endpoint. name is the audit function name of the
// request. The JSON request body is decoded into In; an empty body leaves it
// zero. Whatever fn does, the client receives an Envelope: a returned error is
// converted to a Problem and a panic to an internal_panic Problem.
//
// The request is one audit call. If the audit session cannot record it, fn does
// not run, and a result whose completion record cannot be written is not sent.
//
// A client that asks for text/event-stream receives the events fn publishes
// with Emit, followed by the same Envelope as the terminal event. If fn
// finishes before the stream has started, the reply is a plain JSON Envelope
// with its real HTTP status, so a streaming client must accept both.
func Handle[In, Out any](s *Server, name string, fn func(context.Context, In) (Out, error)) http.Handler {
	o := s.options()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if o.Recorder != nil {
			ctx = agentlog.WithRecorder(ctx, o.Recorder)
		}
		if o.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, o.Timeout)
			defer cancel()
		}
		var in In
		decodeErr := decodeBody(w, r, o.MaxBodyBytes, &in)
		stream := WantsStream(r)
		input := map[string]any{"method": r.Method, "path": r.URL.Path, "stream": stream}
		if parent := parentRef(r); parent != nil {
			input["parent"] = parent
		}
		if decodeErr == nil {
			input["body"] = in
		}
		ctx, span := agentlog.Start(ctx, name, input)
		if err := span.Err(); err != nil {
			Write(w, Failure(NewProblem(http.StatusServiceUnavailable, CodeAuditUnavailable, err.Error())))
			return
		}
		ctx = context.WithValue(ctx, requestKey{}, r)
		var sw *sseWriter
		if stream {
			sw = newSSEWriter(w, o)
			ctx = context.WithValue(ctx, emitKey{}, sw)
			sw.keepAlive()
		}

		var out Out
		err := decodeErr
		if err == nil {
			out, err = protect(ctx, fn, in)
		}
		if sw != nil {
			sw.stopKeepAlive()
		}

		var env Envelope
		problem := o.problem(err)
		if problem != nil {
			env = Failure(problem)
		} else {
			env = Success(out)
		}
		record := map[string]any{"status": env.Status(), "code": env.Code}
		if env.OK {
			record["data"] = env.Data
		}
		if sw != nil {
			env.Meta.Events, env.Meta.EventsSHA256 = sw.summary()
			record["events"], record["events_sha256"] = env.Meta.Events, env.Meta.EventsSHA256
		}
		// A panic is recorded with its stack; the client sees only the value.
		recorded := err
		if problem != nil && problem.Code == CodeInternalPanic && problem.cause != nil {
			recorded = problem.cause
		}
		// The completion record is written before the result is released.
		if auditErr := span.End(record, nil, recorded); auditErr != nil {
			env = Failure(NewProblem(http.StatusServiceUnavailable, CodeAuditUnavailable, auditErr.Error()))
			if sw != nil {
				env.Meta.Events, env.Meta.EventsSHA256 = sw.summary()
			}
		} else {
			receipt := span.Receipt()
			env.Meta.Audit = &receipt
		}
		if sw != nil && sw.finish(env) {
			return
		}
		Write(w, env)
	})
}

// protect runs fn and turns a panic into an error that keeps the stack for the
// audit record. The stack is not sent to the client.
func protect[In, Out any](ctx context.Context, fn func(context.Context, In) (Out, error), in In) (out Out, err error) {
	defer func() {
		if p := recover(); p != nil {
			cause := fmt.Errorf("panic: %v\n%s", p, debug.Stack())
			err = NewProblem(http.StatusInternalServerError, CodeInternalPanic, fmt.Sprintf("panic: %v", p)).Wrap(cause)
		}
	}()
	return fn(ctx, in)
}

func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, in any) error {
	if r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return AsProblem(err)
		}
		return NewProblem(http.StatusBadRequest, CodeInvalidJSON, "request body could not be read").Wrap(err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, in); err != nil {
		return NewProblem(http.StatusBadRequest, CodeInvalidJSON, "request body is not valid JSON for this endpoint: "+err.Error()).Wrap(err)
	}
	return nil
}

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,200}$`)

// parentRef reads the caller's audit position. It is recorded as data, never
// trusted for authorization.
func parentRef(r *http.Request) map[string]any {
	session := r.Header.Get(HeaderParentSession)
	callID, err := strconv.ParseUint(r.Header.Get(HeaderParentCallID), 10, 64)
	if err != nil || !sessionPattern.MatchString(session) {
		return nil
	}
	return map[string]any{"session": session, "call_id": callID}
}

// WantsStream reports whether the client asked for an event stream.
func WantsStream(r *http.Request) bool {
	for _, accept := range r.Header.Values("Accept") {
		if strings.Contains(strings.ToLower(accept), "text/event-stream") {
			return true
		}
	}
	return false
}

// Write sends an Envelope from code that runs outside Handle, such as
// authentication middleware or a liveness probe. It writes no audit record, so
// use it only where a record per request is not wanted.
func Write(w http.ResponseWriter, env Envelope) {
	body, err := json.Marshal(env)
	if err != nil {
		env = Failure(NewProblem(http.StatusInternalServerError, CodeInternal, "envelope is not JSON-serializable"))
		body, _ = json.Marshal(env)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(env.Status())
	_, _ = w.Write(append(body, '\n'))
}

type requestKey struct{}
type emitKey struct{}

// Request returns the HTTP request being served, for handlers that need a
// path value, a query parameter, or a header. It is nil outside Handle.
func Request(ctx context.Context) *http.Request {
	r, _ := ctx.Value(requestKey{}).(*http.Request)
	return r
}

// Emit publishes one progress event to the client of the current request. It
// does nothing when the client did not ask for a stream, so the same code
// serves both. An error means the client is gone and work may stop.
func Emit(ctx context.Context, eventType string, data any) error {
	sw, _ := ctx.Value(emitKey{}).(*sseWriter)
	if sw == nil {
		return nil
	}
	return sw.emit(eventType, data)
}

type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	rc      *http.ResponseController
	options Server
	started bool
	err     error
	seq     uint64
	digest  hash.Hash
	stop    chan struct{}
	done    sync.WaitGroup
}

func newSSEWriter(w http.ResponseWriter, o Server) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w), options: o, digest: sha256.New(), stop: make(chan struct{})}
}

// writeLocked sends one frame and flushes it. The first frame commits the
// response to an event stream.
func (s *sseWriter) writeLocked(frame string) error {
	if s.err != nil {
		return s.err
	}
	if !s.started {
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	// Not every ResponseWriter supports deadlines; a stream still works without.
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.options.WriteTimeout))
	if _, err := io.WriteString(s.w, frame); err != nil {
		s.err = err
		return err
	}
	if err := s.rc.Flush(); err != nil {
		s.err = err
	}
	return s.err
}

func (s *sseWriter) emit(eventType string, data any) error {
	if eventType == "" || eventType == EventResult || strings.ContainsAny(eventType, "\r\n") {
		return fmt.Errorf("invalid event type %q", eventType)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("event data is not JSON-serializable: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	payload, _ := json.Marshal(Event{Type: eventType, Seq: s.seq + 1, Time: time.Now().UTC(), Data: raw})
	if err := s.writeLocked("event: " + eventType + "\ndata: " + string(payload) + "\n\n"); err != nil {
		return err
	}
	s.seq++
	s.digest.Write(payload)
	s.digest.Write([]byte{'\n'})
	return nil
}

func (s *sseWriter) keepAlive() {
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		ticker := time.NewTicker(s.options.KeepAlive)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				s.mu.Lock()
				_ = s.writeLocked(": keepalive\n\n")
				s.mu.Unlock()
			}
		}
	}()
}

func (s *sseWriter) stopKeepAlive() {
	close(s.stop)
	s.done.Wait()
}

func (s *sseWriter) summary() (uint64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seq == 0 {
		return 0, ""
	}
	return s.seq, fmt.Sprintf("%x", s.digest.Sum(nil))
}

// finish sends the terminal event. It reports false when the stream never
// started, and the caller then answers with a plain JSON Envelope.
func (s *sseWriter) finish(env Envelope) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return false
	}
	body, err := json.Marshal(env)
	if err != nil {
		body, _ = json.Marshal(Failure(NewProblem(http.StatusInternalServerError, CodeInternal, "envelope is not JSON-serializable")))
	}
	_ = s.writeLocked("event: " + EventResult + "\ndata: " + string(body) + "\n\n")
	_ = s.rc.SetWriteDeadline(time.Time{})
	return true
}
