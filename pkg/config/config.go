package config

import (
	"os"
	"path/filepath"
	"strings"
	template "text/template"
)

type ChatMessage struct {
	Role    string
	Content string
}

type GptPromptTuningByLanguageAndHelpType map[string]map[string]GptPromptTuning

type GptPromptTuning struct {
	Language string
	HelpType string
	Messages []ChatMessage
}

type GptRequestType struct {
	HelpType       string
	PromptTemplate *template.Template
}

type TTSConfig struct {
	Voice string
	Speed float64
}

type Config struct {
	GptTemplateWordUsageExamples *GptRequestType
	GptTemplateWordTranslation   *GptRequestType
	GptTemplateInflection        *GptRequestType
	GptPromptTunings             GptPromptTuningByLanguageAndHelpType
	TTSConfig                    *TTSConfig
}

func NewGptPromptTuningFromTextFiles() (GptPromptTuningByLanguageAndHelpType, error) {
	promptTunings := make(GptPromptTuningByLanguageAndHelpType)

	helpTypeDirectory, err := os.ReadDir("templates/")
	if err != nil {
		return nil, err
	}

	for _, file := range helpTypeDirectory {
		if file.IsDir() {
			helpType := file.Name()
			subDir, err := os.ReadDir(filepath.Join("templates", helpType))
			if err != nil {
				return nil, err
			}
			for _, f := range subDir {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".txt") {
					language := strings.TrimSuffix(f.Name(), ".txt")
					filePath := filepath.Join("templates", helpType, f.Name())
					content, err := os.ReadFile(filePath)
					if err != nil {
						return nil, err
					}
					messages := parseChatMessages(content)
					promptTuning := GptPromptTuning{
						Language: language,
						HelpType: helpType,
						Messages: messages,
					}
					if _, ok := promptTunings[language]; !ok {
						promptTunings[language] = make(map[string]GptPromptTuning)
					}
					promptTunings[language][helpType] = promptTuning
				}
			}
		}
	}
	return promptTunings, nil
}

func parseChatMessages(content []byte) []ChatMessage {
	var messages []ChatMessage
	for _, line := range strings.Split(string(content), "\n") {
		role, text, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		role = strings.TrimSpace(role)
		text = strings.TrimSpace(text)
		messages = append(messages, ChatMessage{
			Role:    role,
			Content: strings.Replace(text, `\n`, "\n", -1),
		})
	}
	return messages
}

func NewConfig() *Config {
	gptPromptTunings, err := NewGptPromptTuningFromTextFiles()
	if err != nil {
		panic(err)
	}

	return &Config{
		GptPromptTunings: gptPromptTunings,
		GptTemplateWordUsageExamples: &GptRequestType{
			HelpType:       "examples",
			PromptTemplate: template.Must(template.ParseFiles("templates/examples.txt")),
		},
		GptTemplateWordTranslation: &GptRequestType{
			HelpType:       "translation",
			PromptTemplate: template.Must(template.ParseFiles("templates/translation.txt")),
		},
		GptTemplateInflection: &GptRequestType{
			HelpType:       "inflection",
			PromptTemplate: template.Must(template.ParseFiles("templates/inflection.txt")),
		},
		TTSConfig: &TTSConfig{
			Voice: "nova",
			Speed: 1,
		},
	}
}
