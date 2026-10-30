package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Disesfgewu/agentkit/agentlog"
)

// ToolRunner calls a named tool.
type ToolRunner interface {
	RunTool(ctx context.Context, name string, input json.RawMessage) (any, error)
}

// ToolBox is a ToolRunner over a fixed set of registered tools. It works alone.
type ToolBox map[string]Tool

func NewToolBox(tools ...Tool) (ToolBox, error) {
	box := make(ToolBox, len(tools))
	for _, tool := range tools {
		if _, exists := box[tool.Name()]; exists {
			return nil, fmt.Errorf("duplicate tool %s", tool.Name())
		}
		box[tool.Name()] = tool
	}
	return box, nil
}

func (b ToolBox) RunTool(ctx context.Context, name string, input json.RawMessage) (any, error) {
	t, ok := b[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %s", name)
	}
	return agentlog.Call(ctx, "agent.Tool."+name, input, func(ctx context.Context) (any, error) { return t.Run(ctx, input) })
}
