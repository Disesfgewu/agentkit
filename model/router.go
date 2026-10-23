package model

import (
	"context"
	"errors"
	"fmt"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agentlog"
	"sort"
)

type Entry struct {
	Config Config
	Client Client
}
type Router struct{ Entries []Entry }

func (r Router) Chat(ctx context.Context, req Request) (response Response, err error) {
	ctx, span := agentlog.Start(ctx, "model.Router.Chat", req)
	defer func() {
		var tokens *int
		if response.UsageKnown {
			n := response.InputTokens + response.OutputTokens
			tokens = &n
		}
		span.End(response, tokens, err)
	}()
	entries := append([]Entry(nil), r.Entries...)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Config.Tier < entries[j].Config.Tier })
	estimated := 0
	for _, m := range req.Messages {
		estimated += (len(m.Content)+3)/4 + 8
	}
	var failures []error
	for _, e := range entries {
		if e.Config.ContextTokens > 0 && estimated+req.MaxOutputTokens > e.Config.ContextTokens {
			continue
		}
		if e.Client == nil {
			continue
		}
		resp, err := e.Client.Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", e.Config.Name, err))
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
	}
	if len(failures) == 0 {
		return Response{}, errors.New("no eligible model")
	}
	return Response{}, errors.Join(failures...)
}
