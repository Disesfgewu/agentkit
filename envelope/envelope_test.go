package envelope_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Disesfgewu/agentkit/agentlog"
	"github.com/Disesfgewu/agentkit/envelope"
)

type question struct {
	Text string `json:"text"`
}
type answer struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

var errNotFound = errors.New("no such thing")

type fixture struct {
	t                *testing.T
	serverLog        *agentlog.Recorder
	clientLog        *agentlog.Recorder
	ctx              context.Context
	client           *envelope.Client
	mux              *http.ServeMux
	server           *envelope.Server
	calls            atomic.Int64
	handlerCancelled chan struct{}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, mux: http.NewServeMux(), handlerCancelled: make(chan struct{}, 1)}
	var err error
	if f.serverLog, err = agentlog.New("server", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if f.clientLog, err = agentlog.New("client", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	f.ctx = agentlog.WithRecorder(context.Background(), f.clientLog)
	f.server = &envelope.Server{Recorder: f.serverLog, KeepAlive: 20 * time.Millisecond, Map: func(err error) *envelope.Problem {
		if errors.Is(err, errNotFound) {
			return envelope.NewProblem(http.StatusNotFound, "thing_unknown", err.Error())
		}
		return nil
	}}
	f.mux.Handle("POST /echo", envelope.Handle(f.server, "test.echo", func(ctx context.Context, q question) (answer, error) {
		f.calls.Add(1)
		return answer{Text: q.Text, Count: len(q.Text)}, nil
	}))
	f.mux.Handle("POST /fail", envelope.Handle(f.server, "test.fail", func(ctx context.Context, q question) (answer, error) {
		switch q.Text {
		case "mapped":
			return answer{}, fmt.Errorf("lookup: %w", errNotFound)
		case "problem":
			p := envelope.NewProblem(http.StatusUnprocessableEntity, "invalid_question", "text must not be empty")
			p.Errors = []envelope.FieldError{{Field: "text", Message: "required"}}
			return answer{}, p
		case "panic":
			panic("intentional-handler-panic")
		case "deadline":
			return answer{}, context.DeadlineExceeded
		case "typed-nil":
			var p *envelope.Problem
			return answer{}, p // a classic mistake: a nil pointer in a non-nil error
		}
		return answer{}, errors.New("unexpected failure")
	}))
	f.mux.Handle("POST /steps", envelope.Handle(f.server, "test.steps", func(ctx context.Context, q question) (answer, error) {
		for i := 1; i <= 3; i++ {
			if err := envelope.Emit(ctx, "step.finished", map[string]any{"step": i}); err != nil {
				return answer{}, err
			}
		}
		if q.Text == "fail-late" {
			return answer{}, errNotFound
		}
		return answer{Text: q.Text, Count: 3}, nil
	}))
	f.mux.Handle("POST /slow", envelope.Handle(f.server, "test.slow", func(ctx context.Context, q question) (answer, error) {
		time.Sleep(90 * time.Millisecond) // several keep-alive intervals with no event
		return answer{Text: "late", Count: 0}, envelope.Emit(ctx, "tick", nil)
	}))
	f.mux.Handle("POST /forever", envelope.Handle(f.server, "test.forever", func(ctx context.Context, q question) (answer, error) {
		for i := 0; ; i++ {
			if err := envelope.Emit(ctx, "tick", i); err != nil {
				f.handlerCancelled <- struct{}{}
				return answer{}, err
			}
			select {
			case <-ctx.Done():
				f.handlerCancelled <- struct{}{}
				return answer{}, ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	ts := httptest.NewServer(f.mux)
	t.Cleanup(ts.Close)
	f.client = &envelope.Client{BaseURL: ts.URL}
	return f
}

// logs closes both sessions, verifies both hash chains, and returns their text.
func (f *fixture) logs() (server, client string) {
	f.t.Helper()
	for _, r := range []*agentlog.Recorder{f.serverLog, f.clientLog} {
		if err := r.Close(); err != nil {
			f.t.Fatal(err)
		}
		if _, err := agentlog.Verify(r.Path); err != nil {
			f.t.Fatalf("%s: %v", r.AgentName, err)
		}
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			f.t.Fatal(err)
		}
		return string(b)
	}
	return read(f.serverLog.Path), read(f.clientLog.Path)
}

func problemOf(t *testing.T, err error) *envelope.Problem {
	t.Helper()
	var p *envelope.Problem
	if !errors.As(err, &p) {
		t.Fatalf("error is not a Problem: %v", err)
	}
	return p
}

func TestCallUnwrapsDataAndBothSidesCrossReference(t *testing.T) {
	f := newFixture(t)
	got, meta, err := envelope.Call[answer](f.ctx, f.client, "POST", "/echo", question{Text: "hello"})
	if err != nil || got != (answer{Text: "hello", Count: 5}) {
		t.Fatal(got, err)
	}
	if meta.Audit == nil || meta.Audit.Session != f.serverLog.SessionID || meta.Audit.Hash == "" || meta.Audit.Seq != 1 {
		t.Fatalf("missing audit receipt: %+v", meta.Audit)
	}
	server, client := f.logs()
	// The receipt names the server's completion record exactly.
	if !strings.Contains(server, ", hash:"+meta.Audit.Hash) || !strings.Contains(server, "func:test.echo(") {
		t.Fatal("receipt does not match the server log")
	}
	// The server recorded who called it, and the client recorded whom it called.
	if !strings.Contains(server, `"parent":{"call_id":1,"session":"`+f.clientLog.SessionID+`"}`) {
		t.Fatal("server log lacks the caller reference:", server)
	}
	if !strings.Contains(client, `"server_audit":{"call_id":1,"hash":"`+meta.Audit.Hash+`"`) {
		t.Fatal("client log lacks the server reference:", client)
	}
}

func TestEveryFailureIsAProblem(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		text, code string
		status     int
	}{
		{"mapped", "thing_unknown", http.StatusNotFound},
		{"problem", "invalid_question", http.StatusUnprocessableEntity},
		{"panic", envelope.CodeInternalPanic, http.StatusInternalServerError},
		{"deadline", envelope.CodeTimeout, http.StatusGatewayTimeout},
		{"other", envelope.CodeInternal, http.StatusInternalServerError},
		{"typed-nil", envelope.CodeInternal, http.StatusInternalServerError},
	}
	for _, c := range cases {
		_, meta, err := envelope.Call[answer](f.ctx, f.client, "POST", "/fail", question{Text: c.text})
		if _, bare := err.(*envelope.Problem); !bare {
			t.Errorf("%s: Call must return the Problem itself, got %T", c.text, err)
		}
		p := problemOf(t, err)
		if p.Code != c.code || p.Status != c.status {
			t.Errorf("%s: got %s/%d, want %s/%d", c.text, p.Code, p.Status, c.code, c.status)
		}
		if meta.Audit == nil || meta.Audit.Hash == "" {
			t.Errorf("%s: failure has no audit receipt", c.text)
		}
		if c.text == "problem" && (len(p.Errors) != 1 || p.Errors[0].Field != "text") {
			t.Errorf("field errors lost: %+v", p.Errors)
		}
		if c.text == "panic" && strings.Contains(p.Detail, "goroutine") {
			t.Error("stack trace leaked to the client")
		}
	}
	server, _ := f.logs()
	// The audit record, unlike the client, keeps the stack of the panic.
	if !strings.Contains(server, "intentional-handler-panic") || !strings.Contains(server, "envelope_test.go") {
		t.Fatal("panic evidence missing from the audit log")
	}
}

func TestMalformedRequestsAreProblems(t *testing.T) {
	f := newFixture(t)
	f.server.MaxBodyBytes = 64 // read when the handler below is built
	f.mux.Handle("POST /small", envelope.Handle(f.server, "test.small", func(ctx context.Context, q question) (answer, error) {
		return answer{}, nil
	}))
	post := func(path, body string) envelope.Envelope {
		resp, err := http.Post(f.client.BaseURL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var env envelope.Envelope
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatal(err)
		}
		if env.Status() != resp.StatusCode {
			t.Errorf("HTTP status %d disagrees with envelope status %d", resp.StatusCode, env.Status())
		}
		return env
	}
	if env := post("/echo", "{not json"); env.OK || env.Code != envelope.CodeInvalidJSON || env.Error.Status != http.StatusBadRequest || env.Data != nil && string(env.Data) != "null" {
		t.Errorf("invalid JSON: %+v", env)
	}
	if env := post("/small", `{"text":"`+strings.Repeat("x", 200)+`"}`); env.Code != envelope.CodeBodyTooLarge || env.Error.Status != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %+v", env)
	}
	if f.calls.Load() != 0 {
		t.Error("handler ran on a malformed request")
	}
}

func TestUnavailableAuditFailsClosed(t *testing.T) {
	f := newFixture(t)
	if err := f.serverLog.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err := envelope.Call[answer](f.ctx, f.client, "POST", "/echo", question{Text: "x"})
	if p := problemOf(t, err); p.Code != envelope.CodeAuditUnavailable || p.Status != http.StatusServiceUnavailable {
		t.Fatalf("got %+v", p)
	}
	if f.calls.Load() != 0 {
		t.Fatal("handler ran although the audit session was unavailable")
	}
	// The same rule on the client: no audit session, no request.
	closed, _ := agentlog.New("closed", t.TempDir())
	_ = closed.Close()
	_, _, err = envelope.Call[answer](agentlog.WithRecorder(context.Background(), closed), f.client, "POST", "/echo", question{Text: "x"})
	if p := problemOf(t, err); p.Code != envelope.CodeAuditUnavailable {
		t.Fatalf("got %+v", p)
	}
}

func TestStreamDeliversEventsThenResultWithMatchingDigests(t *testing.T) {
	f := newFixture(t)
	var seen []uint64
	got, meta, err := envelope.Collect[answer](envelope.Stream(f.ctx, f.client, "POST", "/steps", question{Text: "go"}), func(e envelope.Event) error {
		if e.Type != "step.finished" {
			t.Errorf("unexpected event type %q", e.Type)
		}
		seen = append(seen, e.Seq)
		return nil
	})
	if err != nil || got != (answer{Text: "go", Count: 3}) || fmt.Sprint(seen) != "[1 2 3]" {
		t.Fatal(got, seen, err)
	}
	if meta.Events != 3 || meta.EventsSHA256 == "" || meta.Audit == nil || meta.Audit.Hash == "" {
		t.Fatalf("stream metadata incomplete: %+v", meta)
	}
	server, client := f.logs()
	// One audit record per stream on each side, both holding the same digest.
	digest := `"events_sha256":"` + meta.EventsSHA256 + `"`
	if strings.Count(server, "phase:END") != 1 || strings.Count(client, "phase:END") != 1 {
		t.Fatal("a stream must be exactly one audit call per side")
	}
	if !strings.Contains(server, digest) || !strings.Contains(client, digest) || !strings.Contains(client, `"terminal":"completed"`) {
		t.Fatal("digests are not recorded on both sides")
	}
}

func TestSameHandlerServesPlainCalls(t *testing.T) {
	f := newFixture(t)
	got, meta, err := envelope.Call[answer](f.ctx, f.client, "POST", "/steps", question{Text: "go"})
	if err != nil || got != (answer{Text: "go", Count: 3}) || meta.Events != 0 {
		t.Fatal(got, meta, err)
	}
}

func TestStreamFailureBeforeAnyEventKeepsItsHTTPStatus(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest("POST", f.client.BaseURL+"/fail", strings.NewReader(`{"text":"mapped"}`))
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatal(resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	_, _, err = envelope.Collect[answer](envelope.Stream(f.ctx, f.client, "POST", "/fail", question{Text: "mapped"}), nil)
	if p := problemOf(t, err); p.Code != "thing_unknown" || p.Status != http.StatusNotFound {
		t.Fatalf("got %+v", p)
	}
}

func TestStreamFailureAfterEventsArrivesAsResult(t *testing.T) {
	f := newFixture(t)
	events := 0
	_, meta, err := envelope.Collect[answer](envelope.Stream(f.ctx, f.client, "POST", "/steps", question{Text: "fail-late"}), func(envelope.Event) error {
		events++
		return nil
	})
	if p := problemOf(t, err); p.Code != "thing_unknown" || events != 3 || meta.Audit == nil {
		t.Fatal(p, events, meta)
	}
}

func TestKeepAliveStartsAnIdleStream(t *testing.T) {
	f := newFixture(t)
	got, meta, err := envelope.Collect[answer](envelope.Stream(f.ctx, f.client, "POST", "/slow", nil), nil)
	if err != nil || got.Text != "late" || meta.Events != 1 {
		t.Fatal(got, meta, err)
	}
}

func TestLeavingTheLoopCancelsTheHandlerAndClosesBothSpans(t *testing.T) {
	f := newFixture(t)
	for event, err := range envelope.Stream(f.ctx, f.client, "POST", "/forever", nil) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Seq == 2 {
			break
		}
	}
	select {
	case <-f.handlerCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler kept running after the client left")
	}
	done := make(chan struct{})
	var server, client string
	go func() { server, client = f.logs(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an audit span was left open, so Close blocked")
	}
	if !strings.Contains(client, `"terminal":"canceled"`) || !strings.Contains(server, "func:test.forever(") {
		t.Fatal("cancellation not recorded")
	}
}

func TestBrokenStreamsAreFailures(t *testing.T) {
	frames := map[string]string{
		"/truncated": "event: tick\ndata: {\"type\":\"tick\",\"seq\":1}\n\n",
		"/gap":       "event: tick\ndata: {\"type\":\"tick\",\"seq\":2}\n\n",
		"/miscount":  "event: tick\ndata: {\"type\":\"tick\",\"seq\":1}\n\nevent: result\ndata: {\"version\":\"1\",\"ok\":true,\"code\":\"ok\",\"data\":{},\"meta\":{\"events\":2,\"events_sha256\":\"00\"}}\n\n",
	}
	mux := http.NewServeMux()
	for path, body := range frames {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, body)
		})
	}
	mux.HandleFunc("/html", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	})
	mux.HandleFunc("/legacy", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"unknown model"}`)
	})
	mux.HandleFunc("/bare", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"text":"not wrapped"}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	rec, _ := agentlog.New("client", t.TempDir())
	defer rec.Close()
	ctx := agentlog.WithRecorder(context.Background(), rec)
	c := &envelope.Client{BaseURL: ts.URL}
	for path, code := range map[string]string{"/truncated": envelope.CodeStreamTruncated, "/gap": envelope.CodeStreamGap, "/miscount": envelope.CodeStreamCorrupt} {
		_, _, err := envelope.Collect[answer](envelope.Stream(ctx, c, "POST", path, nil), nil)
		if p := problemOf(t, err); p.Code != code {
			t.Errorf("%s: got %s, want %s", path, p.Code, code)
		}
	}
	for path, status := range map[string]int{"/html": http.StatusBadGateway, "/legacy": http.StatusNotFound, "/bare": http.StatusBadGateway} {
		_, _, err := envelope.Call[answer](ctx, c, "POST", path, nil)
		p := problemOf(t, err)
		if p.Code != envelope.CodeBadEnvelope || p.Status != status {
			t.Errorf("%s: got %s/%d", path, p.Code, p.Status)
		}
		if path == "/legacy" && !strings.Contains(p.Detail, "unknown model") {
			t.Errorf("legacy error text lost: %q", p.Detail)
		}
	}
	// An oversized plain reply is rejected, not cut to a prefix that may parse.
	small := &envelope.Client{BaseURL: ts.URL, MaxBodyBytes: 8}
	for _, err := range []error{
		func() error { _, _, e := envelope.Call[answer](ctx, small, "POST", "/bare", nil); return e }(),
		func() error {
			_, _, e := envelope.Collect[answer](envelope.Stream(ctx, small, "POST", "/bare", nil), nil)
			return e
		}(),
	} {
		if p := problemOf(t, err); p.Code != envelope.CodeBadEnvelope || !strings.Contains(p.Detail, "exceeds 8 bytes") {
			t.Errorf("oversized reply: %+v", p)
		}
	}
	// No listener at all is still a Problem.
	_, _, err := envelope.Call[answer](ctx, &envelope.Client{BaseURL: "http://127.0.0.1:1"}, "POST", "/x", nil)
	if p := problemOf(t, err); p.Code != envelope.CodeTransport || !p.Retryable {
		t.Errorf("transport failure: %+v", p)
	}
}

func TestPulledStreamReleasesItsSpanOnStop(t *testing.T) {
	f := newFixture(t)
	next, stop := iter.Pull2(envelope.Stream(f.ctx, f.client, "POST", "/forever", nil))
	if _, err, ok := next(); err != nil || !ok {
		t.Fatal(err, ok)
	}
	stop()
	done := make(chan struct{})
	go func() { f.logs(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked after stop")
	}
}

func TestWithoutARecorderEachRequestStillAuditsItself(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ts := httptest.NewServer(envelope.Handle(nil, "test.standalone", func(ctx context.Context, q question) (answer, error) {
		return answer{Text: q.Text}, nil
	}))
	defer ts.Close()
	_, meta, err := envelope.Call[answer](context.Background(), &envelope.Client{BaseURL: ts.URL}, "POST", "/", question{Text: "x"})
	if err != nil || meta.Audit == nil {
		t.Fatal(meta, err)
	}
	files, _ := os.ReadDir("log")
	logs := 0
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".log") {
			logs++
			if _, err := agentlog.Verify("log/" + file.Name()); err != nil {
				t.Fatal(file.Name(), err)
			}
		}
	}
	if logs != 2 {
		t.Fatalf("want one automatic session per side, got %d", logs)
	}
}
