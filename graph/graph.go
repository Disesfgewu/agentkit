package graph

import (
	"context"
	"errors"
	"fmt"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agentlog"
	"maps"
	"sort"
	"sync"
)

const (
	Start = "__start__"
	End   = "__end__"
)

type State map[string]any
type Node func(context.Context, State) (State, error)
type Router func(State) (string, error)
type Step struct {
	Node  string `json:"node"`
	Route string `json:"route,omitempty"`
	Patch State  `json:"patch,omitempty"`
}
type Result struct {
	State    State  `json:"state"`
	Steps    []Step `json:"steps"`
	Finished bool   `json:"finished"`
}
type edge struct {
	to      string
	route   Router
	allowed map[string]string
}
type Graph struct {
	nodes    map[string]Node
	edges    map[string]edge
	MaxSteps int
	// RetainPatches controls whether Step history copies each node patch.
	// Disable it for long-running workflows that already retain final State.
	RetainPatches bool
}

func New() *Graph { return NewContext(context.Background()) }
func NewContext(ctx context.Context) (g *Graph) {
	_, span := agentlog.Start(ctx, "graph.New", nil)
	defer func() { span.End(map[string]any{"max_steps": g.MaxSteps}, nil, nil) }()
	return &Graph{nodes: map[string]Node{}, edges: map[string]edge{}, MaxSteps: 32, RetainPatches: true}
}
func (g *Graph) AddNode(name string, fn Node) error {
	return g.AddNodeContext(context.Background(), name, fn)
}
func (g *Graph) AddNodeContext(ctx context.Context, name string, fn Node) (err error) {
	_, span := agentlog.Start(ctx, "graph.Graph.AddNode", map[string]any{"name": name})
	defer func() { span.End(map[string]any{"registered": err == nil}, nil, err) }()
	if name == Start || name == End || name == "" || fn == nil {
		return errors.New("invalid node")
	}
	if _, ok := g.nodes[name]; ok {
		return fmt.Errorf("duplicate node %s", name)
	}
	g.nodes[name] = fn
	return nil
}
func (g *Graph) Edge(from, to string) { g.EdgeContext(context.Background(), from, to) }
func (g *Graph) EdgeContext(ctx context.Context, from, to string) {
	_, span := agentlog.Start(ctx, "graph.Graph.Edge", map[string]any{"from": from, "to": to})
	defer span.End(map[string]any{"registered": true}, nil, nil)
	g.edges[from] = edge{to: to}
}
func (g *Graph) Conditional(from string, route Router, choices map[string]string) {
	g.ConditionalContext(context.Background(), from, route, choices)
}
func (g *Graph) ConditionalContext(ctx context.Context, from string, route Router, choices map[string]string) {
	_, span := agentlog.Start(ctx, "graph.Graph.Conditional", map[string]any{"from": from, "choices": choices})
	defer span.End(map[string]any{"registered": true}, nil, nil)
	g.edges[from] = edge{route: route, allowed: maps.Clone(choices)}
}
func (g *Graph) Compile() error {
	return g.compile(context.Background())
}
func (g *Graph) compile(ctx context.Context) (err error) {
	_, span := agentlog.Start(ctx, "graph.Graph.Compile", map[string]any{"nodes": len(g.nodes), "edges": len(g.edges), "max_steps": g.MaxSteps})
	defer func() { span.End(map[string]any{"valid": err == nil}, nil, err) }()
	if g.MaxSteps < 1 {
		return errors.New("MaxSteps must be positive")
	}
	if _, ok := g.edges[Start]; !ok {
		return errors.New("missing start edge")
	}
	for from, e := range g.edges {
		if from != Start {
			if _, ok := g.nodes[from]; !ok {
				return fmt.Errorf("unknown source %s", from)
			}
		}
		targets := []string{e.to}
		if e.route != nil {
			if len(e.allowed) == 0 {
				return fmt.Errorf("empty routes on %s", from)
			}
			targets = nil
			for _, to := range e.allowed {
				targets = append(targets, to)
			}
		}
		for _, to := range targets {
			if to != End {
				if _, ok := g.nodes[to]; !ok {
					return fmt.Errorf("unknown target %s", to)
				}
			}
		}
	}
	return nil
}
func clone(s State) State {
	return cloneContext(context.Background(), s)
}
func cloneContext(_ context.Context, s State) (result State) {
	if s == nil {
		return State{}
	}
	return maps.Clone(s)
}
func (g *Graph) Run(ctx context.Context, initial State) (result Result, err error) {
	ctx, span := agentlog.Start(ctx, "graph.Graph.Run", stateSummary(initial))
	defer func() { span.End(resultSummary(result), nil, err) }()
	return g.RunFrom(ctx, initial, Start, nil)
}
func (g *Graph) RunFrom(ctx context.Context, initial State, from string, history []Step) (result Result, err error) {
	ctx, span := agentlog.Start(ctx, "graph.Graph.RunFrom", map[string]any{"initial": stateSummary(initial), "from": from, "history_steps": len(history)})
	defer func() { span.End(resultSummary(result), nil, err) }()
	return g.run(ctx, initial, from, history, nil)
}
func (g *Graph) run(ctx context.Context, initial State, from string, history []Step, onCommit func(Checkpoint) error) (result Result, err error) {
	ctx, span := agentlog.Start(ctx, "graph.Graph.run", map[string]any{"initial": stateSummary(initial), "from": from, "history_steps": len(history), "checkpointed": onCommit != nil})
	defer func() { span.End(resultSummary(result), nil, err) }()
	if err := g.compile(ctx); err != nil {
		return Result{}, err
	}
	s := cloneContext(ctx, initial)
	steps := append([]Step(nil), history...)
	current := from
	for i := len(history); i < g.MaxSteps; i++ {
		if err := ctx.Err(); err != nil {
			return Result{State: s, Steps: steps}, err
		}
		e, ok := g.edges[current]
		if !ok {
			return Result{State: s, Steps: steps}, fmt.Errorf("no outgoing edge from %s", current)
		}
		next := e.to
		route := ""
		if e.route != nil {
			routeCtx, routeSpan := agentlog.Start(ctx, "graph.Router."+current, stateSummary(s))
			var err error
			route, err = e.route(cloneContext(routeCtx, s))
			_ = routeCtx
			routeSpan.End(map[string]any{"route": route}, nil, err)
			if err != nil {
				return Result{State: s, Steps: steps}, err
			}
			next, ok = e.allowed[route]
			if !ok {
				return Result{State: s, Steps: steps}, fmt.Errorf("invalid route %s from %s", route, current)
			}
		}
		if next == End {
			if onCommit != nil {
				commitCtx, commitSpan := agentlog.Start(ctx, "graph.Store.Save", map[string]any{"last_node": End, "steps": len(steps)})
				err := onCommit(Checkpoint{State: s, Steps: steps, LastNode: End})
				_ = commitCtx
				commitSpan.End(map[string]any{"saved": err == nil}, nil, err)
				if err != nil {
					return Result{State: s, Steps: steps}, err
				}
			}
			return Result{State: s, Steps: steps, Finished: true}, nil
		}
		nodeCtx, nodeSpan := agentlog.Start(ctx, "graph.Node."+next, stateSummary(s))
		patch, err := g.nodes[next](nodeCtx, cloneContext(nodeCtx, s))
		nodeSpan.End(patch, nil, err)
		if err != nil {
			return Result{State: s, Steps: steps}, fmt.Errorf("node %s: %w", next, err)
		}
		for k, v := range patch {
			s[k] = v
		}
		step := Step{Node: next, Route: route}
		if g.RetainPatches {
			step.Patch = cloneContext(ctx, patch)
		}
		steps = append(steps, step)
		current = next
		if onCommit != nil {
			commitCtx, commitSpan := agentlog.Start(ctx, "graph.Store.Save", map[string]any{"last_node": current, "steps": len(steps)})
			err := onCommit(Checkpoint{State: s, Steps: steps, LastNode: current})
			_ = commitCtx
			commitSpan.End(map[string]any{"saved": err == nil}, nil, err)
			if err != nil {
				return Result{State: s, Steps: steps}, err
			}
		}
	}
	return Result{State: s, Steps: steps}, fmt.Errorf("graph exceeded %d steps", g.MaxSteps)
}

func stateSummary(s State) map[string]any {
	keys := make([]string, 0, len(s))
	for key := range s {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	summary := map[string]any{"keys": keys, "key_count": len(keys)}
	for _, key := range []string{"record_id", "document_count", "retrieval_attempts", "source_valid", "processed", "has_new_data", "decision_mode"} {
		if value, ok := s[key]; ok {
			summary[key] = value
		}
	}
	return summary
}

func resultSummary(result Result) map[string]any {
	return map[string]any{"finished": result.Finished, "step_count": len(result.Steps), "state": stateSummary(result.State)}
}

type Checkpoint struct {
	State    State  `json:"state"`
	Steps    []Step `json:"steps"`
	LastNode string `json:"last_node"`
}
type Store interface {
	Save(string, Checkpoint) error
	Load(string) (Checkpoint, bool, error)
}

// RunCheckpointed resumes after the last committed node for a thread.
func (g *Graph) RunCheckpointed(ctx context.Context, threadID string, initial State, store Store) (result Result, err error) {
	ctx, span := agentlog.Start(ctx, "graph.Graph.RunCheckpointed", map[string]any{"thread_id": threadID, "initial": initial})
	defer func() { span.End(result, nil, err) }()
	if store == nil {
		return Result{}, errors.New("checkpoint store required")
	}
	loadCtx, loadSpan := agentlog.Start(ctx, "graph.Store.Load", map[string]any{"thread_id": threadID})
	checkpoint, found, err := store.Load(threadID)
	_ = loadCtx
	loadSpan.End(map[string]any{"checkpoint": checkpoint, "found": found}, nil, err)
	if err != nil {
		return Result{}, err
	}
	if found {
		if checkpoint.LastNode == End {
			return Result{State: checkpoint.State, Steps: checkpoint.Steps, Finished: true}, nil
		}
		return g.run(ctx, checkpoint.State, checkpoint.LastNode, checkpoint.Steps, func(c Checkpoint) error { return store.Save(threadID, c) })
	}
	return g.run(ctx, initial, Start, nil, func(c Checkpoint) error { return store.Save(threadID, c) })
}

type MemoryStore struct {
	mu    sync.RWMutex
	items map[string]Checkpoint
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{items: map[string]Checkpoint{}} }
func (m *MemoryStore) Save(id string, c Checkpoint) error {
	if id == "" {
		return errors.New("empty thread id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c.State = clone(c.State)
	c.Steps = append([]Step(nil), c.Steps...)
	m.items[id] = c
	return nil
}
func (m *MemoryStore) Load(id string) (Checkpoint, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.items[id]
	c.State = clone(c.State)
	c.Steps = append([]Step(nil), c.Steps...)
	return c, ok, nil
}
