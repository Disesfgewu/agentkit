package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agentlog"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/graph"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/gsd"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/model"
	"strings"
	"sync"
	"time"
)

type Retriever interface {
	Retrieve(context.Context, string, int) ([]Document, error)
}
type Document struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source string `json:"source"`
}
type Tool interface {
	Name() string
	Run(context.Context, json.RawMessage) (any, error)
}
type Template struct {
	Name      string
	LogDir    string
	Logger    *agentlog.Recorder
	logMu     sync.Mutex
	Model     model.Client
	Retriever Retriever
	Tools     map[string]Tool
	Knowledge *gsd.Knowledge
	Before    func(context.Context, graph.State) error
	After     func(context.Context, graph.Result) error
}

func (a *Template) recorder() (*agentlog.Recorder, error) {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.Logger == nil {
		r, err := agentlog.New(a.Name, a.LogDir)
		if err != nil {
			return nil, err
		}
		a.Logger = r
	}
	return a.Logger, nil
}
func (a *Template) Close() error {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.Logger == nil {
		return nil
	}
	err := a.Logger.Close()
	a.Logger = nil
	return err
}

func (a *Template) GetInformation(ctx context.Context, q string, k int) (docs []Document, err error) {
	log, e := a.recorder()
	if e != nil {
		return nil, e
	}
	start := time.Now()
	defer func() {
		_ = log.Record("GetInformation", map[string]any{"query": q, "k": k}, docs, nil, time.Since(start), err)
	}()
	if a.Retriever == nil {
		return nil, errors.New("retriever not configured")
	}
	return a.Retriever.Retrieve(ctx, q, k)
}
func (a *Template) Ask(ctx context.Context, q string, reasoning string) (result graph.Result, err error) {
	log, e := a.recorder()
	if e != nil {
		return graph.Result{}, e
	}
	start := time.Now()
	defer func() {
		var tok *int
		if result.State != nil && result.State["usage_known"] == true {
			n, _ := result.State["total_tokens"].(int)
			tok = &n
		}
		_ = log.Record("Ask", map[string]any{"question": q, "reasoning": reasoning}, result, tok, time.Since(start), err)
	}()
	g := graph.New()
	if err := g.AddNode("retrieve", func(ctx context.Context, s graph.State) (graph.State, error) {
		if a.Retriever == nil {
			return graph.State{"documents": []Document{}}, nil
		}
		docs, err := a.Retriever.Retrieve(ctx, q, 5)
		return graph.State{"documents": docs}, err
	}); err != nil {
		return graph.Result{}, err
	}
	g.AddNode("generate", func(ctx context.Context, s graph.State) (graph.State, error) {
		if a.Model == nil {
			return nil, errors.New("model not configured")
		}
		docs, _ := s["documents"].([]Document)
		var b strings.Builder
		for _, d := range docs {
			fmt.Fprintf(&b, "[%s] %s\n", d.ID, d.Text)
		}
		resp, err := a.Model.Chat(ctx, model.Request{Messages: []model.Message{{Role: "system", Content: "Answer using the retrieved evidence. Cite document IDs. If evidence is insufficient, say so.\n" + b.String()}, {Role: "user", Content: q}}, Reasoning: reasoning})
		if err != nil {
			return nil, err
		}
		return graph.State{"answer": resp.Text, "provider": resp.Provider, "model": resp.Model, "input_tokens": resp.InputTokens, "output_tokens": resp.OutputTokens, "total_tokens": resp.InputTokens + resp.OutputTokens, "usage_known": resp.UsageKnown}, nil
	})
	g.AddNode("validate", func(_ context.Context, s graph.State) (graph.State, error) {
		answer, _ := s["answer"].(string)
		docs, _ := s["documents"].([]Document)
		cited := []string{}
		for _, d := range docs {
			if strings.Contains(answer, d.ID) || (strings.HasPrefix(d.ID, "card:") && strings.Contains(answer, strings.TrimPrefix(d.ID, "card:"))) {
				cited = append(cited, d.ID)
			}
		}
		return graph.State{"citation_ids": cited, "citation_valid": len(docs) == 0 || len(cited) > 0}, nil
	})
	g.Edge(graph.Start, "retrieve")
	g.Edge("retrieve", "generate")
	g.Edge("generate", "validate")
	g.Edge("validate", graph.End)
	initial := graph.State{"question": q}
	if a.Before != nil {
		if err := a.Before(ctx, initial); err != nil {
			return graph.Result{}, err
		}
	}
	r, err := g.Run(ctx, initial)
	if err == nil && a.After != nil {
		err = a.After(ctx, r)
	}
	result = r
	return result, err
}
func (a *Template) Chat(ctx context.Context, messages []model.Message, reasoning string) (response model.Response, err error) {
	log, e := a.recorder()
	if e != nil {
		return model.Response{}, e
	}
	start := time.Now()
	defer func() {
		var tok *int
		if response.UsageKnown {
			n := response.InputTokens + response.OutputTokens
			tok = &n
		}
		_ = log.Record("Chat", map[string]any{"messages": messages, "reasoning": reasoning}, response, tok, time.Since(start), err)
	}()
	if a.Model == nil {
		return model.Response{}, errors.New("model not configured")
	}
	return a.Model.Chat(ctx, model.Request{Messages: messages, Reasoning: reasoning})
}
func (a *Template) RunTool(ctx context.Context, name string, input json.RawMessage) (output any, err error) {
	log, e := a.recorder()
	if e != nil {
		return nil, e
	}
	start := time.Now()
	defer func() {
		_ = log.Record("RunTool", map[string]any{"name": name, "input": input}, output, nil, time.Since(start), err)
	}()
	t, ok := a.Tools[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %s", name)
	}
	return t.Run(ctx, input)
}

// Triage returns a rule-validated diagnostic suggestion from observable signals.
// The generator truth file is never opened by this method.
func (a *Template) Triage(ctx context.Context, observed gsd.Case) (prediction gsd.Prediction, err error) {
	log, e := a.recorder()
	if e != nil {
		return gsd.Prediction{}, e
	}
	start := time.Now()
	defer func() { _ = log.Record("Triage", observed, prediction, nil, time.Since(start), err) }()
	if err := ctx.Err(); err != nil {
		return gsd.Prediction{}, err
	}
	if a.Knowledge == nil {
		return gsd.Prediction{}, errors.New("GSD knowledge not configured")
	}
	return gsd.Decide(observed, *a.Knowledge), nil
}
