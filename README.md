# agentkit

Go building blocks for auditable agentic applications: a LangGraph-style state graph, a multi-provider LLM client and router, a composable agent template, and a hash-chained audit log. No third-party dependencies.

This is a LangGraph-inspired Go runtime, not an official port. It implements the low-level semantics and makes no API compatibility claim. [`examples/langgraph_node.py`](examples/langgraph_node.py) runs the Go client as a node inside a Python LangGraph workflow.

## Packages

| Package | Purpose | Depends on |
|---|---|---|
| `graph` | Named nodes, fixed and conditional edges, bounded loops, per-node patch commit, execution trace, checkpoint store interface | `agentlog` |
| `model` | Chat and embedding clients over six HTTP protocols, declared reasoning modes, retry and rate limit, tier-based `Router`, `RemoteClient` | `agentlog` |
| `agent` | Independent parts (`ChatAgent`, `EmbeddingAgent`, `RAGAgent`, `ToolBox`) and `Template`, which composes them under one audit session | `graph`, `model`, `agentlog` |
| `agentlog` | One audit session per call tree, redaction, and a verifiable SHA-256 chain | none |
| `llmrouter` | Serves `model.Router` over HTTP | `model`, `agentlog` |

Use only what you need. `graph` works without `agent` or `model`.

## Install

Requires Go 1.27.1 or newer. The repository is private, so tell Go not to use the public proxy:

```powershell
$env:GOPRIVATE = "github.com/Disesfgewu/*"
go get github.com/Disesfgewu/agentkit@latest
```

## Compose an agent

An agent is a set of independent parts. Each part does one thing, works without the others, and sits behind a small interface, so you can use one alone or replace it with your own.

| Part | Does one thing | Interface |
|---|---|---|
| `ChatAgent` | Applies a system prompt and calls a chat client | `Chatter` |
| `Indexer` | Chunks, embeds, and writes documents | `Ingester` |
| `VectorRetriever` | Embeds a query and searches an index | `Retriever` |
| `EmbeddingAgent` | An `Indexer` and a `VectorRetriever` sharing one embedder and store | `Ingester`, `Retriever` |
| `LocalRetriever` | Keyword search over loaded documents | `Retriever` |
| `RAGAgent` | Retrieves, answers, and validates citations | `Asker` |
| `ToolBox` | Calls registered tools | `ToolRunner` |
| `MemoryVectorStore` | Exact cosine search in memory | `VectorWriter`, `VectorSearcher` |

`agent.New` composes parts under one audit session. Every part is optional; calling a method whose part is missing returns an error.

```go
a := agent.New(agent.Config{
    Name:      "handbook",
    Chat:      agent.NewChatAgent(chatClient, "Answer only from the supplied evidence."),
    Retriever: agent.NewEmbeddingAgent(embedder, nil), // nil store uses MemoryVectorStore
})
defer a.Close()

a.Ingest(ctx, documents...)
result, err := a.Ask(ctx, "How many days of annual leave do employees get?", "")
```

Depend on the narrowest interface you need. A function that only chats takes an `agent.Chatter`, so it accepts a `ChatAgent`, a `Template`, or your own type. To use a vector database, implement `VectorWriter` and `VectorSearcher`. [`examples/rag`](examples/rag/main.go) runs offline, or against real providers with `-router`.

Parts used alone need no `Template`:

```go
retriever := agent.VectorRetriever{Embedder: embedder, Searcher: myDatabase, MinScore: 0.3}
documents, err := retriever.Retrieve(ctx, "refund policy", 5)
```

Two embedding rules that keep retrieval correct:

- Vectors from different models cannot be compared. An index accepts one model, and a query or write from another fails with `ErrEmbeddingMismatch`. Router embeddings never fail over, for the same reason.
- `MemoryVectorStore` does not survive a restart. Without `MinScore`, retrieval always returns the nearest documents, however unrelated, so set it when an agent must be able to report insufficient evidence.

## Quickstart

```powershell
go run ./examples/quickstart
```

`agent.Template` also works with flat fields (`Model`, `SystemPrompt`, `Tools`). [`examples/quickstart`](examples/quickstart/main.go) is a complete support-ticket agent that needs no provider account. The LLM classifies a ticket, a plain function picks the route, and the graph calls a tool or searches documents before replying:

```go
g := graph.NewContext(ctx)
g.AddNodeContext(ctx, "classify", classify)
g.AddNodeContext(ctx, "lookup_order", lookupOrder)
g.AddNodeContext(ctx, "search_docs", searchDocs)
g.AddNodeContext(ctx, "reply", reply)
g.EdgeContext(ctx, graph.Start, "classify")
g.ConditionalContext(ctx, "classify", route, map[string]string{"refund": "lookup_order", "question": "search_docs"})
g.EdgeContext(ctx, "lookup_order", "reply")
g.EdgeContext(ctx, "search_docs", "reply")
g.EdgeContext(ctx, "reply", graph.End)
result, err := g.Run(ctx, graph.State{"ticket": ticket})
```

A node receives a copy of the state and returns a patch, which is committed only if the node succeeds. The copy is top-level, so treat nested values as immutable. `MaxSteps` defaults to 32; raise it for long-running loops and set `RetainPatches = false` to bound memory.

Graph limits: no reducers, parallel supersteps, or interrupts. The included `MemoryStore` checkpoint does not survive a restart; implement `graph.Store` for durable resume.

## Models

Every provider implements `model.Client` for chat, and `model.Embedder` where it has an embedding API, so application code does not depend on one.

| Protocol | Chat endpoint | Embedding endpoint | Reasoning modes |
|---|---|---|---|
| `openai_responses` | `/responses` | `/embeddings` | `openai_effort` |
| `openai_chat` | `/chat/completions` | `/embeddings` | none |
| `nim` | `/chat/completions` | `/embeddings` | `nim_enable_thinking`, `nim_thinking_mode` |
| `google` | `/models/{model}:generateContent` | `/models/{model}:batchEmbedContents` | `google_level`, `google_budget` |
| `anthropic` | `/v1/messages` | none | `anthropic_adaptive`, `anthropic_budget` |
| `ollama` | `/api/chat` | `/api/embed` | `ollama_think` |

Reasoning is declared per model in the configuration, because the provider alone does not determine it. A model with an incompatible mode fails before the HTTP call. HTTP mock tests verify request and response handling; they do not prove that a given live model accepts a mode. Copy `models.example.json` and `.env.example`, then fill in only the providers you use. Mark embedding models with `"kind": "embedding"`.

One HTTP attempt times out after `timeout_seconds`, three minutes by default. Keep it above your model's slowest normal reply: a timeout retries a request the provider may still bill, without ever returning the answer.

## LLMRouter service

`cmd/llmrouter` lets agents share one quota, one set of provider credentials, and one failover policy. Agents use `model.RemoteClient`, so graph nodes do not change.

```powershell
go run ./cmd/llmrouter -config models.example.json -env .env -addr 127.0.0.1:8787
```

| Endpoint | Purpose |
|---|---|
| `POST /v1/chat` | Body is `model.Request`, reply is `model.Response` with the provider and model actually used |
| `POST /v1/embeddings` | Body is `model.EmbeddingRequest`, reply is `model.EmbeddingResponse` |
| `GET /v1/models` | Lists loaded models without endpoints or credential names |
| `GET /healthz` | Liveness, no token required |

- Models whose environment is unset are skipped and printed at startup. Set `requests_per_minute` and `max_retries` per model; the limit is shared by every agent.
- Set `model` in the request to pin one entry by name. A pinned request never fails over; use this when results must stay comparable.
- A non-loopback `-addr` requires a bearer token in `LLMROUTER_TOKEN`.
- `-request-timeout` bounds retries and failover per request and must stay below `model.RemoteTimeout`.
- The router writes its own hash-chained audit log. It does not yet share call IDs with the calling agent's log.

## Audit log

`agent.Template` owns the session. The same recorder follows `context.Context` through the template, hooks, graph nodes and routers, checkpoints, retrieval, provider calls, and tools. Each completed call writes one record with parent ID, token usage, timing, and a hash link.

```powershell
go run ./cmd/agentctl audit-verify -log log/your-agent_2026-10-23-16-28-13.000000000.log
```

- Known secret field names such as `api_key` and `authorization` are redacted. Prompts, documents, and tool inputs are retained on purpose, so the log directory needs access control.
- The chain detects modification, reordering, and deletion inside the file. Proving the tail was not truncated needs an external signed final hash.
- Go cannot intercept arbitrary functions. Wrap your own with `agentlog.Call(ctx, "package.Function", input, fn)` and pass the supplied context along.
- For HTTP or custom operations, use `template.Runtime()`. It reuses the template's logger; `Runtime.JSON` records HTTP calls and `Runtime.Execute` records custom operations.

## CLI

```powershell
go run ./cmd/agentctl chat -env .env -model ollama -system-prompt "You are a helpful agent." -q "Hello"
go run ./cmd/agentctl ask -env .env -model openai -docs docs/a.md,docs/b.md -q "What does the policy say?"
```
