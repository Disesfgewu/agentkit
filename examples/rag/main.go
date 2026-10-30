// RAG composes an agent from a ChatAgent and an EmbeddingAgent. It runs offline
// with scripted clients, or against real providers through an LLMRouter.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Disesfgewu/agentkit/agent"
	"github.com/Disesfgewu/agentkit/model"
	"hash/fnv"
	"os"
	"regexp"
	"strings"
)

var handbook = []agent.Document{
	{ID: "leave", Source: "handbook.md", Text: "Employees receive fifteen days of paid annual leave each year."},
	{ID: "expenses", Source: "handbook.md", Text: "Submit travel expense receipts within thirty days of the trip."},
	{ID: "security", Source: "handbook.md", Text: "Report a lost laptop to the security team immediately."},
}

// hashEmbedder is a stand-in with no semantic understanding: it only sees shared words.
type hashEmbedder struct{}

func (hashEmbedder) Embed(_ context.Context, r model.EmbeddingRequest) (model.EmbeddingResponse, error) {
	out := model.EmbeddingResponse{Model: "offline-hash", Provider: "scripted"}
	for _, text := range r.Input {
		vector := make([]float32, 128)
		for _, word := range strings.FieldsFunc(strings.ToLower(text), func(c rune) bool { return c < 'a' || c > 'z' }) {
			h := fnv.New32a()
			h.Write([]byte(word))
			vector[h.Sum32()%128]++
		}
		out.Vectors = append(out.Vectors, vector)
	}
	return out, nil
}

// scriptedChat answers by citing the first document Ask placed in the system prompt.
type scriptedChat struct{}

var citation = regexp.MustCompile(`(?m)^\[([^\]]+)\] `)

func (scriptedChat) Chat(_ context.Context, r model.Request) (model.Response, error) {
	text := "Insufficient evidence."
	if match := citation.FindStringSubmatch(r.Messages[0].Content); match != nil {
		text = "See [" + match[1] + "]."
	}
	return model.Response{Text: text, Provider: "scripted", Model: "offline"}, nil
}

type result struct {
	Answer    string           `json:"answer"`
	Citations []string         `json:"citation_ids"`
	Valid     any              `json:"citation_valid"`
	Documents []agent.Document `json:"documents"`
}

func run(ctx context.Context, chat model.Client, embedder model.Embedder, question, logDir string, minScore float64) (result, error) {
	retriever := agent.NewEmbeddingAgent(embedder, nil)
	retriever.MinScore = minScore
	a := agent.New(agent.Config{
		Name:      "handbook",
		LogDir:    logDir,
		Chat:      &agent.ChatAgent{Client: chat, SystemPrompt: "Answer only from the supplied evidence.", MaxOutputTokens: 2048},
		Retriever: retriever,
	})
	defer a.Close()
	if err := a.Ingest(ctx, handbook...); err != nil {
		return result{}, err
	}
	r, err := a.Ask(ctx, question, "")
	if err != nil {
		return result{}, err
	}
	answer, _ := r.State["answer"].(string)
	citations, _ := r.State["citation_ids"].([]string)
	documents, _ := r.State["documents"].([]agent.Document)
	return result{answer, citations, r.State["citation_valid"], documents}, nil
}

func main() {
	question := flag.String("q", "How many days of paid annual leave do employees get?", "question")
	router := flag.String("router", "", "LLMRouter URL; empty uses scripted clients")
	chatModel := flag.String("chat-model", "", "router chat entry to pin")
	embedModel := flag.String("embedding-model", "", "router embedding entry to pin")
	logDir := flag.String("log-dir", "log", "audit log directory")
	minScore := flag.Float64("min-score", 0, "drop documents below this similarity; zero keeps all")
	flag.Parse()
	var chat model.Client = scriptedChat{}
	var embedder model.Embedder = hashEmbedder{}
	if *router != "" {
		remote, err := model.NewRemote(*router, os.Getenv("LLMROUTER_TOKEN"), *chatModel)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		remote.EmbeddingModel = *embedModel
		chat, embedder = remote, remote
	}
	out, err := run(context.Background(), chat, embedder, *question, *logDir, *minScore)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
