package agent

import (
	"context"
	"github.com/Disesfgewu/agentkit/agentlog"
	"github.com/Disesfgewu/agentkit/model"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeModel struct{}

func (fakeModel) Chat(_ context.Context, _ model.Request) (model.Response, error) {
	return model.Response{Text: "ok", Model: "fake", Provider: "mock", InputTokens: 2, OutputTokens: 1, UsageKnown: true}, nil
}

type captureModel struct{ request model.Request }

func (c *captureModel) Chat(_ context.Context, r model.Request) (model.Response, error) {
	c.request = r
	return model.Response{Text: "ok"}, nil
}
func TestSystemPromptIsInjected(t *testing.T) {
	client := &captureModel{}
	a := Template{Name: "prompt-test", SystemPrompt: "You are the audit agent.", LogDir: t.TempDir(), Model: client}
	defer a.Close()
	if _, err := a.Chat(context.Background(), []model.Message{{Role: "user", Content: "status"}}, ""); err != nil {
		t.Fatal(err)
	}
	if len(client.request.Messages) != 2 || client.request.Messages[0].Role != "system" || client.request.Messages[0].Content != a.SystemPrompt {
		t.Fatalf("messages=%+v", client.request.Messages)
	}
}
func TestAutomaticLogger(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "log")
	a := Template{Name: "demo-agent", LogDir: dir, Model: fakeModel{}}
	_, err := a.Chat(context.Background(), []model.Message{{Role: "user", Content: "hello"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "demo-agent_*.log"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("log path: %v %v", paths, err)
	}
	b, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	for _, want := range []string{"[DEBUG] agent:demo-agent", "phase:END", "func:agent.Template.Chat(input:", "return:", "token:3", "response_time:", "time:", "prev_hash:", "hash:"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %s in %s", want, line)
		}
	}
}

type citedModel struct{}

func (citedModel) Chat(_ context.Context, _ model.Request) (model.Response, error) {
	return model.Response{Text: "POLICY-7 covers refunds. See [doc:guide].", Model: "fake", Provider: "mock"}, nil
}
func TestAskCitationTrace(t *testing.T) {
	ask := func(a *Template, id string) any {
		a.Name, a.LogDir, a.Model = "rag-test", t.TempDir(), citedModel{}
		a.Retriever = &LocalRetriever{Documents: []Document{{ID: id, Text: "refund policy", Source: "policy.md"}}}
		defer a.Close()
		r, err := a.Ask(context.Background(), "refund policy", "")
		if err != nil || !r.Finished || len(r.Steps) != 3 {
			t.Fatalf("unexpected RAG trace %+v %v", r, err)
		}
		return r.State["citation_valid"]
	}
	if ask(&Template{}, "doc:guide") != true {
		t.Error("bracketed document ID should count as a citation")
	}
	if ask(&Template{}, "doc:other") != false {
		t.Error("uncited document should not count")
	}
	if ask(&Template{}, "POLICY-7") != false {
		t.Error("mentioning an ID without brackets should not count")
	}
	alias := func(d Document, answer string) bool {
		return strings.Contains(answer, strings.TrimPrefix(d.ID, "card:"))
	}
	if ask(&Template{CitationMatch: alias}, "card:POLICY-7") != true {
		t.Error("CitationMatch should decide citation validity")
	}
}

func TestFullAuditCallTree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[card:K-VALVE]"}}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`))
	}))
	defer server.Close()
	client, err := model.New(model.Config{Name: "mock", Protocol: "openai_chat", BaseURL: server.URL, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	a := Template{Name: "audit-tree", LogDir: t.TempDir(), Model: client, Retriever: &LocalRetriever{Documents: []Document{{ID: "card:K-VALVE", Text: "pressure_high valve_position_mismatch", Source: "knowledge.json#K-VALVE"}}}}
	r, err := a.Ask(context.Background(), "pressure_high valve_position_mismatch", "")
	if err != nil || r.State["citation_valid"] != true {
		t.Fatalf("result=%+v err=%v", r, err)
	}
	path := a.Logger.Path
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := agentlog.Verify(path)
	if err != nil || records < 20 {
		t.Fatalf("records=%d err=%v", records, err)
	}
	b, _ := os.ReadFile(path)
	log := string(b)
	for _, fn := range []string{"agent.Template.Ask", "graph.Graph.AddNode", "graph.Node.retrieve", "agent.LocalRetriever.Retrieve", "agent.tokens", "graph.Node.generate", "agent.ChatAgent.BuildMessages", "model.HTTPClient.Chat", "model.HTTPClient.payload", "model.HTTPClient.parse", "graph.Node.validate"} {
		if !strings.Contains(log, "func:"+fn+"(") {
			t.Errorf("missing audited function %s", fn)
		}
	}
}
