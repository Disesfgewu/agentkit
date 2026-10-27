# GSD traceable agent template client

**Audit review status (2026-10-27): not ready for complete-audit acceptance.** Thirteen fault-injection cases reproduce silent audit loss, lifecycle/ownership defects, missing panic records and incomplete inputs/outputs. Earlier successful live runs verify only their exercised paths. See the [engineering review and reproduction commands](../docs/fleet-http/07_AUTOLOGGER_REVIEW.md). Production fixes are still outstanding.

The existing `agent.Template` owns automatic logging. Use `template.Runtime()` for HTTP/custom operations; it reuses `Template.recorder()` and the same logger as `Template.Chat`. `Runtime.JSON` records HTTP calls automatically; `Runtime.Execute` runs custom operations with one completion and nested call IDs. Applications do not call `Recorder.Record`. `NewRuntime` is only a convenience constructor delegating to Template; it does not implement a separate logging system. An empty logDir defaults to `log/`. See the [real-call rerun and audit reconciliation](../docs/fleet-http/06_PACKAGE_AUTOLOG.md).

A Go package for evidence retrieval, model calls, conditional state graphs, and replay of the existing GSD diagnostic validation set. This is a LangGraph-inspired Go runtime, not an official Go port of LangGraph. The optional [Python node bridge](examples/langgraph_node.py) runs the Go client inside a Python LangGraph workflow.

## Quick start

Requires Go 1.27.1 or newer. From this directory:

```powershell
go test ./...
go run ./cmd/agentctl benchmark
Copy-Item .env.example .env
# Fill only the provider/model values you intend to use in .env.
go run ./cmd/agentctl ask -env .env -model openai -q "What supports valve-stick diagnosis?" -knowledge ../generator/agent/knowledge.json
go run ./cmd/agentctl chat -env .env -model ollama -system-prompt "You are a traceable equipment agent." -q "Hello"
```

For a remote Ollama device, set `OLLAMA_BASE_URL` to its reachable HTTP address. `models.example.json` uses environment expansion for endpoint and model names; copy and edit its `reasoning_mode` and `context_tokens` per *actual model capabilities*. Leave `-reasoning` unset for models without thinking support. Examples: `-reasoning high` for a compatible OpenAI Responses or Google level model; `-reasoning on` for an Ollama thinking model. A model with an incompatible mode fails before the HTTP call.

The `.env` file is ignored by Git. The CLI does not print API keys or upstream error bodies. It reads only the selected provider's key. Keep any local configuration containing secrets outside version control.

## API packages

- `graph`: named nodes, fixed and conditional edges, bounded loops, per-node patch commit, execution trace, checkpoint storage interface.
- `model`: six HTTP protocols, explicit reasoning wire modes, token estimate based router with failover.
- `agent`: composable template with `Ask`, `Chat`, `GetInformation`, `Triage`, `RunTool`, hooks, Markdown and GSD card retrieval.
- `agentlog`: one audit session shared by every framework function in the call tree, with one completion record per invocation, parent relationships, token usage, timing, redaction, and a verifiable hash chain.
- `gsd`: legacy JSONL reader, rule baseline, evaluator. Truth is read only by `benchmark` and `evaluate`; retrieval indexes knowledge cards and source claims only.

Go uses interfaces and embedding for polymorphism. Extend `agent.Template` through embedding, inject a `model.Client`, `agent.Retriever`, or `agent.Tool`, and use `graph.New()` to define domain-specific workflows. Tool execution is explicitly registered by the host application.

The system prompt is a first-class agent property used by both `Ask` and `Chat`:

```go
equipmentAgent := &agent.Template{
    Name:         "etch-triage",
    SystemPrompt: "You are a traceable semiconductor equipment triage agent. Never invent thresholds or SOPs.",
    Model:        routedModel,
    Retriever:    evidenceRetriever,
    Tools:        registeredTools,
}
defer equipmentAgent.Close()
```

`Ask` appends its evidence and citation contract to this prompt. `Chat` sends the configured prompt directly before caller messages. `BuildMessages` is public for derived agent types that need to inspect or extend prompt construction. The effective prompt and final provider messages are included in the audit log.

`agent.Template` and every CLI command create one `log/{agentName}_{YYYY-MM-DD-HH-MM-SS.nanoseconds}.log` audit session. The same recorder follows `context.Context` through the template, hooks, graph construction and execution, every node and conditional router, checkpoint operation, RAG retrieval and tokenization, provider router, HTTP payload/response parsing, GSD matching/decisions/evaluation, and registered tool boundary. Each completed function invocation writes one record; `call_id` and `parent_call_id` retain nesting without duplicate START/END lines:

```text
[DEBUG] agent:gsd-agent, func:model.HTTPClient.Chat(input:{...}), return:{...}, token:210, response_time:526ms, time:..., session:..., seq:42, call_id:9, parent_call_id:3, phase:END, prev_hash:..., hash:...
```

Every record is flushed to disk. `seq` provides total ordering, call IDs reconstruct nesting, and the SHA-256 chain detects modification, reordering, and deletion inside the retained file. An external signed final hash is still needed to prove that the tail was not truncated. Verify a file with:

Completion-only logging avoids serializing the same large state twice. A process terminated inside an unfinished function has no completion record; production crash evidence should also come from the process supervisor/runtime log.

```powershell
go run ./cmd/agentctl audit-verify -log log/gsd-agent_2026-10-23-16-28-13.000000000.log
```

Token is `?` for functions or providers without reported usage. Known secret field names such as `api_key`, `authorization`, `password`, and `secret` are recursively redacted. Prompts, retrieved documents, tool inputs, and outputs are deliberately retained for audit, so the log directory requires production access control and retention policy.

Go cannot intercept arbitrary functions compiled in another package. Framework functions are already instrumented. Extension code should preserve the supplied context and use `agentlog.Call(ctx, "package.Function", input, fn)` around each internal extension function; the registered tool boundary is logged automatically even if a tool has not added internal spans. Logger serialization, redaction, and file-writing internals are excluded from self-logging to prevent recursive logging.

To score predictions from another agent:

```powershell
go run ./cmd/agentctl evaluate -predictions path/to/predictions.jsonl
go run ./cmd/agentctl llm-eval -env .env -model nim -limit 8 -delay 10s -out ../.agent-artifacts/llm_predictions.jsonl
```

The benchmark action strings are diagnostic suggestions for synthetic data, not equipment control commands or fab SOPs.

See [technical report](TECHNICAL_REPORT.md) for protocol boundaries, actual 10 Hz raw numeric rows, wafer-feature tables around a fault onset, Agent JSON previews, field dictionaries, experiment results, and limitations.

To inspect the Generator dataset as full CSV files rather than Agent JSON, run from `generator/`:

```powershell
go run ./cmd/dsbuild -template benchmarks/templates/agent_preview.yaml -out runs/agent_preview -workers 4
go run ./cmd/dsexportcsv -data runs/agent_preview -split test -out runs/agent_preview_csv/test -raw=true
```

The measured preview export contains 1,125,900 raw numeric rows in `raw_values.csv`, plus complete feature, event, truth, and frame-metadata CSV files.
