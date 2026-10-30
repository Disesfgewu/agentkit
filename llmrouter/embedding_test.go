package llmrouter

import (
	"context"
	"github.com/Disesfgewu/agentkit/model"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddingsThroughRouter(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.5,0.25]}],"usage":{"prompt_tokens":4}}`))
	}))
	defer up.Close()
	config := model.Config{Name: "embed", Kind: model.KindEmbedding, Protocol: "openai_chat", BaseURL: up.URL, Model: "embed-model", MaxRetries: -1}
	client, err := model.New(config)
	if err != nil {
		t.Fatal(err)
	}
	s := start(t, Options{}, model.Entry{Config: config, Client: client})
	remote := remote(t, s.URL, "", "")
	out, err := remote.Embed(context.Background(), model.EmbeddingRequest{Input: []string{"hello"}, InputType: model.InputQuery})
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "embed-model" || out.Provider != "embed" || !out.UsageKnown || out.InputTokens != 4 || out.Vectors[0][1] != 0.25 {
		t.Fatalf("embedding metadata lost: %+v", out)
	}
	if _, err := remote.Embed(context.Background(), model.EmbeddingRequest{}); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("empty input: %v", err)
	}
	if _, err := remote.Chat(context.Background(), hello); err == nil || !strings.Contains(err.Error(), "HTTP 422") {
		t.Fatalf("chat must not reach an embedding-only router: %v", err)
	}
	remote.EmbeddingModel = "missing"
	if _, err := remote.Embed(context.Background(), model.EmbeddingRequest{Input: []string{"x"}}); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("unknown pin: %v", err)
	}
}
