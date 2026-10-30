package llmrouter

import (
	"context"
	"encoding/json"
	"github.com/Disesfgewu/agentkit/agentlog"
	"github.com/Disesfgewu/agentkit/model"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var hello = model.Request{Messages: []model.Message{{Role: "user", Content: "hi"}}}

// upstream fakes an OpenAI-compatible provider and counts its calls.
func upstream(t *testing.T, status int, text string) (string, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + text + `"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	}))
	t.Cleanup(s.Close)
	return s.URL, calls
}

func entry(t *testing.T, name, url string, tier int) model.Entry {
	t.Helper()
	config := model.Config{Name: name, Protocol: "openai_chat", BaseURL: url, Model: name + "-model", Tier: tier, MaxRetries: -1}
	client, err := model.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return model.Entry{Config: config, Client: client}
}

// start serves entries and verifies the router's audit chain when the test ends.
func start(t *testing.T, options Options, entries ...model.Entry) *httptest.Server {
	t.Helper()
	rec := options.Recorder
	if rec == nil {
		rec = newRecorder(t)
		options.Recorder = rec
	}
	server, err := New(entries, options)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		s.Close()
		if err := rec.Close(); err != nil {
			t.Error(err)
		}
		if _, err := agentlog.Verify(rec.Path); err != nil {
			t.Errorf("audit chain: %v", err)
		}
	})
	return s
}

func newRecorder(t *testing.T) *agentlog.Recorder {
	t.Helper()
	rec, err := agentlog.New("llmrouter-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func remote(t *testing.T, url, token, pinned string) *model.RemoteClient {
	t.Helper()
	client, err := model.NewRemote(url, token, pinned)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestRemoteReportsActualProviderAndUsage(t *testing.T) {
	url, _ := upstream(t, 200, "ok")
	rec := newRecorder(t)
	// Cleanups run last-registered first, so this sees the closed audit log.
	t.Cleanup(func() {
		if records, err := agentlog.Verify(rec.Path); err != nil || records == 0 {
			t.Errorf("router must audit chat calls: records=%d err=%v", records, err)
		}
	})
	s := start(t, Options{Recorder: rec}, entry(t, "primary", url, 0))
	out, err := remote(t, s.URL, "", "").Chat(context.Background(), hello)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "ok" || out.Provider != "primary" || out.Model != "primary-model" {
		t.Fatalf("identity lost: %+v", out)
	}
	if !out.UsageKnown || out.InputTokens != 7 || out.OutputTokens != 3 || out.Attempts != 1 {
		t.Fatalf("usage lost: %+v", out)
	}
}

func TestFailoverUnlessPinned(t *testing.T) {
	broken, brokenCalls := upstream(t, 400, "")
	healthy, healthyCalls := upstream(t, 200, "backup")
	s := start(t, Options{}, entry(t, "healthy", healthy, 1), entry(t, "broken", broken, 0))
	out, err := remote(t, s.URL, "", "").Chat(context.Background(), hello)
	if err != nil || out.Provider != "healthy" || brokenCalls.Load() != 1 || healthyCalls.Load() != 1 {
		t.Fatalf("failover %+v %v %d/%d", out, err, brokenCalls.Load(), healthyCalls.Load())
	}
	_, err = remote(t, s.URL, "", "broken").Chat(context.Background(), hello)
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") || healthyCalls.Load() != 1 {
		t.Fatalf("pinned model must not fail over: %v calls=%d", err, healthyCalls.Load())
	}
	_, err = remote(t, s.URL, "", "missing").Chat(context.Background(), hello)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("unknown pin: %v", err)
	}
}

func TestRemoteDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer s.Close()
	if _, err := remote(t, s.URL, "", "").Chat(context.Background(), hello); err == nil || calls.Load() != 1 {
		t.Fatalf("expected one failed call: %v calls=%d", err, calls.Load())
	}
}

func TestTokenProtectsEverythingButHealth(t *testing.T) {
	url, _ := upstream(t, 200, "ok")
	s := start(t, Options{Token: "secret"}, entry(t, "primary", url, 0))
	if _, err := remote(t, s.URL, "wrong", "").Chat(context.Background(), hello); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := remote(t, s.URL, "secret", "").Chat(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{"/healthz": 200, "/v1/models": 401} {
		resp, err := http.Get(s.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s status %d", path, resp.StatusCode)
		}
	}
}

func TestModelsListingHidesEndpointsAndCredentials(t *testing.T) {
	url, _ := upstream(t, 200, "ok")
	e := entry(t, "primary", url, 0)
	e.Config.APIKeyEnv = "SECRET_KEY_NAME"
	s := start(t, Options{}, e)
	resp, err := http.Get(s.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Models) != 1 || body.Models[0]["name"] != "primary" {
		t.Fatalf("models %+v", body.Models)
	}
	for _, leaked := range []string{"base_url", "api_key_env"} {
		if _, ok := body.Models[0][leaked]; ok {
			t.Errorf("listing exposes %s", leaked)
		}
	}
}

func TestRequestValidationAndDeadline(t *testing.T) {
	// The server only notices a client disconnect once the body has been read.
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer hang.Close()
	s := start(t, Options{RequestTimeout: 50 * time.Millisecond}, entry(t, "slow", hang.URL, 0))
	if _, err := remote(t, s.URL, "", "").Chat(context.Background(), model.Request{}); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("empty messages: %v", err)
	}
	if _, err := remote(t, s.URL, "", "").Chat(context.Background(), hello); err == nil || !strings.Contains(err.Error(), "HTTP 504") {
		t.Fatalf("deadline: %v", err)
	}
}

func TestRequireToken(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:8787": true, "localhost:8787": true, "[::1]:8787": true, ":8787": false, "0.0.0.0:8787": false, "10.0.0.5:8787": false} {
		if err := RequireToken(addr, ""); (err == nil) != ok {
			t.Errorf("%s: %v", addr, err)
		}
		if err := RequireToken(addr, "secret"); err != nil {
			t.Errorf("%s with token: %v", addr, err)
		}
	}
}

func TestLoadEntriesSkipsUnconfiguredModels(t *testing.T) {
	t.Setenv("ROUTER_TEST_URL", "https://example.test")
	t.Setenv("ROUTER_TEST_KEY", "value")
	t.Setenv("ROUTER_TEST_MISSING_KEY", "")
	path := filepath.Join(t.TempDir(), "models.json")
	write := func(content string) {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`[
	  {"name":"ready","protocol":"nim","base_url":"${ROUTER_TEST_URL}","model":"m","api_key_env":"ROUTER_TEST_KEY"},
	  {"name":"no-key","protocol":"nim","base_url":"${ROUTER_TEST_URL}","model":"m","api_key_env":"ROUTER_TEST_MISSING_KEY"},
	  {"name":"no-url","protocol":"ollama","base_url":"${ROUTER_TEST_UNSET}","model":"m"}
	]`)
	entries, skipped, err := LoadEntries(context.Background(), path)
	if err != nil || len(entries) != 1 || entries[0].Config.Name != "ready" || strings.Join(skipped, ",") != "no-key,no-url" {
		t.Fatalf("entries=%+v skipped=%v err=%v", entries, skipped, err)
	}
	write(`[{"name":"a","protocol":"ollama","base_url":"http://x","model":"m"},{"name":"a","protocol":"ollama","base_url":"http://x","model":"m"}]`)
	if _, _, err := LoadEntries(context.Background(), path); err == nil {
		t.Fatal("duplicate names must fail")
	}
}
