package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/Disesfgewu/agentkit/agentlog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

type LocalRetriever struct{ Documents []Document }

func LoadMarkdown(paths []string) (*LocalRetriever, error) {
	return LoadMarkdownContext(context.Background(), paths)
}
func LoadMarkdownContext(ctx context.Context, paths []string) (retriever *LocalRetriever, err error) {
	_, span := agentlog.Start(ctx, "agent.LoadMarkdown", paths)
	defer span.Finish(&err, func() any { return retriever }, nil)
	if err = span.Err(); err != nil {
		return
	}
	r := &LocalRetriever{}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		lines := strings.Split(string(b), "\n")
		title := ""
		start := 1
		flush := func(end int) {
			if end < start {
				return
			}
			t := strings.TrimSpace(strings.Join(lines[start-1:end], "\n"))
			if t != "" {
				r.Documents = append(r.Documents, Document{ID: fmt.Sprintf("%s:%d:%x", filepath.Base(path), start, sum[:4]), Text: title + "\n" + t, Source: fmt.Sprintf("%s#L%d-L%d", path, start, end)})
			}
		}
		for i, line := range lines {
			if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
				flush(i)
				title = line
				start = i + 1
			}
		}
		flush(len(lines))
	}
	return r, nil
}
func tokens(ctx context.Context, s string) (result []string) {
	_, span := agentlog.Start(ctx, "agent.tokens", s)
	defer span.Finish(nil, func() any { return result }, nil)
	span.Require()
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' })
}
func (r *LocalRetriever) Retrieve(ctx context.Context, q string, k int) (documents []Document, err error) {
	ctx, span := agentlog.Start(ctx, "agent.LocalRetriever.Retrieve", map[string]any{"query": q, "k": k, "document_count": len(r.Documents)})
	defer span.Finish(&err, func() any { return documents }, nil)
	if err = span.Err(); err != nil {
		return
	}
	if k <= 0 {
		return nil, nil
	}
	want := map[string]bool{}
	for _, v := range tokens(ctx, q) {
		want[v] = true
	}
	type hit struct {
		d Document
		n int
	}
	hits := []hit{}
	for _, d := range r.Documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := 0
		for _, v := range tokens(ctx, d.Text) {
			if want[v] {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, hit{d, n})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].n > hits[j].n })
	if len(hits) > k {
		hits = hits[:k]
	}
	out := []Document{}
	for _, h := range hits {
		out = append(out, h.d)
	}
	return out, nil
}
