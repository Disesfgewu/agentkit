// Package llmrouter exposes model.Router as an HTTP service, so agents share one
// quota, one set of provider credentials, and one failover policy.
package llmrouter

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Disesfgewu/agentkit/agentlog"
	"github.com/Disesfgewu/agentkit/envelope"
	"github.com/Disesfgewu/agentkit/model"
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
	Kind              string `json:"kind,omitempty"`
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

// Problem codes of the router, in addition to those of package envelope.
const (
	CodeUnauthorized     = "unauthorized"
	CodeMessagesRequired = "messages_required"
	CodeInputRequired    = "input_required"
	CodeModelUnknown     = "model_unknown"
	CodeNoEligibleModel  = "no_eligible_model"
	CodeUpstreamFailed   = "upstream_failed"
)

// ModelList is the data of GET /v1/models.
type ModelList struct {
	Models []ModelInfo `json:"models"`
}

// Handler serves the router. Every reply is an envelope.Envelope, and every
// request except the liveness probe is one audit call that records its caller.
func (s *Server) Handler() http.Handler {
	api := &envelope.Server{Recorder: s.options.Recorder, MaxBodyBytes: maxRequestBytes, Timeout: s.options.RequestTimeout}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		envelope.Write(w, envelope.Success(map[string]any{"ok": true}))
	})
	mux.Handle("GET /v1/models", s.authorized(envelope.Handle(api, "llmrouter.Server.models", s.models)))
	mux.Handle("POST /v1/chat", s.authorized(envelope.Handle(api, "llmrouter.Server.chat", s.chat)))
	mux.Handle("POST /v1/embeddings", s.authorized(envelope.Handle(api, "llmrouter.Server.embeddings", s.embeddings)))
	return mux
}

func (s *Server) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.options.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.options.Token)) != 1 {
				envelope.Write(w, envelope.Failure(envelope.NewProblem(http.StatusUnauthorized, CodeUnauthorized, "invalid router token")))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) models(context.Context, struct{}) (ModelList, error) {
	infos := make([]ModelInfo, 0, len(s.router.Entries))
	for _, e := range s.router.Entries {
		c := e.Config
		infos = append(infos, ModelInfo{c.Name, c.Protocol, c.Model, c.Tier, c.ContextTokens, c.ReasoningMode, c.RequestsPerMinute, c.Kind})
	}
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].Tier < infos[j].Tier })
	return ModelList{Models: infos}, nil
}

func (s *Server) chat(ctx context.Context, request model.Request) (model.Response, error) {
	if len(request.Messages) == 0 {
		return model.Response{}, envelope.NewProblem(http.StatusBadRequest, CodeMessagesRequired, "messages required")
	}
	response, err := s.router.Chat(ctx, request)
	return response, routerProblem(ctx, err)
}

func (s *Server) embeddings(ctx context.Context, request model.EmbeddingRequest) (model.EmbeddingResponse, error) {
	if len(request.Input) == 0 {
		return model.EmbeddingResponse{}, envelope.NewProblem(http.StatusBadRequest, CodeInputRequired, "input required")
	}
	response, err := s.router.Embed(ctx, request)
	return response, routerProblem(ctx, err)
}

// routerProblem reads the request deadline directly: a provider's own transport
// timeout also matches context.DeadlineExceeded but is an upstream failure.
func routerProblem(ctx context.Context, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, model.ErrUnknownModel):
		return envelope.NewProblem(http.StatusNotFound, CodeModelUnknown, err.Error()).Wrap(err)
	case errors.Is(err, model.ErrNoEligibleModel):
		return envelope.NewProblem(http.StatusUnprocessableEntity, CodeNoEligibleModel, err.Error()).Wrap(err)
	case ctx.Err() != nil:
		return envelope.NewProblem(http.StatusGatewayTimeout, envelope.CodeTimeout, err.Error()).Wrap(err)
	}
	return envelope.NewProblem(http.StatusBadGateway, CodeUpstreamFailed, err.Error()).Wrap(err)
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
