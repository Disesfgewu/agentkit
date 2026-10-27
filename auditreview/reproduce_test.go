//go:build auditreview

// Characterization tests: PASS means the documented defect was reproduced,
// NOT that the audit system is correct. Excluded from ordinary CI until fixed.
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

func TestReproduceTemplateSuccessDespiteBrokenAudit(t *testing.T) {
	m := &probeModel{}
	a := &agent.Template{Name: "broken", LogDir: t.TempDir(), Model: m}
	r, e := a.Runtime()
	if e != nil {
		t.Fatal(e)
	}
	// Simulate a writer becoming unavailable after initialization.
	if e = a.Logger.Close(); e != nil {
		t.Fatal(e)
	}
	out, e := a.Chat(context.Background(), []model.Message{{Role: "user", Content: "test"}}, "")
	if e != nil || out.Text != "ok" || m.calls != 1 || a.Logger.Err() == nil {
		t.Fatal("expected defect not reproduced")
	}
	n, e := agentlog.Verify(r.Path())
	if e != nil || n != 0 {
		t.Fatal(n, e)
	}
	t.Log("REPRODUCED: model executed, Template returned success, zero audit records")
}
func TestReproduceBackgroundContextSilentlyDisablesAudit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := &agent.Template{Name: "direct", LogDir: filepath.Join(dir, "log")}
	_ = a.BuildMessages(context.Background(), nil, "instruction")
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
	if a.Logger != nil {
		t.Fatal("expected uninitialized logger")
	}
	files, e := filepath.Glob(filepath.Join(dir, "log", "*"))
	if e != nil || len(files) != 0 {
		t.Fatal(files, e)
	}
	t.Log("REPRODUCED: Template.BuildMessages, Graph and GSD side effect run without audit")
}
func TestReproduceInputSnapshotMutation(t *testing.T) {
	r, ctx := recorder(t)
	input := map[string]any{"pressure": 1}
	_, span := agentlog.Start(ctx, "mutates", input)
	input["pressure"] = 9
	span.End(nil, nil, nil)
	data := read(t, r.Path)
	if !strings.Contains(data, `input:{"pressure":9}`) || strings.Contains(data, `input:{"pressure":1}`) {
		t.Fatal("expected defect not reproduced")
	}
	t.Log("REPRODUCED: log input is the post-call mutated value")
}
func TestReproduceToolPanicLosesChildRecord(t *testing.T) {
	a := &agent.Template{Name: "panic", LogDir: t.TempDir(), Tools: map[string]agent.Tool{"boom": panicTool{}}}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic")
			}
		}()
		_, _ = a.RunTool(context.Background(), "boom", nil)
	}()
	data := read(t, a.Logger.Path)
	defer a.Close()
	if strings.Contains(data, "func:agent.Tool.boom") || strings.Contains(data, "intentional-tool-panic") {
		t.Fatal("expected defect not reproduced")
	}
	if !strings.Contains(data, "func:agent.Template.RunTool") {
		t.Fatal("missing parent")
	}
	t.Log("REPRODUCED: tool panic has no child completion; parent has no panic/error")
}
func TestReproduceGraphPanicLosesNodeRecord(t *testing.T) {
	r, ctx := recorder(t)
	g := graph.NewContext(ctx)
	_ = g.AddNodeContext(ctx, "boom", func(context.Context, graph.State) (graph.State, error) { panic("intentional-node-panic") })
	g.EdgeContext(ctx, graph.Start, "boom")
	g.EdgeContext(ctx, "boom", graph.End)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic")
			}
		}()
		_, _ = g.Run(ctx, nil)
	}()
	data := read(t, r.Path)
	if strings.Contains(data, "func:graph.Node.boom") || strings.Contains(data, "intentional-node-panic") {
		t.Fatal("expected defect not reproduced")
	}
	t.Log("REPRODUCED: graph node panic has no node completion/error evidence")
}
func TestReproduceCrossSessionParentSelfReference(t *testing.T) {
	a := &agent.Template{Name: "a", LogDir: t.TempDir()}
	b := &agent.Template{Name: "b", LogDir: t.TempDir()}
	ra, e := a.Runtime()
	if e != nil {
		t.Fatal(e)
	}
	defer ra.Close()
	defer b.Close()
	_, _ = ra.Execute(context.Background(), "parent", nil, func(ctx context.Context) (any, error) { return b.GetInformation(ctx, "q", 1) })
	data := read(t, b.Logger.Path)
	if !strings.Contains(data, ", call_id:1, parent_call_id:1,") {
		t.Fatal("expected defect not reproduced")
	}
	if _, e := agentlog.Verify(b.Logger.Path); e != nil {
		t.Fatal(e)
	}
	t.Log("REPRODUCED: switching Template creates self-parent; hash verifier accepts it")
}
func TestReproduceStaleRuntimeClosesNewSession(t *testing.T) {
	a := &agent.Template{Name: "lifecycle", LogDir: t.TempDir()}
	old, e := a.Runtime()
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	a.Name = "new-session" // Isolate lifecycle defect from wall-clock filename collisions.
	current, e := a.Runtime()
	if e != nil {
		t.Fatal(e)
	}
	if current.Path() == old.Path() {
		t.Fatal("expected new session")
	}
	if e = old.Close(); e != nil {
		t.Fatal(e)
	}
	_, e = current.Execute(context.Background(), "new-operation", nil, func(context.Context) (any, error) { return "ok", nil })
	if e == nil {
		t.Fatal("expected current session to have been closed")
	}
	t.Log("REPRODUCED: stale Runtime.Close closes the owner's NEW logger")
}
func TestReproduceBorrowedRuntimeClosesOwner(t *testing.T) {
	a := &agent.Template{Name: "owner", LogDir: t.TempDir()}
	r, e := a.Runtime()
	if e != nil {
		t.Fatal(e)
	}
	borrowed, e := agent.RuntimeFromContext(r.Context(context.Background()))
	if e != nil {
		t.Fatal(e)
	}
	if e = borrowed.Close(); e != nil {
		t.Fatal(e)
	}
	_, e = r.Execute(context.Background(), "owner-operation", nil, func(context.Context) (any, error) { return nil, nil })
	if e == nil {
		t.Fatal("expected closed owner recorder")
	}
	t.Log("REPRODUCED: borrowed context handle can close owner recorder")
}
func TestReproduceGraphInputValuesOmitted(t *testing.T) {
	r, ctx := recorder(t)
	g := graph.NewContext(ctx)
	_ = g.AddNodeContext(ctx, "n", func(context.Context, graph.State) (graph.State, error) { return graph.State{"done": true}, nil })
	g.EdgeContext(ctx, graph.Start, "n")
	g.EdgeContext(ctx, "n", graph.End)
	if _, e := g.Run(ctx, graph.State{"pressure": 42.125}); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(read(t, r.Path), "42.125") {
		t.Fatal("expected input value to be omitted")
	}
	t.Log("REPRODUCED: full graph audit contains field name but not input sensor value")
}
func TestReproduceErrorDropsPartialOutput(t *testing.T) {
	r, ctx := recorder(t)
	_, span := agentlog.Start(ctx, "partial", nil)
	span.End(map[string]any{"accepted_id": "command-123"}, nil, errors.New("write timeout"))
	data := read(t, r.Path)
	if strings.Contains(data, "command-123") || !strings.Contains(data, "write timeout") {
		t.Fatal("expected defect not reproduced")
	}
	t.Log("REPRODUCED: partial action output discarded when error is present")
}
func TestReproduceTruncatedTailPassesVerify(t *testing.T) {
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
	n, e := agentlog.Verify(r.Path)
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	t.Log("REPRODUCED: removed trailing operation passes chain verifier")
}
func TestReproduceSecretInURLIsNotRedacted(t *testing.T) {
	r, _ := recorder(t)
	_ = r.Record("HTTP.GET", map[string]any{"url": "https://example.invalid/?api_key=CANARY_NOT_A_REAL_KEY"}, nil, nil, 0, nil)
	if !strings.Contains(read(t, r.Path), "CANARY_NOT_A_REAL_KEY") {
		t.Fatal("expected defect not reproduced")
	}
	t.Log("REPRODUCED: synthetic secret in URL survives key-based redaction")
}

type blockingModel struct{ entered, release chan struct{} }

func (m blockingModel) Chat(context.Context, model.Request) (model.Response, error) {
	close(m.entered)
	<-m.release
	return model.Response{Text: "ok"}, nil
}
func TestReproduceCloseDoesNotWaitForInFlightChat(t *testing.T) {
	m := blockingModel{make(chan struct{}), make(chan struct{})}
	a := &agent.Template{Name: "inflight", LogDir: t.TempDir(), Model: m}
	done := make(chan error, 1)
	go func() {
		_, e := a.Chat(context.Background(), []model.Message{{Role: "user", Content: "test"}}, "")
		done <- e
	}()
	<-m.entered
	r := a.Logger
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	close(m.release)
	if e := <-done; e != nil {
		t.Fatal("expected false success", e)
	}
	if r.Err() == nil || strings.Contains(read(t, r.Path), "func:agent.Template.Chat(") {
		t.Fatal("expected lost completion")
	}
	t.Log("REPRODUCED: Close returns while Chat is active; Chat later reports success without completion")
}
