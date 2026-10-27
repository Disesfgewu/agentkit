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
)

type Retriever interface {
	Retrieve(context.Context, string, int) ([]Document, error)
}

func (a *Template) auditContext(ctx context.Context) (context.Context, error) {
	a.logMu.Lock()
	current := a.Logger
	a.logMu.Unlock()
	if current != nil && agentlog.FromContext(ctx) == current {
		return ctx, nil
	}
	r, err := a.recorder()
	if err != nil {
		return ctx, err
	}
	return agentlog.WithRecorder(ctx, r), nil
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
	Name string
	// SystemPrompt is prepended to Ask and Chat model messages.
	SystemPrompt string
	// MaxOutputTokens is forwarded to provider requests. Zero uses the model
	// client's default, preserving backward compatibility.
	MaxOutputTokens int
	LogDir          string
	Logger          *agentlog.Recorder
	logMu           sync.Mutex
	closed          bool
	Model           model.Client
	Retriever       Retriever
	Tools           map[string]Tool
	Knowledge       *gsd.Knowledge
	Before          func(context.Context, graph.State) error
	After           func(context.Context, graph.Result) error
}

// BuildMessages applies the agent-level system prompt and an optional operation instruction.
func (a *Template) BuildMessages(ctx context.Context, messages []model.Message, instruction string) (result []model.Message) {
	var e error
	ctx, e = a.auditContext(ctx)
	if e != nil {
		panic(e)
	}
	_, span := agentlog.Start(ctx, "agent.Template.BuildMessages", map[string]any{"system_prompt": a.SystemPrompt, "instruction": instruction, "messages": messages})
	defer span.Finish(nil, func() any { return result }, nil)
	span.Require()
	system := strings.TrimSpace(strings.Join([]string{a.SystemPrompt, instruction}, "\n"))
	result = append([]model.Message(nil), messages...)
	if system != "" {
		result = append([]model.Message{{Role: "system", Content: system}}, result...)
	}
	return result
}

func (a *Template) recorder() (*agentlog.Recorder, error) {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.closed {
		return nil, agentlog.ErrClosed
	}
	if a.Logger == nil {
		r, err := agentlog.New(a.Name, a.LogDir)
		if err != nil {
			return nil, err
		}
		a.Logger = r
	}
	return a.Logger, a.Logger.Err()
}
func (a *Template) Close() error {
	a.logMu.Lock()
	a.closed = true
	r := a.Logger
	a.logMu.Unlock()
	if r == nil {
		return nil
	}
	err := r.Close()
	a.logMu.Lock()
	a.Logger = nil
	a.logMu.Unlock()
	return err
}

func (a *Template) GetInformation(ctx context.Context, q string, k int) (docs []Document, err error) {
	ctx, e := a.auditContext(ctx)
	if e != nil {
		return nil, e
	}
	ctx, span := agentlog.Start(ctx, "agent.Template.GetInformation", map[string]any{"query": q, "k": k})
	defer span.Finish(&err, func() any { return docs }, nil)
	if err = span.Err(); err != nil {
		return
	}
	if a.Retriever == nil {
		return nil, errors.New("retriever not configured")
	}
	return a.Retriever.Retrieve(ctx, q, k)
}
func (a *Template) Ask(ctx context.Context, q string, reasoning string) (result graph.Result, err error) {
	ctx, e := a.auditContext(ctx)
	if e != nil {
		return graph.Result{}, e
	}
	ctx, span := agentlog.Start(ctx, "agent.Template.Ask", map[string]any{"question": q, "reasoning": reasoning})
	defer span.Finish(&err, func() any { return result }, func() *int {
		if result.State != nil && result.State["usage_known"] == true {
			n, _ := result.State["total_tokens"].(int)
			return &n
		}
		return nil
	})
	if err = span.Err(); err != nil {
		return
	}
	g := graph.NewContext(ctx)
	if err := g.AddNodeContext(ctx, "retrieve", func(ctx context.Context, s graph.State) (graph.State, error) {
		if a.Retriever == nil {
			return graph.State{"documents": []Document{}}, nil
		}
		docs, err := a.Retriever.Retrieve(ctx, q, 5)
		return graph.State{"documents": docs}, err
	}); err != nil {
		return graph.Result{}, err
	}
	g.AddNodeContext(ctx, "generate", func(ctx context.Context, s graph.State) (graph.State, error) {
		if a.Model == nil {
			return nil, errors.New("model not configured")
		}
		docs, _ := s["documents"].([]Document)
		var b strings.Builder
		for _, d := range docs {
			fmt.Fprintf(&b, "[%s] %s\n", d.ID, d.Text)
		}
		instruction := "Answer using the retrieved evidence. Cite document IDs. If evidence is insufficient, say so.\n" + b.String()
		resp, err := a.Model.Chat(ctx, model.Request{Messages: a.BuildMessages(ctx, []model.Message{{Role: "user", Content: q}}, instruction), Reasoning: reasoning, MaxOutputTokens: a.MaxOutputTokens})
		if err != nil {
			return nil, err
		}
		return graph.State{"answer": resp.Text, "provider": resp.Provider, "model": resp.Model, "input_tokens": resp.InputTokens, "output_tokens": resp.OutputTokens, "total_tokens": resp.InputTokens + resp.OutputTokens, "usage_known": resp.UsageKnown}, nil
	})
	g.AddNodeContext(ctx, "validate", func(_ context.Context, s graph.State) (graph.State, error) {
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
	g.EdgeContext(ctx, graph.Start, "retrieve")
	g.EdgeContext(ctx, "retrieve", "generate")
	g.EdgeContext(ctx, "generate", "validate")
	g.EdgeContext(ctx, "validate", graph.End)
	initial := graph.State{"question": q}
	if a.Before != nil {
		_, err := agentlog.Call(ctx, "agent.Hook.Before", initial, func(ctx context.Context) (bool, error) { e := a.Before(ctx, initial); return e == nil, e })
		if err != nil {
			return graph.Result{}, err
		}
	}
	r, err := g.Run(ctx, initial)
	if err == nil && a.After != nil {
		_, err = agentlog.Call(ctx, "agent.Hook.After", r, func(ctx context.Context) (bool, error) { e := a.After(ctx, r); return e == nil, e })
	}
	result = r
	return result, err
}
func (a *Template) Chat(ctx context.Context, messages []model.Message, reasoning string) (response model.Response, err error) {
	ctx, e := a.auditContext(ctx)
	if e != nil {
		return model.Response{}, e
	}
	ctx, span := agentlog.Start(ctx, "agent.Template.Chat", map[string]any{"messages": messages, "reasoning": reasoning})
	defer span.Finish(&err, func() any { return response }, func() *int {
		if response.UsageKnown {
			n := response.InputTokens + response.OutputTokens
			return &n
		}
		return nil
	})
	if err = span.Err(); err != nil {
		return
	}
	if a.Model == nil {
		return model.Response{}, errors.New("model not configured")
	}
	return a.Model.Chat(ctx, model.Request{Messages: a.BuildMessages(ctx, messages, ""), Reasoning: reasoning, MaxOutputTokens: a.MaxOutputTokens})
}
func (a *Template) RunTool(ctx context.Context, name string, input json.RawMessage) (output any, err error) {
	ctx, e := a.auditContext(ctx)
	if e != nil {
		return nil, e
	}
	ctx, span := agentlog.Start(ctx, "agent.Template.RunTool", map[string]any{"name": name, "input": input})
	defer span.Finish(&err, func() any { return output }, nil)
	if err = span.Err(); err != nil {
		return
	}
	t, ok := a.Tools[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %s", name)
	}
	return agentlog.Call(ctx, "agent.Tool."+name, input, func(ctx context.Context) (any, error) { return t.Run(ctx, input) })
}

// Triage returns a rule-validated diagnostic suggestion from observable signals.
// The generator truth file is never opened by this method.
func (a *Template) Triage(ctx context.Context, observed gsd.Case) (prediction gsd.Prediction, err error) {
	ctx, e := a.auditContext(ctx)
	if e != nil {
		return gsd.Prediction{}, e
	}
	ctx, span := agentlog.Start(ctx, "agent.Template.Triage", observed)
	defer span.Finish(&err, func() any { return prediction }, nil)
	if err = span.Err(); err != nil {
		return
	}
	if err := ctx.Err(); err != nil {
		return gsd.Prediction{}, err
	}
	if a.Knowledge == nil {
		return gsd.Prediction{}, errors.New("GSD knowledge not configured")
	}
	return gsd.DecideContext(ctx, observed, *a.Knowledge), nil
}
