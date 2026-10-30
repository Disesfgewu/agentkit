// Quickstart builds a support-ticket agent from agentkit with no provider
// account: a scripted model stands in for the LLM unless -router is set.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Disesfgewu/agentkit/agent"
	"github.com/Disesfgewu/agentkit/graph"
	"github.com/Disesfgewu/agentkit/model"
	"os"
	"strings"
)

const classifyPrompt = "Classify this ticket as refund or question: "

type scriptedModel struct{}

func (scriptedModel) Chat(_ context.Context, r model.Request) (model.Response, error) {
	last := r.Messages[len(r.Messages)-1].Content
	text := "Draft reply based on: " + last
	if ticket, ok := strings.CutPrefix(last, classifyPrompt); ok {
		text = "question"
		if strings.Contains(strings.ToLower(ticket), "refund") {
			text = "refund"
		}
	}
	return model.Response{Text: text, Provider: "scripted", Model: "offline"}, nil
}

type orderLookup struct{}

func (orderLookup) Name() string { return "lookup_order" }
func (orderLookup) Run(context.Context, json.RawMessage) (any, error) {
	return map[string]any{"order": "A-1001", "refundable": true}, nil
}

// run returns the final state and the audit log path. The LLM classifies, but a
// plain function picks the route, so the model cannot skip a step.
func run(ctx context.Context, client model.Client, ticket, logDir string) (graph.State, string, error) {
	a := &agent.Template{
		Name:         "support",
		SystemPrompt: "You are a support agent. Use only the supplied facts.",
		LogDir:       logDir,
		Model:        client,
		Retriever:    &agent.LocalRetriever{Documents: []agent.Document{{ID: "faq:shipping", Source: "faq.md", Text: "Shipping takes three days."}}},
		Tools:        map[string]agent.Tool{"lookup_order": orderLookup{}},
	}
	defer a.Close()
	runtime, err := a.Runtime()
	if err != nil {
		return nil, "", err
	}
	ctx = runtime.Context(ctx)
	ask := func(ctx context.Context, prompt string) (string, error) {
		response, err := a.Chat(ctx, []model.Message{{Role: "user", Content: prompt}}, "")
		return response.Text, err
	}

	g := graph.NewContext(ctx)
	for name, node := range map[string]graph.Node{
		"classify": func(ctx context.Context, s graph.State) (graph.State, error) {
			text, err := ask(ctx, classifyPrompt+ticket)
			return graph.State{"category": strings.TrimSpace(text)}, err
		},
		"lookup_order": func(ctx context.Context, s graph.State) (graph.State, error) {
			order, err := a.RunTool(ctx, "lookup_order", json.RawMessage(`{}`))
			return graph.State{"facts": fmt.Sprint(order)}, err
		},
		"search_docs": func(ctx context.Context, s graph.State) (graph.State, error) {
			docs, err := a.GetInformation(ctx, ticket, 3)
			return graph.State{"facts": fmt.Sprint(docs)}, err
		},
		"reply": func(ctx context.Context, s graph.State) (graph.State, error) {
			text, err := ask(ctx, "Reply to the ticket using these facts: "+s["facts"].(string))
			return graph.State{"reply": text}, err
		},
	} {
		if err := g.AddNodeContext(ctx, name, node); err != nil {
			return nil, "", err
		}
	}
	g.EdgeContext(ctx, graph.Start, "classify")
	g.ConditionalContext(ctx, "classify", func(s graph.State) (string, error) {
		if s["category"] == "refund" {
			return "refund", nil
		}
		return "question", nil
	}, map[string]string{"refund": "lookup_order", "question": "search_docs"})
	g.EdgeContext(ctx, "lookup_order", "reply")
	g.EdgeContext(ctx, "search_docs", "reply")
	g.EdgeContext(ctx, "reply", graph.End)

	result, err := g.Run(ctx, graph.State{"ticket": ticket})
	return result.State, runtime.Path(), err
}

func main() {
	ticket := flag.String("ticket", "I want a refund for my order", "support ticket text")
	router := flag.String("router", "", "optional LLMRouter URL; empty uses the scripted model")
	logDir := flag.String("log-dir", "log", "audit log directory")
	flag.Parse()
	var client model.Client = scriptedModel{}
	if *router != "" {
		remote, err := model.NewRemote(*router, os.Getenv("LLMROUTER_TOKEN"), "")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		client = remote
	}
	state, audit, err := run(context.Background(), client, *ticket, *logDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("category: %v\nreply: %v\naudit: %s\n", state["category"], state["reply"], audit)
}
