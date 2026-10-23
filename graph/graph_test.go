package graph

import (
	"context"
	"errors"
	"testing"
)

func TestConditionalAndAtomicFailure(t *testing.T) {
	g := New()
	g.AddNode("ok", func(_ context.Context, s State) (State, error) { return State{"count": 1}, nil })
	g.AddNode("bad", func(_ context.Context, s State) (State, error) {
		s["count"] = 99
		return State{"leak": true}, errors.New("failure")
	})
	g.Edge(Start, "ok")
	g.Conditional("ok", func(s State) (string, error) { return "fail", nil }, map[string]string{"fail": "bad", "done": End})
	r, err := g.Run(context.Background(), State{"count": 0})
	if err == nil || r.State["count"] != 1 || r.State["leak"] != nil || len(r.Steps) != 1 {
		t.Fatalf("unexpected transaction: %+v %v", r, err)
	}
}
func TestLoopBound(t *testing.T) {
	g := New()
	g.MaxSteps = 3
	g.AddNode("loop", func(_ context.Context, s State) (State, error) { return State{"n": s["n"].(int) + 1}, nil })
	g.Edge(Start, "loop")
	g.Edge("loop", "loop")
	r, err := g.Run(context.Background(), State{"n": 0})
	if err == nil || r.State["n"] != 3 {
		t.Fatalf("unexpected bound: %+v %v", r, err)
	}
}
func TestCheckpointResume(t *testing.T) {
	g := New()
	firstCalls := 0
	secondCalls := 0
	g.AddNode("first", func(_ context.Context, _ State) (State, error) { firstCalls++; return State{"value": 1}, nil })
	g.AddNode("second", func(_ context.Context, s State) (State, error) {
		secondCalls++
		if secondCalls == 1 {
			return nil, errors.New("temporary")
		}
		return State{"value": s["value"].(int) + 1}, nil
	})
	g.Edge(Start, "first")
	g.Edge("first", "second")
	g.Edge("second", End)
	store := NewMemoryStore()
	_, err := g.RunCheckpointed(context.Background(), "thread", nil, store)
	if err == nil {
		t.Fatal("expected first failure")
	}
	r, err := g.RunCheckpointed(context.Background(), "thread", nil, store)
	if err != nil || !r.Finished || r.State["value"] != 2 || firstCalls != 1 || secondCalls != 2 {
		t.Fatalf("bad resume %+v %v calls=%d/%d", r, err, firstCalls, secondCalls)
	}
}
