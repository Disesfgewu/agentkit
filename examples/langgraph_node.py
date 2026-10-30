"""Optional Python LangGraph bridge; install langgraph separately.

Build the Go binary once: go build -o agentctl.exe ./cmd/agentctl
Run from the repository root with a local .env and configured model.
"""
import json
import subprocess
from typing import TypedDict

from langgraph.graph import END, START, StateGraph


class State(TypedDict):
    question: str
    answer: str
    evidence: list[dict]


def go_agent_node(state: State) -> dict:
    process = subprocess.run(
        ["./agentctl.exe", "ask", "-env", ".env", "-config", "models.example.json",
         "-model", "openai", "-q", state["question"]],
        check=True, capture_output=True, text=True, timeout=120,
    )
    result = json.loads(process.stdout)["state"]
    return {"answer": result["answer"], "evidence": result["documents"]}


builder = StateGraph(State)
builder.add_node("go_agent", go_agent_node)
builder.add_edge(START, "go_agent")
builder.add_edge("go_agent", END)
graph = builder.compile()
