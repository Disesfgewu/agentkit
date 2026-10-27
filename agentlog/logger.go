package agentlog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrClosed = errors.New("audit session is closing or closed")
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
var sequencePattern = regexp.MustCompile(`, seq:([0-9]+),`)
var callPattern = regexp.MustCompile(`, call_id:([0-9]+), parent_call_id:([0-9]+),`)
var urls = regexp.MustCompile(`https?://[^\s"<>]+`)

type contextKey struct{}
type callKey struct{}
type effectiveTimeKey struct{}

type Recorder struct {
	mu                         sync.Mutex
	cond                       *sync.Cond
	file, journal              *os.File
	AgentName, Path, SessionID string
	seq, callSeq               uint64
	lastHash                   string
	writeErr, closeErr         error
	closing, closed            bool
	active                     map[uint64]bool
}
type Span struct {
	recorder         *Recorder
	function         string
	input            json.RawMessage
	started          time.Time
	effectiveAt      string
	callID, parentID uint64
	once             sync.Once
	own              bool
	startErr, endErr error
}
type beginRecord struct {
	CallID   uint64          `json:"call_id"`
	ParentID uint64          `json:"parent_call_id"`
	Function string          `json:"function"`
	Input    json.RawMessage `json:"input"`
	Time     time.Time       `json:"time"`
}
type seal struct {
	Records               uint64
	LastHash, JournalHash string
	Calls                 uint64
}

func New(name, dir string) (*Recorder, error) {
	if name == "" {
		name = "agent"
	}
	if dir == "" {
		dir = "log"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	var nonce [8]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return nil, e
	}
	safe := unsafeName.ReplaceAllString(name, "_")
	session := safe + "_" + time.Now().Format("2006-01-02-15-04-05.000000000") + fmt.Sprintf("_%x", nonce)
	path := filepath.Join(dir, session+".log")
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	journal, e := os.OpenFile(path+".journal", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		f.Close()
		return nil, e
	}
	r := &Recorder{file: f, journal: journal, AgentName: safe, Path: path, SessionID: session, active: map[uint64]bool{}}
	r.cond = sync.NewCond(&r.mu)
	return r, nil
}
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if FromContext(ctx) != r {
		ctx = context.WithValue(ctx, callKey{}, uint64(0))
	}
	return context.WithValue(ctx, contextKey{}, r)
}
func FromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(contextKey{}).(*Recorder)
	return r
}
func WithEffectiveTime(ctx context.Context, t time.Time) context.Context {
	if t.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, effectiveTimeKey{}, t.UTC().Format(time.RFC3339Nano))
}

// Standalone operations get an automatically owned session. Nested operations
// inherit one session. Begin is durable in a journal; .log has one completion.
func Start(ctx context.Context, name string, input any) (context.Context, *Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	s := &Span{function: strings.NewReplacer("\r", "_", "\n", "_").Replace(name), input: json.RawMessage(safeJSON(input)), started: time.Now()}
	r := FromContext(ctx)
	if r == nil {
		var e error
		r, e = New(name, "")
		if e != nil {
			s.startErr = e
			return ctx, s
		}
		s.own = true
		ctx = WithRecorder(ctx, r)
	}
	s.recorder = r
	s.effectiveAt, _ = ctx.Value(effectiveTimeKey{}).(string)
	s.parentID, _ = ctx.Value(callKey{}).(uint64)
	r.mu.Lock()
	if r.writeErr != nil {
		s.startErr = r.writeErr
	} else if r.closed || (r.closing && !r.active[s.parentID]) {
		s.startErr = ErrClosed
	}
	if s.startErr == nil {
		if !r.active[s.parentID] {
			s.parentID = 0
		}
		r.callSeq++
		s.callID = r.callSeq
		b, e := json.Marshal(beginRecord{s.callID, s.parentID, s.function, s.input, time.Now().UTC()})
		if e == nil {
			_, e = r.journal.Write(append(b, '\n'))
		}
		if e == nil {
			e = r.journal.Sync()
		}
		if e != nil {
			r.writeErr = e
			s.startErr = e
		} else {
			r.active[s.callID] = true
		}
	}
	r.mu.Unlock()
	return context.WithValue(ctx, callKey{}, s.callID), s
}
func (s *Span) Err() error { return s.startErr }
func (s *Span) Require() {
	if s.startErr != nil {
		panic(s.startErr)
	}
}
func (s *Span) End(out any, tokens *int, operationErr error) error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.endErr = s.startErr
		if s.recorder != nil && s.startErr == nil {
			r := s.recorder
			r.mu.Lock()
			s.endErr = r.writeLocked(s, out, tokens, operationErr)
			delete(r.active, s.callID)
			r.cond.Broadcast()
			r.mu.Unlock()
		}
		if s.own {
			s.endErr = errors.Join(s.endErr, s.recorder.Close())
		}
	})
	return s.endErr
}

// Finish must be deferred directly. It preserves panic evidence and partial
// output, returns audit failures through errp, and rethrows business panics.
func (s *Span) Finish(errp *error, output func() any, tokens func() *int) {
	p := recover()
	var opErr error
	if errp != nil {
		opErr = *errp
	}
	if p != nil {
		opErr = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
	}
	var out any
	if output != nil {
		out = output()
	}
	var tok *int
	if tokens != nil {
		tok = tokens()
	}
	auditErr := s.End(out, tok, opErr)
	if errp != nil {
		*errp = errors.Join(*errp, auditErr)
	}
	if p != nil {
		panic(p)
	}
	if errp == nil && auditErr != nil {
		panic(auditErr)
	}
}
func Call[T any](ctx context.Context, name string, input any, fn func(context.Context) (T, error)) (out T, err error) {
	ctx, s := Start(ctx, name, input)
	defer s.Finish(&err, func() any { return out }, nil)
	if err = s.Err(); err != nil {
		return
	}
	return fn(ctx)
}
func sensitive(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"api_key", "apikey", "authorization", "password", "secret", "credential", "access_token", "refresh_token", "x-goog-api-key", "token"} {
		if strings.Contains(k, s) && k != "input_tokens" && k != "output_tokens" && k != "max_output_tokens" && k != "context_tokens" && k != "total_tokens" {
			return true
		}
	}
	return false
}
func redactString(s string) string {
	return urls.ReplaceAllStringFunc(s, func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			return "[REDACTED_URL]"
		}
		if u.User != nil {
			u.User = url.User("[REDACTED]")
		}
		q := u.Query()
		for k := range q {
			if sensitive(k) {
				q.Set(k, "[REDACTED]")
			}
		}
		u.RawQuery = q.Encode()
		return u.String()
	})
}
func redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			if sensitive(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = redact(v)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = redact(v)
		}
		return out
	case string:
		return redactString(x)
	default:
		return v
	}
}
func safeJSON(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		return `"[UNSERIALIZABLE]"`
	}
	var raw any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&raw) != nil {
		return `"[UNSERIALIZABLE]"`
	}
	clean, _ := json.Marshal(redact(raw))
	return string(clean)
}
func (r *Recorder) Record(name string, in, out any, tokens *int, elapsed time.Duration, e error) error {
	_, s := Start(WithRecorder(context.Background(), r), name, in)
	s.started = time.Now().Add(-elapsed)
	return s.End(out, tokens, e)
}
func (r *Recorder) writeLocked(s *Span, out any, tokens *int, opErr error) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	tokenText := "?"
	if tokens != nil {
		tokenText = fmt.Sprint(*tokens)
	}
	effective := s.effectiveAt
	if effective == "" {
		effective = "?"
	}
	result := out
	if opErr != nil {
		result = map[string]any{"output": out, "error": opErr.Error()}
	}
	body := fmt.Sprintf("agent:%s, func:%s(input:%s), return:%s, token:%s, response_time:%s, time:%s, effective_time:%s, session:%s, seq:%d, call_id:%d, parent_call_id:%d, phase:END, prev_hash:%s", r.AgentName, s.function, s.input, safeJSON(result), tokenText, time.Since(s.started).Round(time.Microsecond), time.Now().Format(time.RFC3339Nano), effective, r.SessionID, r.seq+1, s.callID, s.parentID, r.lastHash)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	_, e := r.file.WriteString("[DEBUG] " + body + ", hash:" + hash + "\n")
	if e == nil {
		e = r.file.Sync()
	}
	if e != nil {
		r.writeErr = e
		return e
	}
	r.seq++
	r.lastHash = hash
	return nil
}
func (r *Recorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writeErr != nil {
		return r.writeErr
	}
	if r.closed {
		return ErrClosed
	}
	return nil
}
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	r.closing = true
	for len(r.active) > 0 {
		r.cond.Wait()
	}
	if r.closed {
		return r.closeErr
	}
	e := r.writeErr
	if e == nil {
		_, e = r.journal.Seek(0, io.SeekStart)
		h := sha256.New()
		if e == nil {
			_, e = io.Copy(h, r.journal)
		}
		if e == nil {
			b, _ := json.Marshal(seal{r.seq, r.lastHash, fmt.Sprintf("%x", h.Sum(nil)), r.callSeq})
			var f *os.File
			f, e = os.OpenFile(r.Path+".seal", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e == nil {
				_, e = f.Write(b)
				if e == nil {
					e = f.Sync()
				}
				e = errors.Join(e, f.Close())
			}
		}
	}
	r.closeErr = errors.Join(e, r.file.Close(), r.journal.Close())
	r.closed = true
	r.cond.Broadcast()
	return r.closeErr
}

// Verify requires a close seal and a begin journal. For legacy files use the
// explicitly weaker VerifyLegacy; neither mode prevents rewriting all files.
func Verify(path string) (int, error) {
	b, e := os.ReadFile(path + ".seal")
	if e != nil {
		return 0, fmt.Errorf("missing close seal: %w", e)
	}
	var seal seal
	if e = json.Unmarshal(b, &seal); e != nil {
		return 0, e
	}
	journal, e := os.ReadFile(path + ".journal")
	if e != nil {
		return 0, e
	}
	if fmt.Sprintf("%x", sha256.Sum256(journal)) != seal.JournalHash {
		return 0, errors.New("journal hash mismatch")
	}
	expected := map[uint64]beginRecord{}
	for _, line := range bytes.Split(bytes.TrimSpace(journal), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var item beginRecord
		if e = json.Unmarshal(line, &item); e != nil {
			return 0, e
		}
		if item.CallID != uint64(len(expected)+1) {
			return 0, errors.New("journal call sequence mismatch")
		}
		if item.ParentID >= item.CallID {
			return 0, errors.New("invalid parent")
		}
		expected[item.CallID] = item
	}
	n, last, seen, e := verifyLines(path, expected)
	if e != nil {
		return n, e
	}
	if uint64(n) != seal.Records || last != seal.LastHash || uint64(len(expected)) != seal.Calls || len(seen) != len(expected) {
		return n, errors.New("incomplete session or truncated tail")
	}
	return n, nil
}
func VerifyLegacy(path string) (int, error) { n, _, _, e := verifyLines(path, nil); return n, e }
func verifyLines(path string, expected map[uint64]beginRecord) (records int, last string, seen map[uint64]bool, err error) {
	seen = map[uint64]bool{}
	f, e := os.Open(path)
	if e != nil {
		err = e
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "[DEBUG] ") {
			err = errors.New("invalid prefix")
			return
		}
		content := strings.TrimPrefix(line, "[DEBUG] ")
		at := strings.LastIndex(content, ", hash:")
		if at < 0 {
			err = errors.New("missing hash")
			return
		}
		body, hash := content[:at], content[at+7:]
		if hash != fmt.Sprintf("%x", sha256.Sum256([]byte(body))) || !strings.HasSuffix(body, "prev_hash:"+last) {
			err = errors.New("hash chain mismatch")
			return
		}
		match := sequencePattern.FindStringSubmatch(body)
		if len(match) != 2 {
			err = errors.New("missing sequence")
			return
		}
		seq, _ := strconv.ParseUint(match[1], 10, 64)
		if seq != uint64(records+1) {
			err = errors.New("sequence mismatch")
			return
		}
		if expected != nil {
			m := callPattern.FindStringSubmatch(body)
			if len(m) != 3 {
				err = errors.New("missing call id")
				return
			}
			id, _ := strconv.ParseUint(m[1], 10, 64)
			parent, _ := strconv.ParseUint(m[2], 10, 64)
			begin, ok := expected[id]
			if !ok || seen[id] || parent != begin.ParentID || !strings.Contains(body, "func:"+begin.Function+"(input:"+string(begin.Input)+"), return:") {
				err = errors.New("call journal mismatch")
				return
			}
			seen[id] = true
		}
		last = hash
		records++
	}
	err = scanner.Err()
	return
}
