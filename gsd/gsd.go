package gsd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agentlog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Case struct {
	CaseID        string      `json:"case_id"`
	Signals       []string    `json:"signals"`
	ToolID        string      `json:"tool_id"`
	EquipmentType string      `json:"equipment_type"`
	Context       CaseContext `json:"context"`
	Source        CaseSource  `json:"source"`
}
type CaseContext struct {
	EventProtocol string `json:"event_protocol"`
	RecipeStep    string `json:"recipe_step"`
}
type CaseSource struct {
	Kind       string `json:"kind"`
	Seed       int    `json:"seed"`
	SpecSHA256 string `json:"spec_sha256"`
}
type Truth struct {
	CaseID       string `json:"case_id"`
	Fault        string `json:"fault"`
	Action       string `json:"action"`
	ScenarioCard string `json:"scenario_card"`
}
type Prediction struct {
	CaseID         string   `json:"case_id"`
	Decision       string   `json:"decision"`
	Action         string   `json:"action"`
	CardIDs        []string `json:"card_ids"`
	EvidenceIDs    []string `json:"evidence_ids"`
	ObservationIDs []string `json:"observation_ids"`
	Reason         string   `json:"reason"`
}
type Card struct {
	ID        string   `json:"id"`
	Fault     string   `json:"fault"`
	Required  []string `json:"required"`
	Forbidden []string `json:"forbidden"`
	Action    string   `json:"action"`
	Checks    []string `json:"checks"`
	Evidence  []string `json:"evidence"`
	Basis     string   `json:"basis"`
}
type Source struct {
	Kind  string `json:"kind"`
	URI   string `json:"uri"`
	Claim string `json:"claim"`
}
type Knowledge struct {
	SchemaVersion string            `json:"schema_version"`
	Scope         string            `json:"scope"`
	Sources       map[string]Source `json:"sources"`
	Cards         []Card            `json:"cards"`
}

func LoadKnowledge(path string) (Knowledge, error) {
	return LoadKnowledgeContext(context.Background(), path)
}
func LoadKnowledgeContext(ctx context.Context, path string) (knowledge Knowledge, err error) {
	_, span := agentlog.Start(ctx, "gsd.LoadKnowledge", map[string]any{"path": path})
	defer span.Finish(&err, func() any { return knowledge }, nil)
	if err = span.Err(); err != nil {
		return
	}
	var k Knowledge
	b, err := os.ReadFile(path)
	if err != nil {
		return k, err
	}
	err = json.Unmarshal(b, &k)
	if err != nil {
		return k, err
	}
	seen := map[string]bool{}
	for _, c := range k.Cards {
		if c.ID == "" || seen[c.ID] {
			return k, errors.New("invalid or duplicate card ID")
		}
		seen[c.ID] = true
		for _, id := range c.Evidence {
			if _, ok := k.Sources[id]; !ok {
				return k, fmt.Errorf("unknown source %s", id)
			}
		}
	}
	return k, nil
}
func ReadJSONL[T any](path string) ([]T, error) {
	return ReadJSONLContext[T](context.Background(), path)
}
func ReadJSONLContext[T any](ctx context.Context, path string) (items []T, err error) {
	_, span := agentlog.Start(ctx, "gsd.ReadJSONL", map[string]any{"path": path})
	defer span.Finish(&err, func() any { return items }, nil)
	if err = span.Err(); err != nil {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := []T{}
	for n, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var x T
		if err := json.Unmarshal([]byte(line), &x); err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		out = append(out, x)
	}
	return out, nil
}
func WriteJSONL[T any](path string, items []T) error {
	return WriteJSONLContext(context.Background(), path, items)
}
func WriteJSONLContext[T any](ctx context.Context, path string, items []T) (err error) {
	_, span := agentlog.Start(ctx, "gsd.WriteJSONL", map[string]any{"path": path, "items": items})
	defer span.Finish(&err, func() any { return map[string]any{"written": len(items)} }, nil)
	if err = span.Err(); err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	var b strings.Builder
	for _, x := range items {
		v, err := json.Marshal(x)
		if err != nil {
			return err
		}
		b.Write(v)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0600)
}
func Match(c Case, k Knowledge) []Card {
	return MatchContext(context.Background(), c, k)
}
func MatchContext(ctx context.Context, c Case, k Knowledge) (cards []Card) {
	_, span := agentlog.Start(ctx, "gsd.Match", map[string]any{"case": c, "knowledge_schema": k.SchemaVersion})
	defer span.Finish(nil, func() any { return cards }, nil)
	span.Require()
	signals := map[string]bool{}
	for _, s := range c.Signals {
		signals[s] = true
	}
	out := []Card{}
	for _, card := range k.Cards {
		ok := true
		for _, s := range card.Required {
			if !signals[s] {
				ok = false
			}
		}
		for _, s := range card.Forbidden {
			if signals[s] {
				ok = false
			}
		}
		if ok {
			out = append(out, card)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Required) > len(out[j].Required) })
	return out
}
func Decide(c Case, k Knowledge) Prediction {
	return DecideContext(context.Background(), c, k)
}
func DecideContext(ctx context.Context, c Case, k Knowledge) (prediction Prediction) {
	ctx, span := agentlog.Start(ctx, "gsd.Decide", map[string]any{"case": c, "knowledge_schema": k.SchemaVersion})
	defer span.Finish(nil, func() any { return prediction }, nil)
	span.Require()
	p := Prediction{CaseID: c.CaseID, Decision: "insufficient_evidence", Action: "request_additional_measurement", CardIDs: []string{}, EvidenceIDs: []string{}, ObservationIDs: []string{}, Reason: "No knowledge card uniquely matches the observed signals."}
	cards := MatchContext(ctx, c, k)
	if len(cards) == 0 {
		return p
	}
	top := len(cards[0].Required)
	if len(cards) > 1 && len(cards[1].Required) == top {
		return p
	}
	card := cards[0]
	p.Decision = card.Fault
	p.Action = card.Action
	p.CardIDs = []string{card.ID}
	p.EvidenceIDs = append([]string(nil), card.Evidence...)
	p.Reason = fmt.Sprintf("Observed signals match %s; next diagnostic check: %s", card.ID, strings.Join(card.Checks, "; "))
	return p
}

type Score struct {
	Cases           int `json:"cases"`
	DecisionCorrect int `json:"decision_correct"`
	ActionCorrect   int `json:"action_correct"`
	CitationValid   int `json:"citation_valid"`
	SchemaValid     int `json:"schema_valid"`
	Coverage        int `json:"coverage"`
}

func Evaluate(cases []Case, truth []Truth, pred []Prediction, k Knowledge) (Score, error) {
	return EvaluateContext(context.Background(), cases, truth, pred, k)
}
func EvaluateContext(ctx context.Context, cases []Case, truth []Truth, pred []Prediction, k Knowledge) (score Score, err error) {
	ctx, span := agentlog.Start(ctx, "gsd.Evaluate", map[string]any{"cases": cases, "truth": truth, "predictions": pred, "knowledge_schema": k.SchemaVersion})
	defer span.Finish(&err, func() any { return score }, nil)
	if err = span.Err(); err != nil {
		return
	}
	s := Score{Cases: len(cases)}
	allowedFault := map[string]bool{"insufficient_evidence": true}
	allowedAction := map[string]bool{"request_additional_measurement": true}
	cardsByID := map[string]Card{}
	for _, card := range k.Cards {
		allowedFault[card.Fault] = true
		allowedAction[card.Action] = true
		cardsByID[card.ID] = card
	}
	ts := map[string]Truth{}
	ps := map[string]Prediction{}
	for _, v := range truth {
		if _, ok := ts[v.CaseID]; ok {
			return s, fmt.Errorf("duplicate truth %s", v.CaseID)
		}
		ts[v.CaseID] = v
	}
	for _, v := range pred {
		if _, ok := ps[v.CaseID]; ok {
			return s, fmt.Errorf("duplicate prediction %s", v.CaseID)
		}
		ps[v.CaseID] = v
	}
	if len(cases) != len(ts) || len(cases) != len(ps) {
		return s, errors.New("case/truth/prediction counts differ")
	}
	for _, c := range cases {
		t, ok1 := ts[c.CaseID]
		p, ok2 := ps[c.CaseID]
		if !ok1 || !ok2 {
			return s, fmt.Errorf("missing case %s", c.CaseID)
		}
		if p.Decision == t.Fault {
			s.DecisionCorrect++
		}
		if p.Action == t.Action {
			s.ActionCorrect++
		}
		if p.Decision != "insufficient_evidence" {
			s.Coverage++
		}
		validSchema := allowedFault[p.Decision] && allowedAction[p.Action] && p.Reason != ""
		for _, id := range p.CardIDs {
			if _, ok := cardsByID[id]; !ok {
				validSchema = false
			}
		}
		for _, id := range p.EvidenceIDs {
			if _, ok := k.Sources[id]; !ok {
				validSchema = false
			}
		}
		if p.Decision == "insufficient_evidence" && (p.Action != "request_additional_measurement" || len(p.CardIDs) > 0) {
			validSchema = false
		}
		if validSchema {
			s.SchemaValid++
		}
		matched := MatchContext(ctx, c, k)
		for _, card := range matched {
			if len(p.CardIDs) == 1 && p.CardIDs[0] == card.ID && p.Decision == card.Fault && p.Action == card.Action {
				valid := true
				for _, id := range p.EvidenceIDs {
					found := false
					for _, allowed := range card.Evidence {
						if id == allowed {
							found = true
						}
					}
					if !found {
						valid = false
					}
				}
				if len(p.EvidenceIDs) == 0 {
					valid = false
				}
				if valid {
					s.CitationValid++
				}
				break
			}
		}
	}
	return s, nil
}
