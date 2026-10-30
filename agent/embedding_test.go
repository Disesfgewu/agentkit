package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Disesfgewu/agentkit/model"
	"hash/fnv"
	"strings"
	"testing"
	"unicode/utf8"
)

// wordEmbedder hashes words into a small vector, so related texts score higher
// without a provider, and records how it was called.
type wordEmbedder struct {
	model string
	types map[string]int
	sizes []int
}

func (w *wordEmbedder) Embed(_ context.Context, r model.EmbeddingRequest) (model.EmbeddingResponse, error) {
	if w.types == nil {
		w.types = map[string]int{}
	}
	w.types[r.InputType]++
	w.sizes = append(w.sizes, len(r.Input))
	out := model.EmbeddingResponse{Model: w.model}
	for _, text := range r.Input {
		vector := make([]float32, 64)
		for _, word := range strings.Fields(strings.ToLower(text)) {
			h := fnv.New32a()
			h.Write([]byte(word))
			vector[h.Sum32()%64]++
		}
		out.Vectors = append(out.Vectors, vector)
	}
	return out, nil
}

func TestEmbeddingAgentRanksByMeaningfulOverlap(t *testing.T) {
	embedder := &wordEmbedder{model: "m1"}
	e := NewEmbeddingAgent(embedder, nil)
	ctx := context.Background()
	if err := e.Ingest(ctx, Document{ID: "ship", Text: "shipping takes three days"}, Document{ID: "refund", Text: "refund within thirty days"}); err != nil {
		t.Fatal(err)
	}
	docs, err := e.Retrieve(ctx, "how long does shipping take", 1)
	if err != nil || len(docs) != 1 || docs[0].ID != "ship" || docs[0].Score <= 0 {
		t.Fatalf("docs=%+v err=%v", docs, err)
	}
	if embedder.types[model.InputDocument] != 1 || embedder.types[model.InputQuery] != 1 {
		t.Fatalf("documents and queries must be labeled: %+v", embedder.types)
	}
}

func TestMinScoreLetsAskReportNoEvidence(t *testing.T) {
	e := NewEmbeddingAgent(&wordEmbedder{model: "m1"}, nil)
	e.MinScore = 0.5
	ctx := context.Background()
	if err := e.Ingest(ctx, Document{ID: "ship", Text: "shipping takes three days"}); err != nil {
		t.Fatal(err)
	}
	docs, err := e.Retrieve(ctx, "completely unrelated question", 3)
	if err != nil || len(docs) != 0 {
		t.Fatalf("weak match must be dropped: %+v %v", docs, err)
	}
}

func TestIndexRejectsDifferentEmbeddingModel(t *testing.T) {
	store := &MemoryVectorStore{}
	ctx := context.Background()
	if err := NewEmbeddingAgent(&wordEmbedder{model: "m1"}, store).Ingest(ctx, Document{ID: "a", Text: "alpha"}); err != nil {
		t.Fatal(err)
	}
	other := NewEmbeddingAgent(&wordEmbedder{model: "m2"}, store)
	if _, err := other.Retrieve(ctx, "alpha", 1); !errors.Is(err, ErrEmbeddingMismatch) {
		t.Fatalf("query with another model: %v", err)
	}
	if err := other.Ingest(ctx, Document{ID: "b", Text: "beta"}); !errors.Is(err, ErrEmbeddingMismatch) {
		t.Fatalf("ingest with another model: %v", err)
	}
}

func TestIngestChunksBatchesAndReplacesByID(t *testing.T) {
	embedder := &wordEmbedder{model: "m1"}
	store := &MemoryVectorStore{}
	e := NewEmbeddingAgent(embedder, store)
	e.ChunkRunes, e.BatchSize = 10, 2
	ctx := context.Background()
	long := Document{ID: "doc", Source: "s.md", Text: "第一段文字\n\n第二段文字\n\n" + strings.Repeat("長", 25)}
	if err := e.Ingest(ctx, long); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(embedder.sizes) != "[2 2 1]" {
		t.Fatalf("batches %v", embedder.sizes)
	}
	if len(store.records) != 5 {
		t.Fatalf("records %d", len(store.records))
	}
	for id, r := range store.records {
		if !strings.HasPrefix(id, "doc#") || r.Source != "s.md" || !utf8.ValidString(r.Text) || utf8.RuneCountInString(r.Text) > 10 {
			t.Errorf("bad chunk %s %q", id, r.Text)
		}
	}
	if err := e.Ingest(ctx, long); err != nil || len(store.records) != 5 {
		t.Fatalf("reingest must not duplicate: %d %v", len(store.records), err)
	}
}

type answerModel struct{ text string }

func (m answerModel) Chat(context.Context, model.Request) (model.Response, error) {
	return model.Response{Text: m.text, Provider: "mock", Model: "fake"}, nil
}

type echoTool struct{}

func (echoTool) Name() string { return "echo" }
func (echoTool) Run(_ context.Context, in json.RawMessage) (any, error) {
	return string(in), nil
}

func TestNewComposesIndependentParts(t *testing.T) {
	tools, err := NewToolBox(echoTool{})
	if err != nil {
		t.Fatal(err)
	}
	var a Agent = New(Config{
		Name:      "rag",
		LogDir:    t.TempDir(),
		Chat:      NewChatAgent(answerModel{"Three days. See [ship]."}, "Be brief."),
		Retriever: NewEmbeddingAgent(&wordEmbedder{model: "m1"}, nil),
		Tools:     tools,
	})
	defer a.Close()
	ctx := context.Background()
	if err := a.Ingest(ctx, Document{ID: "ship", Text: "shipping takes three days"}); err != nil {
		t.Fatal(err)
	}
	r, err := a.Ask(ctx, "shipping days", "")
	if err != nil || r.State["citation_valid"] != true {
		t.Fatalf("%+v %v", r.State, err)
	}
	if out, err := a.RunTool(ctx, "echo", json.RawMessage(`1`)); err != nil || out != "1" {
		t.Fatalf("tool %v %v", out, err)
	}
}

func TestMissingPartsReturnErrors(t *testing.T) {
	a := New(Config{Name: "empty", LogDir: t.TempDir()})
	defer a.Close()
	ctx := context.Background()
	if _, err := a.Chat(ctx, []model.Message{{Role: "user", Content: "x"}}, ""); err == nil {
		t.Error("chat without a client")
	}
	if _, err := a.Ask(ctx, "x", ""); err == nil {
		t.Error("ask without a client")
	}
	if err := a.Ingest(ctx, Document{ID: "a", Text: "x"}); err == nil {
		t.Error("ingest without an ingester")
	}
	if _, err := a.GetInformation(ctx, "x", 1); err == nil {
		t.Error("retrieve without a retriever")
	}
	if _, err := a.RunTool(ctx, "x", nil); err == nil {
		t.Error("tool without a runner")
	}
}

func TestToolBoxRejectsDuplicates(t *testing.T) {
	if _, err := NewToolBox(echoTool{}, echoTool{}); err == nil {
		t.Fatal("duplicate tools must fail")
	}
}

// Each part must work with no Template and swap for any implementation.
type recordingChatter struct{ messages []model.Message }

func (r *recordingChatter) Chat(_ context.Context, messages []model.Message, _ string) (model.Response, error) {
	r.messages = messages
	return model.Response{Text: "See [a]."}, nil
}

type fixedRetriever struct{}

func (fixedRetriever) Retrieve(context.Context, string, int) ([]Document, error) {
	return []Document{{ID: "a", Text: "fact"}}, nil
}

type splitStore struct {
	writes int
	hits   []Document
}

func (s *splitStore) Upsert(_ context.Context, r []VectorRecord) error {
	s.writes += len(r)
	return nil
}
func (s *splitStore) Search(context.Context, VectorQuery) ([]Document, error) {
	return s.hits, nil
}

func TestPartsWorkAloneAndArePluggable(t *testing.T) {
	ctx := context.Background()
	chat := &recordingChatter{}
	r := RAGAgent{Chat: chat, Retriever: fixedRetriever{}}
	out, err := r.Ask(ctx, "q", "")
	if err != nil || out.State["citation_valid"] != true || !strings.Contains(chat.messages[0].Content, "[a] fact") {
		t.Fatalf("RAGAgent with swapped parts: %+v %v", out.State, err)
	}

	store := &splitStore{hits: []Document{{ID: "x", Score: 0.9}, {ID: "y", Score: 0.1}}}
	embedder := &wordEmbedder{model: "m1"}
	indexer := Indexer{Embedder: embedder, Writer: store}
	if err := indexer.Ingest(ctx, Document{ID: "a", Text: "text"}); err != nil || store.writes != 1 {
		t.Fatalf("Indexer alone: %d %v", store.writes, err)
	}
	retriever := VectorRetriever{Embedder: embedder, Searcher: store, MinScore: 0.5}
	docs, err := retriever.Retrieve(ctx, "q", 2)
	if err != nil || len(docs) != 1 || docs[0].ID != "x" {
		t.Fatalf("VectorRetriever alone: %+v %v", docs, err)
	}

	box, _ := NewToolBox(echoTool{})
	if v, err := box.RunTool(ctx, "echo", json.RawMessage(`2`)); err != nil || v != "2" {
		t.Fatalf("ToolBox alone: %v %v", v, err)
	}
	if _, err := box.RunTool(ctx, "missing", nil); err == nil {
		t.Fatal("unknown tool must fail")
	}
}

func TestChatAgentMergesSystemMessagesIntoOne(t *testing.T) {
	c := NewChatAgent(nil, "Prompt.")
	out := c.BuildMessages(context.Background(), []model.Message{{Role: "system", Content: "Evidence."}, {Role: "user", Content: "q"}}, "")
	if len(out) != 2 || out[0].Role != "system" || out[0].Content != "Prompt.\nEvidence." || out[1].Content != "q" {
		t.Fatalf("%+v", out)
	}
	if out := NewChatAgent(nil, "").BuildMessages(context.Background(), []model.Message{{Role: "user", Content: "q"}}, ""); len(out) != 1 {
		t.Fatalf("no system message expected: %+v", out)
	}
}

func TestSeparateIngesterOverridesRetriever(t *testing.T) {
	store := &splitStore{}
	a := New(Config{Name: "split", LogDir: t.TempDir(), Retriever: fixedRetriever{}, Ingester: &Indexer{Embedder: &wordEmbedder{model: "m1"}, Writer: store}})
	defer a.Close()
	if err := a.Ingest(context.Background(), Document{ID: "a", Text: "x"}); err != nil || store.writes != 1 {
		t.Fatalf("%d %v", store.writes, err)
	}
}

func TestZeroMinScoreKeepsNegativeScores(t *testing.T) {
	store := &splitStore{hits: []Document{{ID: "x", Score: -0.2}}}
	r := VectorRetriever{Embedder: &wordEmbedder{model: "m1"}, Searcher: store}
	if docs, err := r.Retrieve(context.Background(), "q", 1); err != nil || len(docs) != 1 {
		t.Fatalf("%+v %v", docs, err)
	}
}
