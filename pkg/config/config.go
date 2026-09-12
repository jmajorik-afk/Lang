package config

import (
	"os"
	"strings"
	"sync"
)

type ChatMessage struct {
	Role    string
	Content string
}

type Config struct {
	WordSystemPrompt string
	FewShot          []ChatMessage
}

var (
	cfg  *Config
	once sync.Once
)

// Load reads the prompt files once and caches them (no per-request disk IO).
func Load() *Config {
	once.Do(func() {
		cfg = &Config{
			WordSystemPrompt: mustRead("templates/word.txt"),
			FewShot:          parseFewShot(mustRead("templates/word_fewshot.txt")),
		}
	})
	return cfg
}

func mustRead(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// parseFewShot reads "role: content" lines into chat messages. \n in content
// is turned into a real newline.
func parseFewShot(content string) []ChatMessage {
	var msgs []ChatMessage
	for _, line := range strings.Split(content, "\n") {
		role, text, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		role = strings.TrimSpace(role)
		if role != "user" && role != "assistant" {
			continue
		}
		msgs = append(msgs, ChatMessage{
			Role:    role,
			Content: strings.ReplaceAll(strings.TrimSpace(text), `\n`, "\n"),
		})
	}
	return msgs
}
