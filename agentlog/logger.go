package agentlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

type Recorder struct {
	mu        sync.Mutex
	file      *os.File
	AgentName string
	Path      string
}

func New(agentName, dir string) (*Recorder, error) {
	if agentName == "" {
		agentName = "agent"
	}
	if dir == "" {
		dir = "log"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	safe := unsafeName.ReplaceAllString(agentName, "_")
	stamp := time.Now().Format("2006-01-02-15-04-05")
	path := filepath.Join(dir, safe+"_"+stamp+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &Recorder{file: f, AgentName: agentName, Path: path}, nil
}
func sensitive(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"api_key", "apikey", "authorization", "password", "secret", "credential", "access_token", "refresh_token"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}
func redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, value := range x {
			if sensitive(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = redact(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = redact(value)
		}
		return out
	default:
		return v
	}
}
func safeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `"[UNSERIALIZABLE]"`
	}
	var raw any
	if json.Unmarshal(b, &raw) != nil {
		return `"[UNSERIALIZABLE]"`
	}
	clean, _ := json.Marshal(redact(raw))
	return string(clean)
}
func (r *Recorder) Record(function string, input, output any, tokens *int, elapsed time.Duration, err error) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tokenText := "?"
	if tokens != nil {
		tokenText = fmt.Sprint(*tokens)
	}
	result := output
	if err != nil {
		result = map[string]any{"error": err.Error()}
	}
	line := fmt.Sprintf("[DEBUG] agent:%s, func:%s(input:%s), return:%s, token:%s, response_time:%s, time:%s\n", r.AgentName, function, safeJSON(input), safeJSON(result), tokenText, elapsed.Round(time.Millisecond), time.Now().Format(time.RFC3339Nano))
	_, e := r.file.WriteString(line)
	return e
}
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}
