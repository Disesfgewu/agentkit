package agent

import (
	"encoding/json"
	"fmt"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/gsd"
)

// NewGSDRetriever indexes only knowledge cards and source claims, never truth labels.
func NewGSDRetriever(k gsd.Knowledge) *LocalRetriever {
	r := &LocalRetriever{}
	for id, s := range k.Sources {
		r.Documents = append(r.Documents, Document{ID: "source:" + id, Source: s.URI, Text: fmt.Sprintf("%s. %s", s.Kind, s.Claim)})
	}
	for _, c := range k.Cards {
		b, _ := json.Marshal(c)
		text := string(b)
		for _, evidenceID := range c.Evidence {
			if source, ok := k.Sources[evidenceID]; ok {
				text += fmt.Sprintf("\nSource %s (%s): %s", evidenceID, source.Kind, source.Claim)
			}
		}
		r.Documents = append(r.Documents, Document{ID: "card:" + c.ID, Source: "knowledge.json#" + c.ID, Text: text})
	}
	return r
}
