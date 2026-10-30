package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Disesfgewu/agentkit/agentlog"
	"net/url"
	"sort"
)

const (
	// KindEmbedding marks a Config as an embedding model, which Router.Chat skips.
	KindEmbedding = "embedding"
	// InputQuery and InputDocument tell asymmetric models which side a text is on.
	InputQuery    = "query"
	InputDocument = "document"
)

type EmbeddingRequest struct {
	Input     []string `json:"input"`
	InputType string   `json:"input_type,omitempty"`
	// Model pins a Router entry by name.
	Model string `json:"model,omitempty"`
}

type EmbeddingResponse struct {
	Vectors     [][]float32 `json:"vectors"`
	Model       string      `json:"model"`
	Provider    string      `json:"provider"`
	InputTokens int         `json:"input_tokens,omitempty"`
	UsageKnown  bool        `json:"usage_known,omitempty"`
	Attempts    int         `json:"attempts,omitempty"`
}

// Embedder turns texts into vectors. Vectors from different models cannot be
// compared, so callers must keep one model per index.
type Embedder interface {
	Embed(context.Context, EmbeddingRequest) (EmbeddingResponse, error)
}

// Audit records keep counts, not vectors or corpus text, to bound log size.
func embeddingInput(r EmbeddingRequest) map[string]any {
	chars := 0
	for _, text := range r.Input {
		chars += len(text)
	}
	return map[string]any{"count": len(r.Input), "chars": chars, "input_type": r.InputType, "model": r.Model}
}

func embeddingOutput(r EmbeddingResponse) map[string]any {
	dimensions := 0
	if len(r.Vectors) > 0 {
		dimensions = len(r.Vectors[0])
	}
	return map[string]any{"count": len(r.Vectors), "dimensions": dimensions, "model": r.Model, "provider": r.Provider, "input_tokens": r.InputTokens, "attempts": r.Attempts}
}

// Embed supports the OpenAI-compatible, NIM, Google, and Ollama embedding APIs.
func (c *HTTPClient) Embed(ctx context.Context, r EmbeddingRequest) (response EmbeddingResponse, err error) {
	ctx, span := agentlog.Start(ctx, "model.HTTPClient.Embed", embeddingInput(r))
	defer span.Finish(&err, func() any { return embeddingOutput(response) }, nil)
	if err = span.Err(); err != nil {
		return
	}
	if len(r.Input) == 0 {
		return EmbeddingResponse{}, errors.New("input required")
	}
	body, path, err := c.embeddingPayload(r)
	if err != nil {
		return EmbeddingResponse{}, err
	}
	raw, attempts, err := c.post(ctx, path, body, nil)
	if err != nil {
		return EmbeddingResponse{}, err
	}
	response, err = c.parseEmbeddings(raw)
	if err != nil {
		return EmbeddingResponse{}, err
	}
	if len(response.Vectors) != len(r.Input) {
		return EmbeddingResponse{}, fmt.Errorf("%s returned %d vectors for %d inputs", c.Config.Name, len(response.Vectors), len(r.Input))
	}
	response.Model, response.Provider, response.Attempts = c.Config.Model, c.Config.Name, attempts
	return response, nil
}

func (c *HTTPClient) embeddingPayload(r EmbeddingRequest) (map[string]any, string, error) {
	switch c.Config.Protocol {
	case "openai_chat", "openai_responses":
		return map[string]any{"model": c.Config.Model, "input": r.Input}, "/embeddings", nil
	case "nim":
		body := map[string]any{"model": c.Config.Model, "input": r.Input}
		switch r.InputType {
		case InputQuery:
			body["input_type"] = "query"
		case InputDocument:
			body["input_type"] = "passage"
		}
		return body, "/embeddings", nil
	case "ollama":
		return map[string]any{"model": c.Config.Model, "input": r.Input}, "/api/embed", nil
	case "google":
		task := map[string]string{InputQuery: "RETRIEVAL_QUERY", InputDocument: "RETRIEVAL_DOCUMENT"}[r.InputType]
		requests := make([]any, 0, len(r.Input))
		for _, text := range r.Input {
			item := map[string]any{"model": "models/" + c.Config.Model, "content": map[string]any{"parts": []any{map[string]any{"text": text}}}}
			if task != "" {
				item["taskType"] = task
			}
			requests = append(requests, item)
		}
		return map[string]any{"requests": requests}, "/models/" + url.PathEscape(c.Config.Model) + ":batchEmbedContents", nil
	}
	return nil, "", fmt.Errorf("protocol %s has no embedding API", c.Config.Protocol)
}

func (c *HTTPClient) parseEmbeddings(raw []byte) (EmbeddingResponse, error) {
	var out EmbeddingResponse
	switch c.Config.Protocol {
	case "ollama":
		var v struct {
			Embeddings [][]float32 `json:"embeddings"`
			Tokens     int         `json:"prompt_eval_count"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return out, err
		}
		out.Vectors, out.InputTokens = v.Embeddings, v.Tokens
	case "google":
		var v struct {
			Embeddings []struct {
				Values []float32 `json:"values"`
			} `json:"embeddings"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return out, err
		}
		for _, e := range v.Embeddings {
			out.Vectors = append(out.Vectors, e.Values)
		}
	default:
		var v struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
			Usage struct {
				Prompt int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return out, err
		}
		sort.SliceStable(v.Data, func(i, j int) bool { return v.Data[i].Index < v.Data[j].Index })
		for _, d := range v.Data {
			out.Vectors = append(out.Vectors, d.Embedding)
		}
		out.InputTokens = v.Usage.Prompt
	}
	out.UsageKnown = out.InputTokens > 0
	return out, nil
}

// Embed never fails over: a second model would write vectors that cannot be
// compared with the existing index. Pin Model when several entries exist.
func (r Router) Embed(ctx context.Context, req EmbeddingRequest) (response EmbeddingResponse, err error) {
	ctx, span := agentlog.Start(ctx, "model.Router.Embed", embeddingInput(req))
	defer span.Finish(&err, func() any { return embeddingOutput(response) }, nil)
	if err = span.Err(); err != nil {
		return
	}
	var entries []Entry
	for _, e := range r.Entries {
		if e.Config.Kind == KindEmbedding && (req.Model == "" || e.Config.Name == req.Model) {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		if req.Model != "" {
			return EmbeddingResponse{}, fmt.Errorf("%w %q", ErrUnknownModel, req.Model)
		}
		return EmbeddingResponse{}, ErrNoEligibleModel
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Config.Tier < entries[j].Config.Tier })
	embedder, ok := entries[0].Client.(Embedder)
	if !ok {
		return EmbeddingResponse{}, fmt.Errorf("%w: %s cannot embed", ErrNoEligibleModel, entries[0].Config.Name)
	}
	return embedder.Embed(ctx, req)
}
