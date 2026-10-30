package agent

import (
	"context"
	"errors"
	"github.com/Disesfgewu/agentkit/agentlog"
)

// Ingester indexes documents for later retrieval.
type Ingester interface {
	Ingest(context.Context, ...Document) error
}

// Agent is the full surface of a composed agent. Each role is its own small
// interface, so code should depend on only the role it uses.
type Agent interface {
	Chatter
	Asker
	Ingester
	ToolRunner
	GetInformation(context.Context, string, int) ([]Document, error)
	Close() error
}

var (
	_ Agent      = (*Template)(nil)
	_ Chatter    = (*ChatAgent)(nil)
	_ Asker      = (*RAGAgent)(nil)
	_ Ingester   = (*Indexer)(nil)
	_ Retriever  = (*VectorRetriever)(nil)
	_ Ingester   = (*EmbeddingAgent)(nil)
	_ Retriever  = (*EmbeddingAgent)(nil)
	_ ToolRunner = ToolBox(nil)
)

// Config assembles an Agent from independent parts. Every part is optional, and
// a method whose part is missing returns an error.
type Config struct {
	Name   string
	LogDir string
	Logger *agentlog.Recorder
	// Chat is usually a *ChatAgent.
	Chat Chatter
	// Retriever is usually a *EmbeddingAgent or a *LocalRetriever.
	Retriever Retriever
	// Ingester defaults to Retriever when it can also ingest.
	Ingester Ingester
	// Tools is usually a ToolBox.
	Tools ToolRunner
}

// New composes the parts under one audit session.
func New(c Config) *Template {
	return &Template{Name: c.Name, LogDir: c.LogDir, Logger: c.Logger, ChatAgent: c.Chat, Retriever: c.Retriever, Ingester: c.Ingester, ToolRunner: c.Tools}
}

func (a *Template) Ingest(ctx context.Context, docs ...Document) (err error) {
	ctx, e := a.auditContext(ctx)
	if e != nil {
		return e
	}
	ctx, span := agentlog.Start(ctx, "agent.Template.Ingest", map[string]any{"documents": len(docs)})
	defer span.Finish(&err, nil, nil)
	if err = span.Err(); err != nil {
		return
	}
	ingester := a.Ingester
	if ingester == nil {
		ingester, _ = a.Retriever.(Ingester)
	}
	if ingester == nil {
		return errors.New("no ingester configured")
	}
	return ingester.Ingest(ctx, docs...)
}
