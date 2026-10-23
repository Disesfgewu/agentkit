package agentlog

import (
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
}
