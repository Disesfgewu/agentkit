package model

import (
	"context"
	"errors"
	"testing"
)

type stubClient struct {
	response Response
	err      error
	calls    *int
}

func (s stubClient) Chat(_ context.Context, _ Request) (Response, error) {
	*s.calls++
	return s.response, s.err
}
func TestRouterFailoverAndContextFit(t *testing.T) {
	first, second := 0, 0
	r := Router{Entries: []Entry{{Config: Config{Name: "small", Tier: 0, ContextTokens: 2}, Client: stubClient{calls: &first}}, {Config: Config{Name: "fallback", Tier: 1, ContextTokens: 100}, Client: stubClient{response: Response{Text: "ok"}, calls: &second}}}}
	out, err := r.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "longer than two tokens"}}, MaxOutputTokens: 2})
	if err != nil || out.Text != "ok" || first != 0 || second != 1 {
		t.Fatalf("router %+v %v %d/%d", out, err, first, second)
	}
	a, b := 0, 0
	r = Router{Entries: []Entry{{Config: Config{Name: "a"}, Client: stubClient{err: errors.New("unavailable"), calls: &a}}, {Config: Config{Name: "b"}, Client: stubClient{response: Response{Text: "backup"}, calls: &b}}}}
	out, err = r.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil || out.Text != "backup" || a != 1 || b != 1 {
		t.Fatalf("failover %+v %v %d/%d", out, err, a, b)
	}
}
