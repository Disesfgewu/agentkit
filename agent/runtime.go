package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agentlog"
	"io"
	"net/http"
)

// Runtime owns the audit session. Application code calls operations, not logger APIs.
type Runtime struct{ recorder *agentlog.Recorder }

func RuntimeFromContext(ctx context.Context) (*Runtime, error) {
	r := agentlog.FromContext(ctx)
	if r == nil {
		return nil, fmt.Errorf("agent runtime context required")
	}
	return &Runtime{recorder: r}, nil
}
func NewRuntime(name, directory string) (*Runtime, error) {
	r, e := agentlog.New(name, directory)
	if e != nil {
		return nil, e
	}
	return &Runtime{recorder: r}, nil
}
func (r *Runtime) Context(ctx context.Context) context.Context {
	return agentlog.WithRecorder(ctx, r.recorder)
}
func (r *Runtime) Path() string { return r.recorder.Path }
func (r *Runtime) Close() error {
	err := errors.Join(r.recorder.Err(), r.recorder.Close())
	if err != nil {
		return err
	}
	_, err = agentlog.Verify(r.recorder.Path)
	return err
}

// Execute is the extension operation boundary: one completion, including errors/panics.
func (r *Runtime) Execute(ctx context.Context, name string, input any, fn func(context.Context) (any, error)) (output any, err error) {
	ctx, span := agentlog.Start(r.Context(ctx), name, input)
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("operation panic: %v", p)
			span.End(output, nil, err)
			panic(p)
		}
		span.End(output, nil, err)
		err = errors.Join(err, r.recorder.Err())
	}()
	if err = r.recorder.Err(); err != nil {
		return nil, err
	}
	return fn(ctx)
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
