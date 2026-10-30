package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Disesfgewu/agentkit/agentlog"
	"io"
	"net/http"
	"net/url"
	"strings"
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
	return response, c.post(ctx, "/v1/chat", r, &response)
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
	return response, c.post(ctx, "/v1/embeddings", r, &response)
}

func (c *RemoteClient) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &failure)
		return fmt.Errorf("llmrouter HTTP %d: %s", resp.StatusCode, failure.Error)
	}
	return json.Unmarshal(raw, out)
}
