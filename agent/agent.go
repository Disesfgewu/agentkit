package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Disesfgewu/agentkit/agentlog"
	"github.com/Disesfgewu/agentkit/graph"
	"github.com/Disesfgewu/agentkit/model"
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
	// Score is the retriever's relevance, when it has one.
	Score float64 `json:"score,omitempty"`
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
	Before          func(context.Context, graph.State) error
	After           func(context.Context, graph.Result) error
	// CitationMatch is forwarded to the RAGAgent behind Ask.
	CitationMatch func(Document, string) bool
	// ChatAgent replaces Model and SystemPrompt with any Chatter.
	ChatAgent Chatter
	// Ingester indexes documents; nil uses Retriever when it can ingest.
	Ingester Ingester
	// ToolRunner replaces Tools with any runner.
	ToolRunner ToolRunner
}

// chatter returns the configured part, or a ChatAgent over the flat fields.
func (a *Template) chatter() Chatter {
	if a.ChatAgent != nil {
		return a.ChatAgent
	}
	return &ChatAgent{Client: a.Model, SystemPrompt: a.SystemPrompt, MaxOutputTokens: a.MaxOutputTokens}
}

// BuildMessages applies the agent-level system prompt and an optional operation instruction.
func (a *Template) BuildMessages(ctx context.Context, messages []model.Message, instruction string) (result []model.Message) {
	var e error
	ctx, e = a.auditContext(ctx)
	if e != nil {
		panic(e)
	}
	return (&ChatAgent{SystemPrompt: a.SystemPrompt}).BuildMessages(ctx, messages, instruction)
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
	rag := RAGAgent{Chat: a.chatter(), Retriever: a.Retriever, CitationMatch: a.CitationMatch, Before: a.Before, After: a.After}
	result, err = rag.Ask(ctx, q, reasoning)
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
	return a.chatter().Chat(ctx, messages, reasoning)
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
	if a.ToolRunner != nil {
		return a.ToolRunner.RunTool(ctx, name, input)
	}
	return ToolBox(a.Tools).RunTool(ctx, name, input)
}
