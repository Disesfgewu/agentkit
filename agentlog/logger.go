package agentlog

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
var sequencePattern = regexp.MustCompile(`, seq:([0-9]+),`)

type Recorder struct {
	mu        sync.Mutex
	file      *os.File
	AgentName string
	Path      string
	SessionID string
	seq       uint64
	callSeq   atomic.Uint64
	lastHash  string
}

type contextKey struct{}
type callKey struct{}
type Span struct {
	recorder         *Recorder
	function         string
	input            any
	started          time.Time
	callID, parentID uint64
	once             sync.Once
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
	stamp := time.Now().Format("2006-01-02-15-04-05.000000000")
	path := filepath.Join(dir, safe+"_"+stamp+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &Recorder{file: f, AgentName: agentName, Path: path, SessionID: safe + "_" + stamp}, nil
}

// WithRecorder attaches one audit session to the full downstream call tree.
func WithRecorder(ctx context.Context, recorder *Recorder) context.Context {
	return context.WithValue(ctx, contextKey{}, recorder)
}
func FromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(contextKey{}).(*Recorder)
	return r
}

// Start writes a START record and returns a child context carrying its call ID.
func Start(ctx context.Context, function string, input any) (context.Context, *Span) {
	r := FromContext(ctx)
	if r == nil {
		return ctx, &Span{function: function, input: input, started: time.Now()}
	}
	parent, _ := ctx.Value(callKey{}).(uint64)
	id := r.callSeq.Add(1)
	s := &Span{recorder: r, function: function, input: input, started: time.Now(), callID: id, parentID: parent}
	_ = r.write("START", function, input, nil, nil, 0, nil, id, parent)
	return context.WithValue(ctx, callKey{}, id), s
}
func (s *Span) End(output any, tokens *int, err error) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.recorder != nil {
			_ = s.recorder.write("END", s.function, s.input, output, tokens, time.Since(s.started), err, s.callID, s.parentID)
		}
	})
}

// Call is the audit wrapper for extension functions implemented outside this module.
func Call[T any](ctx context.Context, function string, input any, fn func(context.Context) (T, error)) (output T, err error) {
	ctx, span := Start(ctx, function, input)
	defer func() { span.End(output, nil, err) }()
	return fn(ctx)
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
	return r.write("END", function, input, output, tokens, elapsed, err, r.callSeq.Add(1), 0)
}
func (r *Recorder) write(phase, function string, input, output any, tokens *int, elapsed time.Duration, err error, callID, parentID uint64) error {
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
	r.seq++
	now := time.Now().Format(time.RFC3339Nano)
	body := fmt.Sprintf("agent:%s, func:%s(input:%s), return:%s, token:%s, response_time:%s, time:%s, session:%s, seq:%d, call_id:%d, parent_call_id:%d, phase:%s, prev_hash:%s", r.AgentName, function, safeJSON(input), safeJSON(result), tokenText, elapsed.Round(time.Millisecond), now, r.SessionID, r.seq, callID, parentID, phase, r.lastHash)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	r.lastHash = hash
	if _, e := r.file.WriteString("[DEBUG] " + body + ", hash:" + hash + "\n"); e != nil {
		return e
	}
	return r.file.Sync()
}
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}

// Verify checks sequence continuity and the SHA-256 chain of an audit file.
func Verify(path string) (records int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	previous := ""
	expected := uint64(1)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "[DEBUG] ") {
			return records, fmt.Errorf("line %d: invalid prefix", records+1)
		}
		content := strings.TrimPrefix(line, "[DEBUG] ")
		at := strings.LastIndex(content, ", hash:")
		if at < 0 {
			return records, fmt.Errorf("line %d: missing hash", records+1)
		}
		body, actual := content[:at], content[at+7:]
		calculated := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
		if actual != calculated {
			return records, fmt.Errorf("line %d: hash mismatch", records+1)
		}
		if !strings.HasSuffix(body, "prev_hash:"+previous) {
			return records, fmt.Errorf("line %d: chain mismatch", records+1)
		}
		match := sequencePattern.FindStringSubmatch(body)
		if len(match) != 2 {
			return records, fmt.Errorf("line %d: missing sequence", records+1)
		}
		seq, e := strconv.ParseUint(match[1], 10, 64)
		if e != nil || seq != expected {
			return records, fmt.Errorf("line %d: sequence mismatch", records+1)
		}
		previous = actual
		expected++
		records++
	}
	if err := scanner.Err(); err != nil {
		return records, err
	}
	return records, nil
}
