package model

import (
	"context"
	"errors"
	"fmt"
	"github.com/Disesfgewu/agentkit/agentlog"
	"github.com/Disesfgewu/agentkit/envelope"
	"net/http"
	"net/url"
	"time"
)

// RemoteTimeout must exceed the LLMRouter request timeout so that the router,
// not the caller, reports an exhausted retry and failover budget.
const RemoteTimeout = 11 * time.Minute

// RemoteClient calls an LLMRouter service. It never retries: the router owns
// retries, failover, and quota, and a second retry layer would multiply calls.
type RemoteClient struct {
	BaseURL string
	Token   string
	Model   string
	// EmbeddingModel pins the router entry used by Embed.
	EmbeddingModel string
	HTTP           *http.Client
}

func NewRemote(baseURL, token, pinned string) (*RemoteClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("router base_url must be http(s)")
	}
	return &RemoteClient{BaseURL: baseURL, Token: token, Model: pinned, HTTP: &http.Client{Timeout: RemoteTimeout}}, nil
}

func (c *RemoteClient) Chat(ctx context.Context, r Request) (response Response, err error) {
	if r.Model == "" {
		r.Model = c.Model
	}
	ctx, span := agentlog.Start(ctx, "model.RemoteClient.Chat", r)
	defer span.Finish(&err, func() any { return response }, func() *int {
		if response.UsageKnown {
			n := response.InputTokens + response.OutputTokens
			return &n
		}
		return nil
	})
	if err = span.Err(); err != nil {
		return
	}
	return post[Response](ctx, c, "/v1/chat", r)
}

// Embed never reuses the chat pin: EmbeddingModel names the embedding entry.
func (c *RemoteClient) Embed(ctx context.Context, r EmbeddingRequest) (response EmbeddingResponse, err error) {
	if r.Model == "" {
		r.Model = c.EmbeddingModel
	}
	ctx, span := agentlog.Start(ctx, "model.RemoteClient.Embed", embeddingInput(r))
	defer span.Finish(&err, func() any { return embeddingOutput(response) }, nil)
	if err = span.Err(); err != nil {
		return
	}
	return post[EmbeddingResponse](ctx, c, "/v1/embeddings", r)
}

// post calls the router through the shared envelope client, which unwraps the
// reply and records the router's audit receipt in the caller's session. The
// error is a *envelope.Problem wrapped with the router's name.
func post[Out any](ctx context.Context, c *RemoteClient, path string, in any) (Out, error) {
	client := &envelope.Client{BaseURL: c.BaseURL, Bearer: c.Token, HTTP: c.HTTP, MaxBodyBytes: 8 << 20}
	out, _, err := envelope.Call[Out](ctx, client, "POST", path, in)
	if err != nil {
		return out, fmt.Errorf("llmrouter: %w", err)
	}
	return out, nil
}
