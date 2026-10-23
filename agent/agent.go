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
	LogDir       string
	Logger       *agentlog.Recorder
	logMu        sync.Mutex
	Model        model.Client
	Retriever    Retriever
	Tools        map[string]Tool
	Knowledge    *gsd.Knowledge
	Before       func(context.Context, graph.State) error
	After        func(context.Context, graph.Result) error
}

// BuildMessages applies the agent-level system prompt and an optional operation instruction.
func (a *Template) BuildMessages(ctx context.Context, messages []model.Message, instruction string) (result []model.Message) {
	_, span := agentlog.Start(ctx, "agent.Template.BuildMessages", map[string]any{"system_prompt": a.SystemPrompt, "instruction": instruction, "messages": messages})
	defer func() { span.End(result, nil, nil) }()
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
	ctx, e := a.auditContext(ctx)
	if e != nil {
		return nil, e
	}
	ctx, span := agentlog.Start(ctx, "agent.Template.GetInformation", map[string]any{"query": q, "k": k})
	defer func() { span.End(docs, nil, err) }()
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
	defer func() {
		var tok *int
		if result.State != nil && result.State["usage_known"] == true {
			n, _ := result.State["total_tokens"].(int)
			tok = &n
		}
		span.End(result, tok, err)
	}()
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
		resp, err := a.Model.Chat(ctx, model.Request{Messages: a.BuildMessages(ctx, []model.Message{{Role: "user", Content: q}}, instruction), Reasoning: reasoning})
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
		hookCtx, hookSpan := agentlog.Start(ctx, "agent.Hook.Before", initial)
		err := a.Before(hookCtx, initial)
		hookSpan.End(map[string]any{"accepted": err == nil}, nil, err)
		if err != nil {
			return graph.Result{}, err
		}
	}
	r, err := g.Run(ctx, initial)
	if err == nil && a.After != nil {
		hookCtx, hookSpan := agentlog.Start(ctx, "agent.Hook.After", r)
		err = a.After(hookCtx, r)
		hookSpan.End(map[string]any{"accepted": err == nil}, nil, err)
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
	defer func() {
		var tok *int
		if response.UsageKnown {
			n := response.InputTokens + response.OutputTokens
			tok = &n
		}
		span.End(response, tok, err)
	}()
	if a.Model == nil {
		return model.Response{}, errors.New("model not configured")
	}
	return a.Model.Chat(ctx, model.Request{Messages: a.BuildMessages(ctx, messages, ""), Reasoning: reasoning})
}
func (a *Template) RunTool(ctx context.Context, name string, input json.RawMessage) (output any, err error) {
	ctx, e := a.auditContext(ctx)
	if e != nil {
		return nil, e
	}
	ctx, span := agentlog.Start(ctx, "agent.Template.RunTool", map[string]any{"name": name, "input": input})
	defer func() { span.End(output, nil, err) }()
	t, ok := a.Tools[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %s", name)
	}
	toolCtx, toolSpan := agentlog.Start(ctx, "agent.Tool."+name, input)
	output, err = t.Run(toolCtx, input)
	toolSpan.End(output, nil, err)
	return output, err
}

// Triage returns a rule-validated diagnostic suggestion from observable signals.
// The generator truth file is never opened by this method.
func (a *Template) Triage(ctx context.Context, observed gsd.Case) (prediction gsd.Prediction, err error) {
	ctx, e := a.auditContext(ctx)
	if e != nil {
		return gsd.Prediction{}, e
	}
	ctx, span := agentlog.Start(ctx, "agent.Template.Triage", observed)
	defer func() { span.End(prediction, nil, err) }()
	if err := ctx.Err(); err != nil {
		return gsd.Prediction{}, err
	}
	if a.Knowledge == nil {
		return gsd.Prediction{}, errors.New("GSD knowledge not configured")
	}
	return gsd.DecideContext(ctx, observed, *a.Knowledge), nil
}
