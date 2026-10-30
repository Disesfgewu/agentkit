package agent

import (
	"context"
	"errors"
	"github.com/Disesfgewu/agentkit/agentlog"
	"github.com/Disesfgewu/agentkit/model"
	"strings"
)

// Chatter sends messages to a model. It is the only thing RAGAgent needs from a
// chat component, so any implementation can replace ChatAgent.
type Chatter interface {
	Chat(ctx context.Context, messages []model.Message, reasoning string) (model.Response, error)
}

// ChatAgent applies a system prompt and calls a chat client. It works alone.
type ChatAgent struct {
	Client       model.Client
	SystemPrompt string
	// MaxOutputTokens is forwarded to the provider; zero uses the client's default.
	MaxOutputTokens int
}

func NewChatAgent(client model.Client, systemPrompt string) *ChatAgent {
	return &ChatAgent{Client: client, SystemPrompt: systemPrompt}
}

// BuildMessages merges the system prompt, an optional instruction, and any leading
// system messages into one system message, because some providers reject several.
func (c *ChatAgent) BuildMessages(ctx context.Context, messages []model.Message, instruction string) (result []model.Message) {
	_, span := agentlog.Start(ctx, "agent.ChatAgent.BuildMessages", map[string]any{"system_prompt": c.SystemPrompt, "instruction": instruction, "messages": messages})
	defer span.Finish(nil, func() any { return result }, nil)
	span.Require()
	system := []string{c.SystemPrompt, instruction}
	rest := messages
	for len(rest) > 0 && rest[0].Role == "system" {
		system = append(system, rest[0].Content)
		rest = rest[1:]
	}
	parts := make([]string, 0, len(system))
	for _, part := range system {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	result = append([]model.Message(nil), rest...)
	if len(parts) > 0 {
		result = append([]model.Message{{Role: "system", Content: strings.Join(parts, "\n")}}, result...)
	}
	return result
}

func (c *ChatAgent) Chat(ctx context.Context, messages []model.Message, reasoning string) (response model.Response, err error) {
	ctx, span := agentlog.Start(ctx, "agent.ChatAgent.Chat", map[string]any{"messages": messages, "reasoning": reasoning})
	defer span.Finish(&err, func() any { return response }, func() *int {
		if response.UsageKnown {
			n := response.InputTokens + response.OutputTokens
			return &n
		}
		return nil
	})
	if err = span.Err(); err != nil {
		return
	}
	if c.Client == nil {
		return model.Response{}, errors.New("model not configured")
	}
	return c.Client.Chat(ctx, model.Request{Messages: c.BuildMessages(ctx, messages, ""), Reasoning: reasoning, MaxOutputTokens: c.MaxOutputTokens})
}
