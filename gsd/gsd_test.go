package gsd

import (
	"path/filepath"
	"testing"
)

func TestLegacySeed42(t *testing.T) {
	root := filepath.Join("..", "..", "generator", "agent")
	k, err := LoadKnowledge(filepath.Join(root, "knowledge.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases, err := ReadJSONL[Case](filepath.Join(root, "runs", "seed42", "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if cases[0].Context.EventProtocol != "GEM-like JSON abstraction" || cases[0].Context.RecipeStep != "main_etch" || cases[0].Source.Seed != 42 || cases[0].Source.SpecSHA256 == "" {
		t.Fatalf("case metadata was not preserved: %+v", cases[0])
	}
	truth, err := ReadJSONL[Truth](filepath.Join(root, "runs", "seed42", "truth.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	pred := make([]Prediction, 0, len(cases))
	for _, c := range cases {
		pred = append(pred, Decide(c, k))
	}
	score, err := Evaluate(cases, truth, pred, k)
	if err != nil {
		t.Fatal(err)
	}
	if score.Cases != 160 || score.DecisionCorrect != 160 || score.ActionCorrect != 160 || score.CitationValid != 140 || score.Coverage != 140 {
		t.Fatalf("legacy regression: %+v", score)
	}
}
