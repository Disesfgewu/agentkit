package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/Disesfgewu/agentkit/agentlog"
	"github.com/Disesfgewu/agentkit/graph"
	"github.com/Disesfgewu/agentkit/model"
	"strings"
)

const defaultRAGDocuments = 5

// Asker answers a question from evidence.
type Asker interface {
	Ask(ctx context.Context, question, reasoning string) (graph.Result, error)
}

// RAGAgent runs retrieve, generate, and validate as a graph. It needs only a
// Chatter and a Retriever, and with no Retriever it answers from no evidence.
type RAGAgent struct {
	Chat      Chatter
	Retriever Retriever
	// K is the number of documents to retrieve; zero uses five.
	K int
	// CitationMatch decides whether an answer cites a document. Nil matches the
	// document ID literally; set it when documents have an alias.
	CitationMatch func(Document, string) bool
	Before        func(context.Context, graph.State) error
	After         func(context.Context, graph.Result) error
}

func (r *RAGAgent) Ask(ctx context.Context, q, reasoning string) (result graph.Result, err error) {
	ctx, span := agentlog.Start(ctx, "agent.RAGAgent.Ask", map[string]any{"question": q, "reasoning": reasoning})
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
	k := r.K
	if k <= 0 {
		k = defaultRAGDocuments
	}
	g := graph.NewContext(ctx)
	if err := g.AddNodeContext(ctx, "retrieve", func(ctx context.Context, s graph.State) (graph.State, error) {
		if r.Retriever == nil {
			return graph.State{"documents": []Document{}}, nil
		}
		docs, err := r.Retriever.Retrieve(ctx, q, k)
		return graph.State{"documents": docs}, err
	}); err != nil {
		return graph.Result{}, err
	}
	g.AddNodeContext(ctx, "generate", func(ctx context.Context, s graph.State) (graph.State, error) {
		if r.Chat == nil {
			return nil, errors.New("model not configured")
		}
		docs, _ := s["documents"].([]Document)
		var b strings.Builder
		for _, d := range docs {
			fmt.Fprintf(&b, "[%s] %s\n", d.ID, d.Text)
		}
		instruction := "Answer using the retrieved evidence. Cite document IDs. If evidence is insufficient, say so.\n" + b.String()
		resp, err := r.Chat.Chat(ctx, []model.Message{{Role: "system", Content: instruction}, {Role: "user", Content: q}}, reasoning)
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
			if r.cites(d, answer) {
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
	if r.Before != nil {
		_, err := agentlog.Call(ctx, "agent.Hook.Before", initial, func(ctx context.Context) (bool, error) { e := r.Before(ctx, initial); return e == nil, e })
		if err != nil {
			return graph.Result{}, err
		}
	}
	out, err := g.Run(ctx, initial)
	if err == nil && r.After != nil {
		_, err = agentlog.Call(ctx, "agent.Hook.After", out, func(ctx context.Context) (bool, error) { e := r.After(ctx, out); return e == nil, e })
	}
	result = out
	return result, err
}

func (r *RAGAgent) cites(d Document, answer string) bool {
	if r.CitationMatch != nil {
		return r.CitationMatch(d, answer)
	}
	return strings.Contains(answer, d.ID)
}
