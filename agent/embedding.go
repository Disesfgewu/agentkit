package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/Disesfgewu/agentkit/agentlog"
	"github.com/Disesfgewu/agentkit/model"
	"math"
	"sort"
	"strings"
	"sync"
)

const (
	defaultChunkRunes = 1500
	defaultBatchSize  = 32
)

// ErrEmbeddingMismatch reports vectors from a different model or dimension than
// the index. Comparing them would return confident but meaningless results.
var ErrEmbeddingMismatch = errors.New("embedding model does not match the index")

type VectorRecord struct {
	Document
	Vector         []float32
	EmbeddingModel string
}

type VectorQuery struct {
	Vector         []float32
	EmbeddingModel string
	K              int
}

// VectorWriter and VectorSearcher are the storage seams. Implement one or both to
// use a vector database. Search returns documents with Score set, best first.
type VectorWriter interface {
	Upsert(context.Context, []VectorRecord) error
}

type VectorSearcher interface {
	Search(context.Context, VectorQuery) ([]Document, error)
}

type VectorStore interface {
	VectorWriter
	VectorSearcher
}

// MemoryVectorStore is an exact cosine search over memory. It does not survive
// a restart, so documents must be ingested again.
type MemoryVectorStore struct {
	mu      sync.RWMutex
	records map[string]VectorRecord
	model   string
	dim     int
}

func (s *MemoryVectorStore) Upsert(_ context.Context, records []VectorRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	indexModel, dim := s.model, s.dim
	for _, r := range records {
		if indexModel == "" {
			indexModel, dim = r.EmbeddingModel, len(r.Vector)
		}
		if r.EmbeddingModel != indexModel || len(r.Vector) != dim {
			return fmt.Errorf("%w: index %s/%d, record %s/%d", ErrEmbeddingMismatch, indexModel, dim, r.EmbeddingModel, len(r.Vector))
		}
	}
	if s.records == nil {
		s.records = map[string]VectorRecord{}
	}
	s.model, s.dim = indexModel, dim
	for _, r := range records {
		s.records[r.ID] = r
	}
	return nil
}

func (s *MemoryVectorStore) Search(_ context.Context, q VectorQuery) ([]Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.records) == 0 {
		return nil, nil
	}
	if q.EmbeddingModel != s.model || len(q.Vector) != s.dim {
		return nil, fmt.Errorf("%w: index %s/%d, query %s/%d", ErrEmbeddingMismatch, s.model, s.dim, q.EmbeddingModel, len(q.Vector))
	}
	hits := make([]Document, 0, len(s.records))
	for _, r := range s.records {
		d := r.Document
		d.Score = cosine(q.Vector, r.Vector)
		hits = append(hits, d)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	if q.K > 0 && len(hits) > q.K {
		hits = hits[:q.K]
	}
	return hits, nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

// Indexer chunks, embeds, and writes documents. It works alone.
type Indexer struct {
	Embedder model.Embedder
	Writer   VectorWriter
	// Chunk splits a document before embedding. Nil splits on paragraphs at
	// ChunkRunes; zero ChunkRunes uses the default.
	Chunk      func(Document) []Document
	ChunkRunes int
	// BatchSize is the number of chunks per provider call.
	BatchSize int
}

// VectorRetriever embeds a query and searches an index. It works alone.
type VectorRetriever struct {
	Embedder model.Embedder
	Searcher VectorSearcher
	// MinScore drops weak matches, so Ask can report insufficient evidence
	// rather than cite the nearest unrelated text. Zero keeps every match.
	MinScore float64
}

// EmbeddingAgent is an Indexer and a VectorRetriever sharing one embedder and store.
type EmbeddingAgent struct {
	Indexer
	VectorRetriever
}

func NewEmbeddingAgent(embedder model.Embedder, store VectorStore) *EmbeddingAgent {
	if store == nil {
		store = &MemoryVectorStore{}
	}
	return &EmbeddingAgent{Indexer: Indexer{Embedder: embedder, Writer: store}, VectorRetriever: VectorRetriever{Embedder: embedder, Searcher: store}}
}

// Ingest chunks and indexes documents. Chunks are upserted by ID batch by batch,
// so rerunning after a failure repeats work but leaves no duplicates.
func (e *Indexer) Ingest(ctx context.Context, docs ...Document) (err error) {
	ctx, span := agentlog.Start(ctx, "agent.Indexer.Ingest", map[string]any{"documents": len(docs)})
	chunks := []Document{}
	defer span.Finish(&err, func() any { return map[string]any{"chunks": len(chunks)} }, nil)
	if err = span.Err(); err != nil {
		return
	}
	for _, d := range docs {
		if e.Chunk != nil {
			chunks = append(chunks, e.Chunk(d)...)
		} else {
			chunks = append(chunks, chunkDocument(d, e.ChunkRunes)...)
		}
	}
	size := e.BatchSize
	if size <= 0 {
		size = defaultBatchSize
	}
	for start := 0; start < len(chunks); start += size {
		batch := chunks[start:min(start+size, len(chunks))]
		texts := make([]string, len(batch))
		for i, d := range batch {
			texts[i] = d.Text
		}
		response, err := e.Embedder.Embed(ctx, model.EmbeddingRequest{Input: texts, InputType: model.InputDocument})
		if err != nil {
			return err
		}
		if len(response.Vectors) != len(batch) {
			return fmt.Errorf("embedder returned %d vectors for %d chunks", len(response.Vectors), len(batch))
		}
		records := make([]VectorRecord, len(batch))
		for i, d := range batch {
			records[i] = VectorRecord{Document: d, Vector: response.Vectors[i], EmbeddingModel: response.Model}
		}
		if err := e.Writer.Upsert(ctx, records); err != nil {
			return err
		}
	}
	return nil
}

func (e *VectorRetriever) Retrieve(ctx context.Context, q string, k int) (documents []Document, err error) {
	ctx, span := agentlog.Start(ctx, "agent.VectorRetriever.Retrieve", map[string]any{"query": q, "k": k})
	defer span.Finish(&err, func() any { return documents }, nil)
	if err = span.Err(); err != nil {
		return
	}
	if k <= 0 {
		return nil, nil
	}
	response, err := e.Embedder.Embed(ctx, model.EmbeddingRequest{Input: []string{q}, InputType: model.InputQuery})
	if err != nil {
		return nil, err
	}
	if len(response.Vectors) != 1 {
		return nil, fmt.Errorf("embedder returned %d vectors for one query", len(response.Vectors))
	}
	hits, err := e.Searcher.Search(ctx, VectorQuery{Vector: response.Vectors[0], EmbeddingModel: response.Model, K: k})
	if err != nil {
		return nil, err
	}
	documents = []Document{}
	for _, d := range hits {
		if e.MinScore == 0 || d.Score >= e.MinScore {
			documents = append(documents, d)
		}
	}
	return documents, nil
}

// chunkDocument splits on blank lines and hard-splits oversized paragraphs. It
// counts runes, so Chinese text is not cut inside a character.
func chunkDocument(d Document, maxRunes int) []Document {
	if maxRunes <= 0 {
		maxRunes = defaultChunkRunes
	}
	text := strings.TrimSpace(d.Text)
	if text == "" {
		return nil
	}
	if len([]rune(text)) <= maxRunes {
		d.Text = text
		return []Document{d}
	}
	parts := []string{}
	current := ""
	flush := func() {
		if current != "" {
			parts = append(parts, current)
			current = ""
		}
	}
	for _, paragraph := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		runes := []rune(paragraph)
		if len([]rune(current))+len(runes)+2 > maxRunes {
			flush()
		}
		for len(runes) > maxRunes {
			parts = append(parts, string(runes[:maxRunes]))
			runes = runes[maxRunes:]
		}
		if current != "" {
			current += "\n\n"
		}
		current += string(runes)
	}
	flush()
	chunks := make([]Document, len(parts))
	for i, part := range parts {
		chunks[i] = Document{ID: fmt.Sprintf("%s#%d", d.ID, i+1), Text: part, Source: d.Source}
	}
	return chunks
}
