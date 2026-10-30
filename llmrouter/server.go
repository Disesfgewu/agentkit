// Package llmrouter exposes model.Router as an HTTP service, so agents share one
// quota, one set of provider credentials, and one failover policy.
package llmrouter

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agentlog"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/model"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultRequestTimeout bounds retries and failover for one request. It must
	// stay below model.RemoteTimeout.
	DefaultRequestTimeout = 10 * time.Minute
	maxRequestBytes       = 16 << 20
)

type Options struct {
	Token          string
	RequestTimeout time.Duration
	Recorder       *agentlog.Recorder
}

type Server struct {
	router  model.Router
	options Options
}

type ModelInfo struct {
	Name              string `json:"name"`
	Protocol          string `json:"protocol"`
	Model             string `json:"model"`
	Tier              int    `json:"tier"`
	ContextTokens     int    `json:"context_tokens,omitempty"`
	ReasoningMode     string `json:"reasoning_mode,omitempty"`
	RequestsPerMinute int    `json:"requests_per_minute,omitempty"`
}

func New(entries []model.Entry, options Options) (*Server, error) {
	if len(entries) == 0 {
		return nil, errors.New("no models configured")
	}
	if options.Recorder == nil {
		return nil, errors.New("audit recorder required")
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = DefaultRequestTimeout
	}
	return &Server{router: model.Router{Entries: entries}, options: options}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /v1/models", s.authorized(s.models))
	mux.HandleFunc("POST /v1/chat", s.authorized(s.chat))
	return mux
}

func (s *Server) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.options.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.options.Token)) != 1 {
				writeError(w, http.StatusUnauthorized, errors.New("invalid router token"))
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	infos := make([]ModelInfo, 0, len(s.router.Entries))
	for _, e := range s.router.Entries {
		c := e.Config
		infos = append(infos, ModelInfo{c.Name, c.Protocol, c.Model, c.Tier, c.ContextTokens, c.ReasoningMode, c.RequestsPerMinute})
	}
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].Tier < infos[j].Tier })
	writeJSON(w, http.StatusOK, map[string]any{"models": infos})
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var request model.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		return
	}
	if len(request.Messages) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("messages required"))
		return
	}
	ctx, cancel := context.WithTimeout(agentlog.WithRecorder(r.Context(), s.options.Recorder), s.options.RequestTimeout)
	defer cancel()
	response, err := s.router.Chat(ctx, request)
	if err != nil {
		writeError(w, chatStatus(ctx, err), err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// chatStatus reads the request deadline directly: a provider's own transport
// timeout also matches context.DeadlineExceeded but is an upstream failure.
func chatStatus(ctx context.Context, err error) int {
	switch {
	case errors.Is(err, model.ErrUnknownModel):
		return http.StatusNotFound
	case errors.Is(err, model.ErrNoEligibleModel):
		return http.StatusUnprocessableEntity
	case ctx.Err() != nil:
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// RequireToken refuses to expose provider credentials beyond loopback without
// authentication.
func RequireToken(addr, token string) error {
	if token != "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("listening on %s requires a router token", addr)
}

// LoadEntries builds one client per configured model. Models whose environment
// is not set are reported as skipped, so one template can list every provider.
func LoadEntries(ctx context.Context, path string) (entries []model.Entry, skipped []string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var configs []model.Config
	if err := json.Unmarshal([]byte(os.ExpandEnv(string(b))), &configs); err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, c := range configs {
		if seen[c.Name] {
			return nil, nil, fmt.Errorf("duplicate model name %q", c.Name)
		}
		seen[c.Name] = true
		if c.BaseURL == "" || c.Model == "" || c.APIKeyEnv != "" && os.Getenv(c.APIKeyEnv) == "" {
			skipped = append(skipped, c.Name)
			continue
		}
		client, err := model.NewContext(ctx, c)
		if err != nil {
			return nil, nil, fmt.Errorf("model %q: %w", c.Name, err)
		}
		entries = append(entries, model.Entry{Config: c, Client: client})
	}
	return entries, skipped, nil
}
