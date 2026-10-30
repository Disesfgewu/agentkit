package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbeddingProtocols(t *testing.T) {
	cases := []struct {
		protocol, path, reply string
		check                 func(map[string]any) bool
		tokens                int
	}{
		{"openai_chat", "/embeddings", `{"data":[{"index":1,"embedding":[3,4]},{"index":0,"embedding":[1,2]}],"usage":{"prompt_tokens":5}}`,
			func(b map[string]any) bool { return b["input_type"] == nil && len(b["input"].([]any)) == 2 }, 5},
		{"nim", "/embeddings", `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[3,4]}]}`,
			func(b map[string]any) bool { return b["input_type"] == "passage" }, 0},
		{"google", "/models/test:batchEmbedContents", `{"embeddings":[{"values":[1,2]},{"values":[3,4]}]}`,
			func(b map[string]any) bool {
				item := b["requests"].([]any)[0].(map[string]any)
				return item["model"] == "models/test" && item["taskType"] == "RETRIEVAL_DOCUMENT"
			}, 0},
		{"ollama", "/api/embed", `{"embeddings":[[1,2],[3,4]],"prompt_eval_count":9}`,
			func(b map[string]any) bool { return len(b["input"].([]any)) == 2 }, 9},
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.URL.Path != tc.path || !tc.check(body) {
					t.Errorf("path %s body %v", r.URL.Path, body)
				}
				w.Write([]byte(tc.reply))
			}))
			defer s.Close()
			c, err := New(Config{Name: "e", Protocol: tc.protocol, BaseURL: s.URL, Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Embed(context.Background(), EmbeddingRequest{Input: []string{"a", "b"}, InputType: InputDocument})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Vectors) != 2 || out.Vectors[0][0] != 1 || out.Vectors[1][1] != 4 {
				t.Fatalf("vectors out of order: %+v", out.Vectors)
			}
			if out.Model != "test" || out.Provider != "e" || out.Attempts != 1 || out.InputTokens != tc.tokens {
				t.Fatalf("metadata %+v", out)
			}
		})
	}
}

func TestEmbedRejectsUnsupportedProtocolAndShortReply(t *testing.T) {
	c, _ := New(Config{Name: "a", Protocol: "anthropic", BaseURL: "http://x", Model: "m"})
	if _, err := c.Embed(context.Background(), EmbeddingRequest{Input: []string{"a"}}); err == nil {
		t.Fatal("anthropic has no embedding API")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer s.Close()
	c, _ = New(Config{Name: "o", Protocol: "openai_chat", BaseURL: s.URL, Model: "m"})
	if _, err := c.Embed(context.Background(), EmbeddingRequest{Input: []string{"a", "b"}}); err == nil {
		t.Fatal("a short reply must not misalign vectors with chunks")
	}
}

type stubEmbedClient struct {
	stubClient
	embeds *int
	err    error
}

func (s stubEmbedClient) Embed(context.Context, EmbeddingRequest) (EmbeddingResponse, error) {
	*s.embeds++
	return EmbeddingResponse{Vectors: [][]float32{{1}}, Model: "m"}, s.err
}

func TestRouterEmbedNeverFailsOverAndChatSkipsEmbeddingModels(t *testing.T) {
	first, second, chats := 0, 0, 0
	r := Router{Entries: []Entry{
		{Config: Config{Name: "e1", Kind: KindEmbedding, Tier: 0}, Client: stubEmbedClient{stubClient: stubClient{calls: &chats}, embeds: &first, err: errors.New("down")}},
		{Config: Config{Name: "e2", Kind: KindEmbedding, Tier: 1}, Client: stubEmbedClient{stubClient: stubClient{calls: &chats}, embeds: &second}},
	}}
	ctx := context.Background()
	if _, err := r.Embed(ctx, EmbeddingRequest{Input: []string{"x"}}); err == nil || first != 1 || second != 0 {
		t.Fatalf("must not fail over: %v %d/%d", err, first, second)
	}
	if _, err := r.Embed(ctx, EmbeddingRequest{Input: []string{"x"}, Model: "e2"}); err != nil || second != 1 {
		t.Fatalf("pin: %v %d", err, second)
	}
	if _, err := r.Embed(ctx, EmbeddingRequest{Input: []string{"x"}, Model: "nope"}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := r.Chat(ctx, Request{Messages: []Message{{Role: "user", Content: "x"}}}); !errors.Is(err, ErrNoEligibleModel) || chats != 0 {
		t.Fatalf("chat must skip embedding entries: %v %d", err, chats)
	}
}

func TestTimeoutIsConfigurable(t *testing.T) {
	c, err := New(Config{Name: "a", Protocol: "openai_chat", BaseURL: "http://x", Model: "m"})
	if err != nil || c.HTTP.Timeout != DefaultHTTPTimeout {
		t.Fatalf("default timeout %v %v", c.HTTP.Timeout, err)
	}
	c, err = New(Config{Name: "a", Protocol: "openai_chat", BaseURL: "http://x", Model: "m", TimeoutSeconds: 7})
	if err != nil || c.HTTP.Timeout.Seconds() != 7 {
		t.Fatalf("configured timeout %v %v", c.HTTP.Timeout, err)
	}
}
