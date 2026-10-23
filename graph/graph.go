package graph

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
}

func New() *Graph { return &Graph{nodes: map[string]Node{}, edges: map[string]edge{}, MaxSteps: 32} }
func (g *Graph) AddNode(name string, fn Node) error {
	if name == Start || name == End || name == "" || fn == nil {
		return errors.New("invalid node")
	}
	if _, ok := g.nodes[name]; ok {
		return fmt.Errorf("duplicate node %s", name)
	}
	g.nodes[name] = fn
	return nil
}
func (g *Graph) Edge(from, to string) { g.edges[from] = edge{to: to} }
func (g *Graph) Conditional(from string, route Router, choices map[string]string) {
	g.edges[from] = edge{route: route, allowed: maps.Clone(choices)}
}
func (g *Graph) Compile() error {
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
	if s == nil {
		return State{}
	}
	return maps.Clone(s)
}
func (g *Graph) Run(ctx context.Context, initial State) (Result, error) {
	return g.RunFrom(ctx, initial, Start, nil)
}
func (g *Graph) RunFrom(ctx context.Context, initial State, from string, history []Step) (Result, error) {
	return g.run(ctx, initial, from, history, nil)
}
func (g *Graph) run(ctx context.Context, initial State, from string, history []Step, onCommit func(Checkpoint) error) (Result, error) {
	if err := g.Compile(); err != nil {
		return Result{}, err
	}
	s := clone(initial)
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
			var err error
			route, err = e.route(clone(s))
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
				if err := onCommit(Checkpoint{State: s, Steps: steps, LastNode: End}); err != nil {
					return Result{State: s, Steps: steps}, err
				}
			}
			return Result{State: s, Steps: steps, Finished: true}, nil
		}
		patch, err := g.nodes[next](ctx, clone(s))
		if err != nil {
			return Result{State: s, Steps: steps}, fmt.Errorf("node %s: %w", next, err)
		}
		for k, v := range patch {
			s[k] = v
		}
		steps = append(steps, Step{Node: next, Route: route, Patch: clone(patch)})
		current = next
		if onCommit != nil {
			if err := onCommit(Checkpoint{State: s, Steps: steps, LastNode: current}); err != nil {
				return Result{State: s, Steps: steps}, err
			}
		}
	}
	return Result{State: s, Steps: steps}, fmt.Errorf("graph exceeded %d steps", g.MaxSteps)
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
func (g *Graph) RunCheckpointed(ctx context.Context, threadID string, initial State, store Store) (Result, error) {
	if store == nil {
		return Result{}, errors.New("checkpoint store required")
	}
	checkpoint, found, err := store.Load(threadID)
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
