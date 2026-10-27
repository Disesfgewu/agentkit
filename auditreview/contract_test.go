// Package auditreview contains positive regression contracts for the 13 defects
// reproduced at revision 000ac62. These tests run in normal CI.
package auditreview

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agent"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agentlog"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/graph"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/gsd"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type probeModel struct{ calls int }

func (m *probeModel) Chat(context.Context, model.Request) (model.Response, error) {
	m.calls++
	return model.Response{Text: "ok"}, nil
}

type panicTool struct{}

func (panicTool) Name() string                                      { return "boom" }
func (panicTool) Run(context.Context, json.RawMessage) (any, error) { panic("intentional-tool-panic") }
func read(t *testing.T, path string) string {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func recorder(t *testing.T) (*agentlog.Recorder, context.Context) {
	t.Helper()
	r, e := agentlog.New("review", t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, agentlog.WithRecorder(context.Background(), r)
}
func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	fn()
}

func TestTemplateRejectsBrokenAuditBeforeModel(t *testing.T) {
	m := &probeModel{}
	a := &agent.Template{Name: "broken", LogDir: t.TempDir(), Model: m}
	r, e := a.Runtime()
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Logger.Close(); e != nil {
		t.Fatal(e)
	}
	_, e = a.Chat(context.Background(), []model.Message{{Role: "user", Content: "test"}}, "")
	if e == nil || m.calls != 0 {
		t.Fatal("model ran despite unavailable audit", e, m.calls)
	}
	if n, e := agentlog.Verify(r.Path()); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
func TestBackgroundOperationsAutomaticallyCreateAudit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := &agent.Template{Name: "direct", LogDir: filepath.Join(dir, "log")}
	_ = a.BuildMessages(context.Background(), nil, "instruction")
	if a.Logger == nil {
		t.Fatal("Template did not create its logger")
	}
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	g := graph.New()
	_ = g.AddNode("n", func(context.Context, graph.State) (graph.State, error) { return graph.State{"done": true}, nil })
	g.Edge(graph.Start, "n")
	g.Edge("n", graph.End)
	if _, e := g.Run(context.Background(), nil); e != nil {
		t.Fatal(e)
	}
	if e := gsd.WriteJSONL(filepath.Join(dir, "data.jsonl"), []int{1}); e != nil {
		t.Fatal(e)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "log", "*.log"))
	if len(files) < 6 {
		t.Fatal("standalone operations missing", len(files))
	}
	for _, p := range files {
		if _, e := agentlog.Verify(p); e != nil {
			t.Fatal(p, e)
		}
	}
}
func TestInputIsImmutableSnapshot(t *testing.T) {
	r, ctx := recorder(t)
	input := map[string]any{"pressure": 1}
	_, s := agentlog.Start(ctx, "mutates", input)
	input["pressure"] = 9
	if e := s.End(nil, nil, nil); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(read(t, r.Path), `input:{"pressure":1}`) {
		t.Fatal("lost input snapshot")
	}
}
func TestToolPanicHasChildAndParentEvidence(t *testing.T) {
	a := &agent.Template{Name: "panic", LogDir: t.TempDir(), Tools: map[string]agent.Tool{"boom": panicTool{}}}
	mustPanic(t, func() { _, _ = a.RunTool(context.Background(), "boom", nil) })
	path := a.Logger.Path
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	s := read(t, path)
	if !strings.Contains(s, "func:agent.Tool.boom") || strings.Count(s, "intentional-tool-panic") != 2 {
		t.Fatal("missing panic evidence")
	}
	if _, e := agentlog.Verify(path); e != nil {
		t.Fatal(e)
	}
}
func TestGraphPanicHasNodeEvidence(t *testing.T) {
	r, ctx := recorder(t)
	g := graph.NewContext(ctx)
	_ = g.AddNodeContext(ctx, "boom", func(context.Context, graph.State) (graph.State, error) { panic("intentional-node-panic") })
	g.EdgeContext(ctx, graph.Start, "boom")
	g.EdgeContext(ctx, "boom", graph.End)
	mustPanic(t, func() { _, _ = g.Run(ctx, nil) })
	if e := r.Close(); e != nil {
		t.Fatal(e)
	}
	s := read(t, r.Path)
	if !strings.Contains(s, "func:graph.Node.boom") || !strings.Contains(s, "intentional-node-panic") {
		t.Fatal("missing node panic evidence")
	}
	if _, e := agentlog.Verify(r.Path); e != nil {
		t.Fatal(e)
	}
}
func TestCrossSessionResetsParent(t *testing.T) {
	a := &agent.Template{Name: "a", LogDir: t.TempDir()}
	b := &agent.Template{Name: "b", LogDir: t.TempDir()}
	ra, e := a.Runtime()
	if e != nil {
		t.Fatal(e)
	}
	defer ra.Close()
	_, _ = ra.Execute(context.Background(), "parent", nil, func(ctx context.Context) (any, error) { return b.GetInformation(ctx, "q", 1) })
	path := b.Logger.Path
	if e = b.Close(); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(read(t, path), ", call_id:1, parent_call_id:0,") {
		t.Fatal("cross-session parent leaked")
	}
	if _, e := agentlog.Verify(path); e != nil {
		t.Fatal(e)
	}
}
func TestClosedTemplateCannotReopenOrCloseAnotherSession(t *testing.T) {
	a := &agent.Template{Name: "lifecycle", LogDir: t.TempDir()}
	old, e := a.Runtime()
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Runtime(); !errors.Is(e, agentlog.ErrClosed) {
		t.Fatal("closed Template reopened", e)
	}
	other, e := agent.NewRuntime("other", t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	if e = old.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = other.Execute(context.Background(), "still-live", nil, func(context.Context) (any, error) { return "ok", nil }); e != nil {
		t.Fatal(e)
	}
}
func TestBorrowedHandleCannotCloseOwner(t *testing.T) {
	r, e := agent.NewRuntime("owner", t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	borrowed, e := agent.RuntimeFromContext(r.Context(context.Background()))
	if e != nil {
		t.Fatal(e)
	}
	if e = borrowed.Close(); e == nil {
		t.Fatal("borrower closed owner")
	}
	if _, e = r.Execute(context.Background(), "owner-operation", nil, func(context.Context) (any, error) { return nil, nil }); e != nil {
		t.Fatal(e)
	}
}
func TestGraphRecordsSensorInputValues(t *testing.T) {
	r, ctx := recorder(t)
	g := graph.NewContext(ctx)
	_ = g.AddNodeContext(ctx, "n", func(context.Context, graph.State) (graph.State, error) { return graph.State{"done": true}, nil })
	g.EdgeContext(ctx, graph.Start, "n")
	g.EdgeContext(ctx, "n", graph.End)
	if _, e := g.Run(ctx, graph.State{"pressure": 42.125}); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(read(t, r.Path), "42.125") {
		t.Fatal("sensor input omitted")
	}
}
func TestErrorPreservesPartialOutput(t *testing.T) {
	r, ctx := recorder(t)
	_, s := agentlog.Start(ctx, "partial", nil)
	_ = s.End(map[string]any{"accepted_id": "command-123"}, nil, errors.New("write timeout"))
	data := read(t, r.Path)
	if !strings.Contains(data, "command-123") || !strings.Contains(data, "write timeout") {
		t.Fatal("lost output/error")
	}
}
func TestTruncatedTailFailsStrictVerification(t *testing.T) {
	r, _ := recorder(t)
	_ = r.Record("one", nil, nil, nil, time.Millisecond, nil)
	_ = r.Record("two", nil, nil, nil, time.Millisecond, nil)
	if e := r.Close(); e != nil {
		t.Fatal(e)
	}
	first := strings.SplitN(read(t, r.Path), "\n", 2)[0] + "\n"
	if e := os.WriteFile(r.Path, []byte(first), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := agentlog.Verify(r.Path); e == nil {
		t.Fatal("truncated log accepted")
	}
}
func TestURLSecretsAreRedactedInLogAndJournal(t *testing.T) {
	r, _ := recorder(t)
	_ = r.Record("HTTP.GET", map[string]any{"url": "https://example.invalid/?api_key=CANARY_NOT_A_REAL_KEY"}, nil, nil, 0, nil)
	for _, path := range []string{r.Path, r.Path + ".journal"} {
		if strings.Contains(read(t, path), "CANARY_NOT_A_REAL_KEY") {
			t.Fatal("secret leaked", path)
		}
	}
}

type blockingModel struct{ entered, release chan struct{} }

func (m blockingModel) Chat(context.Context, model.Request) (model.Response, error) {
	close(m.entered)
	<-m.release
	return model.Response{Text: "ok"}, nil
}
func TestCloseWaitsForInFlightChat(t *testing.T) {
	m := blockingModel{make(chan struct{}), make(chan struct{})}
	a := &agent.Template{Name: "inflight", LogDir: t.TempDir(), Model: m}
	done := make(chan error, 1)
	go func() {
		_, e := a.Chat(context.Background(), []model.Message{{Role: "user", Content: "test"}}, "")
		done <- e
	}()
	<-m.entered
	path := a.Logger.Path
	closing := make(chan error, 1)
	go func() { closing <- a.Close() }()
	select {
	case e := <-closing:
		t.Fatal("closed active operation", e)
	case <-time.After(20 * time.Millisecond):
	}
	close(m.release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if e := <-closing; e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(read(t, path), "func:agent.Template.Chat(") {
		t.Fatal("missing completion")
	}
	if _, e := agentlog.Verify(path); e != nil {
		t.Fatal(e)
	}
}
