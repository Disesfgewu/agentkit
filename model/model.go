package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agentlog"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type Request struct {
	Messages        []Message `json:"messages"`
	Reasoning       string    `json:"reasoning,omitempty"`
	MaxOutputTokens int       `json:"max_output_tokens,omitempty"`
}
type Response struct {
	Text         string `json:"text"`
	Reasoning    string `json:"reasoning,omitempty"`
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	UsageKnown   bool   `json:"-"`
}
type Client interface {
	Chat(context.Context, Request) (Response, error)
}

// ReasoningMode is a declared wire capability, not an inferred model property.
type Config struct {
	Name          string `json:"name"`
	Protocol      string `json:"protocol"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	APIKeyEnv     string `json:"api_key_env,omitempty"`
	ReasoningMode string `json:"reasoning_mode,omitempty"`
	ContextTokens int    `json:"context_tokens,omitempty"`
	Tier          int    `json:"tier,omitempty"`
}
type HTTPClient struct {
	Config Config
	HTTP   *http.Client
}

func New(c Config) (*HTTPClient, error) {
	return NewContext(context.Background(), c)
}
func NewContext(ctx context.Context, c Config) (client *HTTPClient, err error) {
	_, span := agentlog.Start(ctx, "model.New", c)
	defer func() { span.End(client, nil, err) }()
	if c.Model == "" || c.BaseURL == "" {
		return nil, errors.New("model and base_url required")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("base_url must be http(s)")
	}
	valid := map[string]bool{"openai_responses": true, "openai_chat": true, "nim": true, "google": true, "anthropic": true, "ollama": true}
	if !valid[c.Protocol] {
		return nil, fmt.Errorf("unsupported protocol %s", c.Protocol)
	}
	return &HTTPClient{Config: c, HTTP: &http.Client{Timeout: 60 * time.Second}}, nil
}
func (c *HTTPClient) Chat(ctx context.Context, r Request) (response Response, err error) {
	ctx, span := agentlog.Start(ctx, "model.HTTPClient.Chat", map[string]any{"provider": c.Config.Name, "protocol": c.Config.Protocol, "model": c.Config.Model, "request": r})
	defer func() {
		var tokens *int
		if response.UsageKnown {
			n := response.InputTokens + response.OutputTokens
			tokens = &n
		}
		span.End(response, tokens, err)
	}()
	if len(r.Messages) == 0 {
		return Response{}, errors.New("messages required")
	}
	body, path, headers, err := c.payload(ctx, r)
	if err != nil {
		return Response{}, err
	}
	key := ""
	if c.Config.APIKeyEnv != "" {
		key = os.Getenv(c.Config.APIKeyEnv)
		if key == "" {
			return Response{}, fmt.Errorf("missing API key environment variable %s", c.Config.APIKeyEnv)
		}
	}
	u := strings.TrimRight(c.Config.BaseURL, "/") + path
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	switch c.Config.Protocol {
	case "anthropic":
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "google":
		req.Header.Set("x-goog-api-key", key)
	case "ollama":
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	default:
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("%s HTTP %d (body withheld)", c.Config.Name, resp.StatusCode)
	}
	return c.parse(ctx, raw)
}
func (c *HTTPClient) payload(ctx context.Context, r Request) (body map[string]any, path string, headers map[string]string, err error) {
	_, span := agentlog.Start(ctx, "model.HTTPClient.payload", map[string]any{"protocol": c.Config.Protocol, "model": c.Config.Model, "request": r})
	defer func() { span.End(map[string]any{"body": body, "path": path, "headers": headers}, nil, err) }()
	p := c.Config.Protocol
	m := map[string]any{"model": c.Config.Model}
	h := map[string]string{}
	max := r.MaxOutputTokens
	if max <= 0 {
		max = 1024
	}
	switch p {
	case "openai_chat", "nim":
		m["messages"] = r.Messages
		m["max_tokens"] = max
	case "openai_responses":
		m["input"] = r.Messages
		m["max_output_tokens"] = max
	case "ollama":
		m["messages"] = r.Messages
		m["stream"] = false
		m["options"] = map[string]any{"num_predict": max}
	case "google":
		delete(m, "model")
		contents := []any{}
		var system []string
		for _, v := range r.Messages {
			if v.Role == "system" {
				system = append(system, v.Content)
				continue
			}
			role := "user"
			if v.Role == "assistant" {
				role = "model"
			}
			contents = append(contents, map[string]any{"role": role, "parts": []any{map[string]any{"text": v.Content}}})
		}
		m["contents"] = contents
		if len(system) > 0 {
			m["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": strings.Join(system, "\n")}}}
		}
		m["generationConfig"] = map[string]any{"maxOutputTokens": max}
	case "anthropic":
		msgs := []Message{}
		var system []string
		for _, v := range r.Messages {
			if v.Role == "system" {
				system = append(system, v.Content)
			} else {
				msgs = append(msgs, v)
			}
		}
		m["messages"] = msgs
		m["max_tokens"] = max
		if len(system) > 0 {
			m["system"] = strings.Join(system, "\n")
		}
	}
	if r.Reasoning != "" && r.Reasoning != "off" {
		mode := c.Config.ReasoningMode
		if mode == "" || mode == "none" {
			return nil, "", nil, fmt.Errorf("reasoning unsupported for %s", c.Config.Name)
		}
		switch mode {
		case "openai_effort":
			if p != "openai_responses" {
				return nil, "", nil, errors.New("openai_effort requires responses")
			}
			m["reasoning"] = map[string]any{"effort": r.Reasoning}
		case "ollama_think":
			if p != "ollama" {
				return nil, "", nil, errors.New("ollama_think requires ollama")
			}
			if r.Reasoning == "on" {
				m["think"] = true
			} else {
				m["think"] = r.Reasoning
			}
		case "google_level":
			if p != "google" {
				return nil, "", nil, errors.New("google_level requires google")
			}
			m["generationConfig"].(map[string]any)["thinkingConfig"] = map[string]any{"thinkingLevel": r.Reasoning}
		case "google_budget":
			if p != "google" {
				return nil, "", nil, errors.New("google_budget requires google")
			}
			var n int
			if _, err := fmt.Sscan(r.Reasoning, &n); err != nil || n < 0 {
				return nil, "", nil, errors.New("google budget must be nonnegative integer")
			}
			m["generationConfig"].(map[string]any)["thinkingConfig"] = map[string]any{"thinkingBudget": n}
		case "anthropic_adaptive":
			if p != "anthropic" {
				return nil, "", nil, errors.New("anthropic_adaptive requires anthropic")
			}
			m["thinking"] = map[string]any{"type": "adaptive"}
			m["output_config"] = map[string]any{"effort": r.Reasoning}
		case "anthropic_budget":
			if p != "anthropic" {
				return nil, "", nil, errors.New("anthropic_budget requires anthropic")
			}
			var n int
			if _, err := fmt.Sscan(r.Reasoning, &n); err != nil || n < 1 || n >= max {
				return nil, "", nil, errors.New("anthropic budget must be positive and below max output tokens")
			}
			m["thinking"] = map[string]any{"type": "enabled", "budget_tokens": n}
		case "nim_enable_thinking":
			if p != "nim" {
				return nil, "", nil, errors.New("nim_enable_thinking requires nim")
			}
			if r.Reasoning != "on" {
				return nil, "", nil, errors.New("nim reasoning accepts on only")
			}
			m["chat_template_kwargs"] = map[string]any{"enable_thinking": true}
		default:
			return nil, "", nil, fmt.Errorf("unknown reasoning mode %s", mode)
		}
	}
	switch p {
	case "openai_chat", "nim":
		return m, "/chat/completions", h, nil
	case "openai_responses":
		return m, "/responses", h, nil
	case "ollama":
		return m, "/api/chat", h, nil
	case "google":
		return m, "/models/" + url.PathEscape(c.Config.Model) + ":generateContent", h, nil
	case "anthropic":
		return m, "/v1/messages", h, nil
	}
	return nil, "", nil, errors.New("unsupported protocol")
}
func (c *HTTPClient) parse(ctx context.Context, raw []byte) (out Response, err error) {
	_, span := agentlog.Start(ctx, "model.HTTPClient.parse", map[string]any{"protocol": c.Config.Protocol, "response_bytes": len(raw)})
	defer func() {
		var tokens *int
		if out.UsageKnown {
			n := out.InputTokens + out.OutputTokens
			tokens = &n
		}
		span.End(out, tokens, err)
	}()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return Response{}, err
	}
	out = Response{Model: c.Config.Model, Provider: c.Config.Name}
	obj := func(x any) map[string]any { z, _ := x.(map[string]any); return z }
	arr := func(x any) []any { z, _ := x.([]any); return z }
	str := func(x any) string { z, _ := x.(string); return z }
	num := func(x any) int { z, _ := x.(float64); return int(z) }
	switch c.Config.Protocol {
	case "openai_chat", "nim":
		choices := arr(v["choices"])
		if len(choices) > 0 {
			msg := obj(obj(choices[0])["message"])
			out.Text = str(msg["content"])
			out.Reasoning = str(msg["reasoning_content"])
		}
	case "openai_responses":
		for _, x := range arr(v["output"]) {
			for _, part := range arr(obj(x)["content"]) {
				out.Text += str(obj(part)["text"])
			}
		}
	case "ollama":
		msg := obj(v["message"])
		out.Text = str(msg["content"])
		out.Reasoning = str(msg["thinking"])
	case "google":
		cs := arr(v["candidates"])
		if len(cs) > 0 {
			for _, part := range arr(obj(obj(cs[0])["content"])["parts"]) {
				p := obj(part)
				if p["thought"] == true {
					out.Reasoning += str(p["text"])
				} else {
					out.Text += str(p["text"])
				}
			}
		}
	case "anthropic":
		for _, part := range arr(v["content"]) {
			p := obj(part)
			if p["type"] == "thinking" {
				out.Reasoning += str(p["thinking"])
			} else if p["type"] == "text" {
				out.Text += str(p["text"])
			}
		}
	}
	usage := obj(v["usage"])
	switch c.Config.Protocol {
	case "openai_chat", "nim":
		out.InputTokens, out.OutputTokens = num(usage["prompt_tokens"]), num(usage["completion_tokens"])
	case "openai_responses", "anthropic":
		out.InputTokens, out.OutputTokens = num(usage["input_tokens"]), num(usage["output_tokens"])
	case "google":
		u := obj(v["usageMetadata"])
		out.InputTokens, out.OutputTokens = num(u["promptTokenCount"]), num(u["candidatesTokenCount"])
	case "ollama":
		out.InputTokens, out.OutputTokens = num(v["prompt_eval_count"]), num(v["eval_count"])
	}
	out.UsageKnown = out.InputTokens > 0 || out.OutputTokens > 0
	if out.Text == "" {
		return out, errors.New("provider returned no text")
	}
	return out, nil
}
