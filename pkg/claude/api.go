package claude_api

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func NewClient(apiKey string) anthropic.Client {
	return anthropic.NewClient(option.WithAPIKey(apiKey))
}

type ClaudeRequest struct {
	SystemPrompt string
	Messages     []ChatMessage
	UserMessage  string
}

type ChatMessage struct {
	Role    string
	Content string
}

func GetClaudeResponse(ctx context.Context, client *anthropic.Client, req ClaudeRequest) (string, error) {
	var msgParams []anthropic.MessageParam

	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			msgParams = append(msgParams, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case "assistant":
			msgParams = append(msgParams, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		}
	}
	msgParams = append(msgParams, anthropic.NewUserMessage(anthropic.NewTextBlock(req.UserMessage)))

	params := anthropic.MessageNewParams{
		Model:     anthropic.ModelClaude3_5HaikuLatest,
		MaxTokens: 1024,
		Messages:  msgParams,
	}
	if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.SystemPrompt}}
	}

	msg, err := client.Messages.New(ctx, params)
	if err != nil {
		return "", err
	}

	return msg.Content[0].Text, nil
}
