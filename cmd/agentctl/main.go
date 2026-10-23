package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agent"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/gsd"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/model"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
func loadEnv(path string) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pair := strings.SplitN(line, "=", 2)
		if len(pair) != 2 {
			return fmt.Errorf("invalid env line")
		}
		key := strings.TrimSpace(pair[0])
		val := strings.Trim(strings.TrimSpace(pair[1]), "\"'")
		if key == "" {
			return fmt.Errorf("empty env name")
		}
		if _, ok := os.LookupEnv(key); !ok {
			os.Setenv(key, val)
		}
	}
	return nil
}
func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: agentctl benchmark|evaluate|llm-eval|ask|chat"))
	}
	switch os.Args[1] {
	case "benchmark":
		fs := flag.NewFlagSet("benchmark", flag.ExitOnError)
		root := fs.String("root", "../generator/agent", "GSD agent directory")
		out := fs.String("out", "", "optional prediction JSONL path")
		fs.Parse(os.Args[2:])
		k, err := gsd.LoadKnowledge(filepath.Join(*root, "knowledge.json"))
		if err != nil {
			fail(err)
		}
		cases, err := gsd.ReadJSONL[gsd.Case](filepath.Join(*root, "runs/seed42/cases.jsonl"))
		if err != nil {
			fail(err)
		}
		pred := make([]gsd.Prediction, 0, len(cases))
		for _, c := range cases {
			pred = append(pred, gsd.Decide(c, k))
		}
		if *out != "" {
			if err := gsd.WriteJSONL(*out, pred); err != nil {
				fail(err)
			}
		}
		truth, err := gsd.ReadJSONL[gsd.Truth](filepath.Join(*root, "runs/seed42/truth.jsonl"))
		if err != nil {
			fail(err)
		}
		score, err := gsd.Evaluate(cases, truth, pred, k)
		if err != nil {
			fail(err)
		}
		printJSON(score)
	case "evaluate":
		fs := flag.NewFlagSet("evaluate", flag.ExitOnError)
		root := fs.String("root", "../generator/agent", "GSD agent directory")
		predictions := fs.String("predictions", "", "prediction JSONL path")
		limit := fs.Int("limit", 0, "optional prefix of seed42 cases")
		fs.Parse(os.Args[2:])
		if *predictions == "" {
			fail(fmt.Errorf("-predictions required"))
		}
		k, err := gsd.LoadKnowledge(filepath.Join(*root, "knowledge.json"))
		if err != nil {
			fail(err)
		}
		cases, err := gsd.ReadJSONL[gsd.Case](filepath.Join(*root, "runs/seed42/cases.jsonl"))
		if err != nil {
			fail(err)
		}
		truth, err := gsd.ReadJSONL[gsd.Truth](filepath.Join(*root, "runs/seed42/truth.jsonl"))
		if err != nil {
			fail(err)
		}
		pred, err := gsd.ReadJSONL[gsd.Prediction](*predictions)
		if err != nil {
			fail(err)
		}
		if *limit > 0 && *limit <= len(cases) {
			cases = cases[:*limit]
			truth = truth[:*limit]
		}
		score, err := gsd.Evaluate(cases, truth, pred, k)
		if err != nil {
			fail(err)
		}
		printJSON(score)
	case "llm-eval":
		fs := flag.NewFlagSet("llm-eval", flag.ExitOnError)
		root := fs.String("root", "../generator/agent", "GSD agent directory")
		config := fs.String("config", "models.example.json", "model configuration")
		env := fs.String("env", "", "optional .env path")
		name := fs.String("model", "", "model name from config")
		limit := fs.Int("limit", 8, "number of cases from seed42")
		delay := fs.Duration("delay", 0, "delay between provider requests, e.g. 10s")
		out := fs.String("out", "", "optional prediction JSONL output")
		fs.Parse(os.Args[2:])
		if *limit < 1 {
			fail(fmt.Errorf("-limit must be positive"))
		}
		if err := loadEnv(*env); err != nil {
			fail(err)
		}
		client, err := loadModel(*config, *name)
		if err != nil {
			fail(err)
		}
		k, err := gsd.LoadKnowledge(filepath.Join(*root, "knowledge.json"))
		if err != nil {
			fail(err)
		}
		cases, err := gsd.ReadJSONL[gsd.Case](filepath.Join(*root, "runs/seed42/cases.jsonl"))
		if err != nil {
			fail(err)
		}
		if *limit > len(cases) {
			*limit = len(cases)
		}
		cases = cases[:*limit]
		cards, _ := json.Marshal(k.Cards)
		pred := make([]gsd.Prediction, 0, len(cases))
		for _, c := range cases {
			observed, _ := json.Marshal(c)
			response, e := client.Chat(context.Background(), model.Request{Messages: []model.Message{
				{Role: "system", Content: "You triage synthetic etch equipment. Use only the supplied cards and signals. Do not invent thresholds or SOPs. Return exactly one JSON object with case_id, decision, action, card_ids, evidence_ids, observation_ids, reason. If no unique card fits, use decision insufficient_evidence and action request_additional_measurement with empty citation arrays. A card fits only if every required signal is present and every forbidden signal absent. Knowledge cards: " + string(cards)},
				{Role: "user", Content: "Observable case: " + string(observed)},
			}, MaxOutputTokens: 2048})
			if e != nil {
				fail(fmt.Errorf("case %s: %w", c.CaseID, e))
			}
			var p gsd.Prediction
			answer := strings.TrimSpace(response.Text)
			answer = strings.TrimPrefix(answer, "```json")
			answer = strings.TrimPrefix(answer, "```")
			answer = strings.TrimSuffix(answer, "```")
			if e := json.Unmarshal([]byte(strings.TrimSpace(answer)), &p); e != nil {
				fail(fmt.Errorf("case %s: invalid JSON response: %w", c.CaseID, e))
			}
			if p.CaseID != c.CaseID {
				fail(fmt.Errorf("case %s: response case_id mismatch", c.CaseID))
			}
			pred = append(pred, p)
			if *out != "" {
				if err := gsd.WriteJSONL(*out, pred); err != nil {
					fail(err)
				}
			}
			if len(pred) < len(cases) && *delay > 0 {
				time.Sleep(*delay)
			}
		}
		allTruth, err := gsd.ReadJSONL[gsd.Truth](filepath.Join(*root, "runs/seed42/truth.jsonl"))
		if err != nil {
			fail(err)
		}
		truth := make([]gsd.Truth, 0, len(cases))
		truthByID := map[string]gsd.Truth{}
		for _, t := range allTruth {
			truthByID[t.CaseID] = t
		}
		for _, c := range cases {
			truth = append(truth, truthByID[c.CaseID])
		}
		score, err := gsd.Evaluate(cases, truth, pred, k)
		if err != nil {
			fail(err)
		}
		printJSON(score)
	case "ask", "chat":
		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		config := fs.String("config", "models.example.json", "model configuration")
		env := fs.String("env", "", "optional .env path")
		name := fs.String("model", "", "model name from config")
		agentName := fs.String("agent-name", "gsd-agent", "name used by automatic logger")
		logDir := fs.String("log-dir", "log", "automatic log directory")
		reasoning := fs.String("reasoning", "", "model-specific reasoning value")
		question := fs.String("q", "", "question")
		docs := fs.String("docs", "", "comma separated markdown documents for ask")
		knowledge := fs.String("knowledge", "", "GSD knowledge.json path for ask")
		fs.Parse(os.Args[2:])
		if *question == "" {
			fail(fmt.Errorf("-q required"))
		}
		if err := loadEnv(*env); err != nil {
			fail(err)
		}
		selected, err := loadModel(*config, *name)
		if err != nil {
			fail(err)
		}
		a := agent.Template{Name: *agentName, LogDir: *logDir, Model: selected}
		defer a.Close()
		if os.Args[1] == "ask" {
			combined := &agent.LocalRetriever{}
			if *docs != "" {
				var markdown *agent.LocalRetriever
				markdown, err = agent.LoadMarkdown(strings.Split(*docs, ","))
				if err != nil {
					fail(err)
				}
				combined.Documents = append(combined.Documents, markdown.Documents...)
			}
			if *knowledge != "" {
				k, e := gsd.LoadKnowledge(*knowledge)
				if e != nil {
					fail(e)
				}
				combined.Documents = append(combined.Documents, agent.NewGSDRetriever(k).Documents...)
			}
			a.Retriever = combined
			r, err := a.Ask(context.Background(), *question, *reasoning)
			if err != nil {
				fail(err)
			}
			printJSON(r)
		} else {
			r, err := a.Chat(context.Background(), []model.Message{{Role: "user", Content: *question}}, *reasoning)
			if err != nil {
				fail(err)
			}
			printJSON(r)
		}
	default:
		fail(fmt.Errorf("unknown command %s", os.Args[1]))
	}
}
func printJSON(v any) { b, _ := json.MarshalIndent(v, "", "  "); fmt.Println(string(b)) }
func loadModel(path, name string) (*model.HTTPClient, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var configs []model.Config
	if err := json.Unmarshal([]byte(os.ExpandEnv(string(b))), &configs); err != nil {
		return nil, err
	}
	for _, c := range configs {
		if c.Name == name {
			return model.New(c)
		}
	}
	return nil, fmt.Errorf("model %q missing from config", name)
}
