package main

import (
	"context"
	"github.com/Disesfgewu/agentkit/agentlog"
	"strings"
	"testing"
)

func TestRoutesTicketAndWritesVerifiableAudit(t *testing.T) {
	for ticket, want := range map[string]string{
		"I want a refund":      "refundable",
		"How long is shipping": "faq:shipping",
	} {
		state, audit, err := run(context.Background(), scriptedModel{}, ticket, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(state["reply"].(string), want) {
			t.Errorf("%q: reply %q lacks %q", ticket, state["reply"], want)
		}
		if records, err := agentlog.Verify(audit); err != nil || records == 0 {
			t.Errorf("%q: audit records=%d err=%v", ticket, records, err)
		}
	}
}
