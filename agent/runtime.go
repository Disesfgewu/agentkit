package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Disesfgewu/agentkit/agentlog"
	"io"
	"net/http"
)

// Runtime is an operation facade over the existing Template-owned audit session.
type Runtime struct {
	recorder *agentlog.Recorder
	owner    *Template
}

func RuntimeFromContext(ctx context.Context) (*Runtime, error) {
	r := agentlog.FromContext(ctx)
	if r == nil {
		return nil, fmt.Errorf("agent runtime context required")
	}
	return &Runtime{recorder: r}, nil
}
func NewRuntime(name, directory string) (*Runtime, error) {
	return (&Template{Name: name, LogDir: directory}).Runtime()
}

// Runtime reuses Template.recorder; it never creates a second logger/session.
func (a *Template) Runtime() (*Runtime, error) {
	r, e := a.recorder()
	if e != nil {
		return nil, e
	}
	return &Runtime{recorder: r, owner: a}, nil
}
func (r *Runtime) Context(ctx context.Context) context.Context {
	return agentlog.WithRecorder(ctx, r.recorder)
}
func (r *Runtime) Path() string { return r.recorder.Path }
func (r *Runtime) Close() error {
	if r.owner == nil {
		return errors.New("borrowed runtime cannot close its owner")
	}
	err := r.owner.Close()
	if err != nil {
		return err
	}
	_, err = agentlog.Verify(r.recorder.Path)
	return err
}

// Execute is the extension operation boundary: one completion, including errors/panics.
func (r *Runtime) Execute(ctx context.Context, name string, input any, fn func(context.Context) (any, error)) (output any, err error) {
	return agentlog.Call(r.Context(ctx), name, input, fn)
}

// JSON automatically records HTTP input/output and errors; callers never Record.
func (r *Runtime) JSON(ctx context.Context, c *http.Client, method, url string, input, output any) error {
	_, err := r.Execute(ctx, "HTTP."+method, map[string]any{"url": url, "body": input}, func(ctx context.Context) (any, error) {
		var body io.Reader
		if input != nil {
			b, e := json.Marshal(input)
			if e != nil {
				return nil, e
			}
			body = bytes.NewReader(b)
		}
		request, e := http.NewRequestWithContext(ctx, method, url, body)
		if e != nil {
			return nil, e
		}
		request.Header.Set("Content-Type", "application/json")
		response, e := c.Do(request)
		if e != nil {
			return nil, e
		}
		defer response.Body.Close()
		data, e := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
		if e != nil {
			return nil, e
		}
		if len(data) > 4<<20 {
			return nil, fmt.Errorf("response too large")
		}
		if response.StatusCode >= 300 {
			return nil, fmt.Errorf("HTTP %d", response.StatusCode)
		}
		if e = json.Unmarshal(data, output); e != nil {
			return nil, e
		}
		return output, nil
	})
	return err
}
