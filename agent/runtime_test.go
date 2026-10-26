package agent

import (
	"context"
	"errors"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agentlog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRuntimeAutomaticallyAuditsOperations(t *testing.T) {
	r, e := NewRuntime("automatic", t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { w.Write([]byte(`{"value":42}`)) }))
	defer server.Close()
	var out map[string]int
	if e = r.JSON(context.Background(), server.Client(), "GET", server.URL, nil, &out); e != nil {
		t.Fatal(e)
	}
	_, e = r.Execute(context.Background(), "custom.operation", map[string]any{"api_key": "never-print"}, func(ctx context.Context) (any, error) {
		return r.Execute(ctx, "custom.child", 7, func(context.Context) (any, error) { return nil, errors.New("intentional failure") })
	})
	if e == nil {
		t.Fatal("lost error")
	}
	if e = r.Close(); e != nil {
		t.Fatal(e)
	}
	n, e := agentlog.Verify(r.Path())
	if e != nil || n != 3 {
		t.Fatalf("records=%d err=%v", n, e)
	}
	b, _ := os.ReadFile(r.Path())
	s := string(b)
	if strings.Contains(s, "never-print") || strings.Count(s, "func:HTTP.GET(") != 1 || !strings.Contains(s, "intentional failure") || !strings.Contains(s, "parent_call_id:2") {
		t.Fatal("missing automatic record, duplicate, or unredacted secret")
	}
}
func TestRuntimeReportsAuditWriteFailure(t *testing.T) {
	r, e := NewRuntime("failure", t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = r.recorder.Close(); e != nil {
		t.Fatal(e)
	}
	_, e = r.Execute(context.Background(), "closed-log", nil, func(context.Context) (any, error) { return true, nil })
	if e == nil {
		t.Fatal("audit failure silently ignored")
	}
	called := false
	_, e = r.Execute(context.Background(), "blocked", nil, func(context.Context) (any, error) { called = true; return nil, nil })
	if e == nil || called {
		t.Fatal("operation continued after audit failure")
	}
}
