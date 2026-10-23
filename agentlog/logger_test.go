package agentlog

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRedaction(t *testing.T) {
	r, err := New("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.Record("Call", map[string]any{"API_KEY": "secret-value", "nested": map[string]any{"password": "hidden"}}, nil, nil, time.Second, nil)
	r.Close()
	b, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	if strings.Contains(line, "secret-value") || strings.Contains(line, "hidden") || !strings.Contains(line, "[REDACTED]") {
		t.Fatal("redaction failed")
	}
	if records, err := Verify(r.Path); err != nil || records != 1 {
		t.Fatalf("verify records=%d err=%v", records, err)
	}
	tampered := strings.Replace(string(b), "agent:test", "agent:changed", 1)
	if err := os.WriteFile(r.Path, []byte(tampered), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(r.Path); err == nil {
		t.Fatal("tampering was not detected")
	}
}

func TestNestedCallAndChain(t *testing.T) {
	r, err := New("nested", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithRecorder(context.Background(), r)
	value, err := Call(ctx, "outer", map[string]any{"x": 1}, func(ctx context.Context) (string, error) {
		return Call(ctx, "inner", "input", func(context.Context) (string, error) { return "ok", nil })
	})
	if err != nil || value != "ok" {
		t.Fatal(value, err)
	}
	r.Close()
	records, err := Verify(r.Path)
	if err != nil || records != 4 {
		t.Fatalf("records=%d err=%v", records, err)
	}
	b, _ := os.ReadFile(r.Path)
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "func:inner(") && strings.Contains(line, "parent_call_id:1") && strings.Contains(line, "phase:START") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing parent relationship")
	}
}
