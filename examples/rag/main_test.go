package main

import (
	"context"
	"testing"
)

func TestAnswersCiteTheMatchingDocument(t *testing.T) {
	for question, want := range map[string]string{
		"How many days of paid annual leave do employees get?": "leave",
		"Where do I submit travel expense receipts?":           "expenses",
		"Who do I tell about a lost laptop?":                   "security",
	} {
		out, err := run(context.Background(), scriptedChat{}, hashEmbedder{}, question, t.TempDir(), 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Documents) == 0 || out.Documents[0].ID != want || out.Valid != true {
			t.Errorf("%q: %+v", question, out)
		}
	}
}
