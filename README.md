# GSD traceable agent template client

A Go package for evidence retrieval, model calls, conditional state graphs, and replay of the existing GSD diagnostic validation set. This is a LangGraph-inspired Go runtime, not an official Go port of LangGraph. The optional [Python node bridge](examples/langgraph_node.py) runs the Go client inside a Python LangGraph workflow.

## Quick start

Requires Go 1.27.1 or newer. From this directory:

```powershell
go test ./...
go run ./cmd/agentctl benchmark
Copy-Item .env.example .env
# Fill only the provider/model values you intend to use in .env.
go run ./cmd/agentctl ask -env .env -model openai -q "What supports valve-stick diagnosis?" -knowledge ../generator/agent/knowledge.json
go run ./cmd/agentctl chat -env .env -model ollama -q "Hello"
```

For a remote Ollama device, set `OLLAMA_BASE_URL` to its reachable HTTP address. `models.example.json` uses environment expansion for endpoint and model names; copy and edit its `reasoning_mode` and `context_tokens` per *actual model capabilities*. Leave `-reasoning` unset for models without thinking support. Examples: `-reasoning high` for a compatible OpenAI Responses or Google level model; `-reasoning on` for an Ollama thinking model. A model with an incompatible mode fails before the HTTP call.

The `.env` file is ignored by Git. The CLI does not print API keys or upstream error bodies. It reads only the selected provider's key. Keep any local configuration containing secrets outside version control.

## API packages

- `graph`: named nodes, fixed and conditional edges, bounded loops, per-node patch commit, execution trace, checkpoint storage interface.
- `model`: six HTTP protocols, explicit reasoning wire modes, token estimate based router with failover.
- `agent`: composable template with `Ask`, `Chat`, `GetInformation`, `Triage`, `RunTool`, hooks, Markdown and GSD card retrieval.
- `agentlog`: automatic per-agent call logger with token count, response time, timestamp, and sensitive-key redaction.
- `gsd`: legacy JSONL reader, rule baseline, evaluator. Truth is read only by `benchmark` and `evaluate`; retrieval indexes knowledge cards and source claims only.

Go uses interfaces and embedding for polymorphism. Extend `agent.Template` through embedding, inject a `model.Client`, `agent.Retriever`, or `agent.Tool`, and use `graph.New()` to define domain-specific workflows. Tool execution is explicitly registered by the host application.

`agent.Template` creates `log/{agentName}_{YYYY-MM-DD-HH-MM-SS}.log` on its first operation (`Ask`, `Chat`, `GetInformation`, `Triage`, or `RunTool`). Each line uses `[DEBUG] agent:..., func:...(input:...), return:..., token:..., response_time:..., time:...`. Token is `?` when the provider does not report usage. Set `Name` and optionally `LogDir`, or use `-agent-name` and `-log-dir` in the CLI. Call `Close()` when finished. Keys with names such as `api_key`, `authorization`, `password`, and `secret` are redacted; prompts and responses themselves are logged as requested, so point `LogDir` at a protected location for sensitive production data.

To score predictions from another agent:

```powershell
go run ./cmd/agentctl evaluate -predictions path/to/predictions.jsonl
go run ./cmd/agentctl llm-eval -env .env -model nim -limit 8 -delay 10s -out ../.agent-artifacts/llm_predictions.jsonl
```

The benchmark action strings are diagnostic suggestions for synthetic data, not equipment control commands or fab SOPs.

See [technical report](TECHNICAL_REPORT.md) for protocol boundaries, experiment results, and limitations.
