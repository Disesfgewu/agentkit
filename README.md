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
| `envelope` | The one response format between services: typed endpoint wrapper, matching client, event stream, automatic audit on both ends | `agentlog` |

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
- `MemoryVectorStore` does not survive a restart. Without `MinScore`, retrieval always returns the nearest documents, however unrelated, and they can leak into the answer. Set it when an agent must be able to report insufficient evidence. Useful values depend on the embedding model, so measure your own scores.

`Ask` counts a citation only when the answer contains the document ID in square brackets, such as `[leave]`, so an ID that is an ordinary word does not validate by being mentioned. Set `CitationMatch` to change this.

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
| `POST /v1/chat` | Body is `model.Request`; `data` is `model.Response` with the provider and model actually used |
| `POST /v1/embeddings` | Body is `model.EmbeddingRequest`; `data` is `model.EmbeddingResponse` |
| `GET /v1/models` | `data.models` lists loaded models without endpoints or credential names |
| `GET /healthz` | Liveness, no token required, not audited |

Every reply is an [envelope](#envelope), so read the payload from `data` and failures from `error`. `model.RemoteClient` unwraps it for Go callers. A client in another language must do the same: check `ok`, then use `data`. Router failures use these codes: `unauthorized` (401), `messages_required` and `input_required` (400), `model_unknown` (404), `no_eligible_model` (422), `timeout` (504), `upstream_failed` (502).

- Models whose environment is unset are skipped and printed at startup. Set `requests_per_minute` and `max_retries` per model; the limit is shared by every agent.
- Set `model` in the request to pin one entry by name. A pinned request never fails over; use this when results must stay comparable.
- A non-loopback `-addr` requires a bearer token in `LLMROUTER_TOKEN`.
- `-request-timeout` bounds retries and failover per request and must stay below `model.RemoteTimeout`.
- The router writes its own hash-chained audit log. Each request records the caller's session and call ID, and the caller records the router's receipt, so the two logs can be joined.

## Envelope

Services talk to each other in one response format, and `envelope` is the only code that writes or reads it. Success and failure have the same shape, so a caller unwraps every reply the same way:

```json
{"version":"1","ok":true,"code":"ok","message":"","data":{"text":"hello"},"error":null,
 "meta":{"audit":{"session":"router_2026-11-16-...","call_id":7,"seq":12,"hash":"9f2c..."},"t":"2026-11-16T02:00:00Z"}}
```

On failure `ok` is false, `data` is null, and `error` is a problem object whose `type`, `title`, `status`, `detail`, and `instance` are the RFC 9457 members, plus a stable `code` to branch on, `retryable`, and optional field `errors`. The HTTP status always equals `error.status`.

```go
// Server: a typed function becomes an endpoint.
mux.Handle("POST /v1/answer", envelope.Handle(server, "qa.Answer", func(ctx context.Context, q Question) (Answer, error) {
    envelope.Emit(ctx, "step.finished", map[string]any{"step": "retrieve"}) // sent only to streaming clients
    return answer(ctx, q)
}))

// Client: one call returns the typed data or a *envelope.Problem.
out, meta, err := envelope.Call[Answer](ctx, client, "POST", "/v1/answer", Question{Text: "..."})

// Or follow progress; the same endpoint serves both.
out, meta, err := envelope.Collect[Answer](envelope.Stream(ctx, client, "POST", "/v1/answer", q), func(e envelope.Event) error {
    return nil
})
```

What the wrapper guarantees, so endpoint code does not have to:

- **Nothing escapes the format.** A returned error, a panic, an invalid or oversized body, a timeout, a refused connection, a proxy error page, and a reply from a service that does not use the envelope all reach the caller as a `*envelope.Problem`. Use `Server.Map` to give your own errors a status and code.
- **Audit is automatic on both ends.** Each request is one audit call on the server and one on the client. The server records the caller's session and call ID; the client records the server's receipt from `meta.audit`, so the two hash chains reference each other.
- **Audit before work, and before release.** If the audit session cannot record the request, the function does not run. A result is sent only after its completion record is durable, and `meta.audit.hash` names that record.
- **A stream is one audit call.** Events are not logged one by one; the completion record on each side holds the event count and a SHA-256 over them, and the client rejects a stream whose count or digest differs from what the server reports. A stream that ends without a result, or skips a sequence number, is a failure.
- **Leaving the loop cancels.** Breaking out of `range envelope.Stream(...)` closes the connection, cancels the handler's context, and closes both audit calls. With `iter.Pull2`, always call `stop`, or `Recorder.Close` waits forever.

Limits: `data` is null on failure, so partial output is kept in the audit record only. Events are sent as given, without the audit log's redaction, so do not put secrets in them. Field names containing `token`, `secret`, `password`, `credential`, or `authorization` are redacted in the audit record; name fields accordingly (for example `next_cursor`, not `next_page_token`). There is no resume after a broken stream: re-issue the request. `Client.HTTP` must not set a total `Timeout`, because it also cuts off a stream that is still delivering; bound a call with its context.

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
