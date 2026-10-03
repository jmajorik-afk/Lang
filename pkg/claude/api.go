package claude_api

import (
	"context"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// requestTimeout bounds one model call; the SDK retries transient failures
// (429/5xx/network) with backoff up to maxRetries times inside that budget.
const (
	requestTimeout = 60 * time.Second
	maxRetries     = 3
)

func NewClient(apiKey string) anthropic.Client {
	return anthropic.NewClient(option.WithAPIKey(apiKey), option.WithMaxRetries(maxRetries))
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
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

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
		Model:       anthropic.ModelClaudeSonnet4_6,
		MaxTokens:   2048,
		Temperature: anthropic.Float(0.3),
		Messages:    msgParams,
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
