package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Disesfgewu/agentkit/agent"
	"github.com/Disesfgewu/agentkit/agentlog"
	"github.com/Disesfgewu/agentkit/model"
	"os"
	"strings"
)

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
func loadEnv(path string) error {
	return loadEnvContext(context.Background(), path)
}
func loadEnvContext(ctx context.Context, path string) (err error) {
	_, span := agentlog.Start(ctx, "cmd.loadEnv", map[string]any{"path": path})
	loaded := []string{}
	defer func() { span.End(map[string]any{"variables": loaded}, nil, err) }()
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
		loaded = append(loaded, key)
	}
	return nil
}
func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: agentctl ask|chat|audit-verify"))
	}
	switch os.Args[1] {
	case "audit-verify":
		fs := flag.NewFlagSet("audit-verify", flag.ExitOnError)
		path := fs.String("log", "", "audit log path")
		fs.Parse(os.Args[2:])
		if *path == "" {
			fail(fmt.Errorf("-log required"))
		}
		records, err := agentlog.Verify(*path)
		if err != nil {
			fail(err)
		}
		printJSON(map[string]any{"valid": true, "records": records, "log": *path})
	case "ask", "chat":
		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		config := fs.String("config", "models.example.json", "model configuration")
		env := fs.String("env", "", "optional .env path")
		name := fs.String("model", "", "model name from config")
		agentName := fs.String("agent-name", "agent", "name used by automatic logger")
		logDir := fs.String("log-dir", "log", "automatic log directory")
		reasoning := fs.String("reasoning", "", "model-specific reasoning value")
		systemPrompt := fs.String("system-prompt", "", "agent-level system prompt")
		question := fs.String("q", "", "question")
		docs := fs.String("docs", "", "comma separated markdown documents for ask")
		fs.Parse(os.Args[2:])
		ctx, rec, err := newAudit(*agentName, *logDir)
		if err != nil {
			fail(err)
		}
		defer rec.Close()
		if *question == "" {
			fail(fmt.Errorf("-q required"))
		}
		if err := loadEnvContext(ctx, *env); err != nil {
			fail(err)
		}
		selected, err := loadModelContext(ctx, *config, *name)
		if err != nil {
			fail(err)
		}
		a := agent.Template{Name: *agentName, SystemPrompt: *systemPrompt, LogDir: *logDir, Logger: rec, Model: selected}
		if os.Args[1] == "ask" {
			combined := &agent.LocalRetriever{}
			if *docs != "" {
				var markdown *agent.LocalRetriever
				markdown, err = agent.LoadMarkdownContext(ctx, strings.Split(*docs, ","))
				if err != nil {
					fail(err)
				}
				combined.Documents = append(combined.Documents, markdown.Documents...)
			}
			a.Retriever = combined
			r, err := a.Ask(ctx, *question, *reasoning)
			if err != nil {
				fail(err)
			}
			printJSONContext(ctx, r)
		} else {
			r, err := a.Chat(ctx, []model.Message{{Role: "user", Content: *question}}, *reasoning)
			if err != nil {
				fail(err)
			}
			printJSONContext(ctx, r)
		}
	default:
		fail(fmt.Errorf("unknown command %s", os.Args[1]))
	}
}
func printJSON(v any) { printJSONContext(context.Background(), v) }
func printJSONContext(ctx context.Context, v any) {
	_, span := agentlog.Start(ctx, "cmd.printJSON", v)
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		fmt.Println(string(b))
	}
	span.End(map[string]any{"bytes": len(b)}, nil, err)
}
func loadModel(path, name string) (*model.HTTPClient, error) {
	return loadModelContext(context.Background(), path, name)
}
func loadModelContext(ctx context.Context, path, name string) (client *model.HTTPClient, err error) {
	ctx, span := agentlog.Start(ctx, "cmd.loadModel", map[string]any{"path": path, "name": name})
	defer func() { span.End(map[string]any{"loaded": client != nil}, nil, err) }()
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
			return model.NewContext(ctx, c)
		}
	}
	return nil, fmt.Errorf("model %q missing from config", name)
}
func newAudit(agentName, logDir string) (context.Context, *agentlog.Recorder, error) {
	r, err := agentlog.New(agentName, logDir)
	if err != nil {
		return context.Background(), nil, err
	}
	ctx := agentlog.WithRecorder(context.Background(), r)
	return ctx, r, nil
}
