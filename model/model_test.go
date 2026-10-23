package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProtocols(t *testing.T) {
	cases := []struct{ protocol, mode, reason, path, reply string }{
		{"openai_chat", "", "", "/chat/completions", `{"choices":[{"message":{"content":"ok"}}]}`},
		{"openai_responses", "openai_effort", "high", "/responses", `{"output":[{"content":[{"type":"output_text","text":"ok"}]}]}`},
		{"nim", "nim_enable_thinking", "on", "/chat/completions", `{"choices":[{"message":{"content":"ok","reasoning_content":"thought"}}]}`},
		{"google", "google_level", "high", "/models/test:generateContent", `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`},
		{"anthropic", "anthropic_adaptive", "high", "/v1/messages", `{"content":[{"type":"text","text":"ok"}]}`},
		{"ollama", "ollama_think", "on", "/api/chat", `{"message":{"content":"ok","thinking":"thought"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Errorf("path %s", r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if tc.reason != "" {
					if tc.mode == "openai_effort" && body["reasoning"] == nil {
						t.Error("missing reasoning")
					}
					if tc.mode == "ollama_think" && body["think"] == nil {
						t.Error("missing think")
					}
				}
				w.Write([]byte(tc.reply))
			}))
			defer s.Close()
			c, err := New(Config{Name: tc.protocol, Protocol: tc.protocol, BaseURL: s.URL, Model: "test", ReasoningMode: tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}, Reasoning: tc.reason})
			if err != nil || r.Text != "ok" {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}
func TestUnsupportedReasoning(t *testing.T) {
	c, _ := New(Config{Name: "plain", Protocol: "openai_chat", BaseURL: "http://localhost", Model: "test"})
	_, err := c.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}, Reasoning: "high"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatal(err)
	}
}
