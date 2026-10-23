package agent

import (
	"context"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/gsd"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeModel struct{}

func (fakeModel) Chat(_ context.Context, _ model.Request) (model.Response, error) {
	return model.Response{Text: "ok", Model: "fake", Provider: "mock", InputTokens: 2, OutputTokens: 1, UsageKnown: true}, nil
}
func TestAutomaticLogger(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "log")
	a := Template{Name: "etch-agent", LogDir: dir, Model: fakeModel{}}
	_, err := a.Chat(context.Background(), []model.Message{{Role: "user", Content: "hello"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "etch-agent_*.log"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("log path: %v %v", paths, err)
	}
	b, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	for _, want := range []string{"[DEBUG] agent:etch-agent", "func:Chat(input:", "return:", "token:3", "response_time:", "time:"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %s in %s", want, line)
		}
	}
}

func TestTriageUsesKnowledge(t *testing.T) {
	k := gsd.Knowledge{Cards: []gsd.Card{{ID: "K-VALVE", Fault: "valve_stick", Required: []string{"pressure_high", "valve_position_mismatch"}, Action: "inspect_valve_position_trace", Evidence: []string{"gsd_rules"}}}, Sources: map[string]gsd.Source{"gsd_rules": {Kind: "generator_spec"}}}
	a := Template{Name: "test", LogDir: t.TempDir(), Knowledge: &k}
	defer a.Close()
	p, err := a.Triage(context.Background(), gsd.Case{CaseID: "one", Signals: []string{"pressure_high", "valve_position_mismatch"}})
	if err != nil || p.Decision != "valve_stick" || p.Action != "inspect_valve_position_trace" {
		t.Fatalf("%+v %v", p, err)
	}
}

type citedModel struct{}

func (citedModel) Chat(_ context.Context, _ model.Request) (model.Response, error) {
	return model.Response{Text: "K-VALVE supports valve trace inspection.", Model: "fake", Provider: "mock"}, nil
}
func TestAskCitationTrace(t *testing.T) {
	a := Template{Name: "rag-test", LogDir: t.TempDir(), Model: citedModel{}, Retriever: &LocalRetriever{Documents: []Document{{ID: "card:K-VALVE", Text: "pressure_high valve_position_mismatch", Source: "knowledge.json#K-VALVE"}}}}
	defer a.Close()
	r, err := a.Ask(context.Background(), "pressure_high valve_position_mismatch", "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Finished || r.State["citation_valid"] != true || len(r.Steps) != 3 {
		t.Fatalf("unexpected RAG trace %+v", r)
	}
}
