package openai_api

import (
	"context"
	"io"
	"log"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// go-openai has no built-in retries, so one transient failure used to mean no
// audio. Each attempt is bounded by a timeout and retried once after a pause.
const (
	requestTimeout = 30 * time.Second
	attempts       = 2
)

func GetTTSResponse(ctx context.Context, openaiClient *openai.Client, speechSpeed float64, req string) ([]byte, error) {
	request := openai.CreateSpeechRequest{
		Model: openai.TTSModel1,
		Input: req,
		Voice: openai.VoiceNova,
		Speed: speechSpeed,
	}
	log.Printf("GetTTSResponse request: speed=%.1f req=%s", speechSpeed, req)

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		body, err := speakOnce(ctx, openaiClient, request)
		if err == nil {
			return body, nil
		}
		lastErr = err
		log.Printf("TTS attempt %d/%d failed: %v", attempt, attempts, err)
	}
	return nil, lastErr
}

func speakOnce(ctx context.Context, client *openai.Client, request openai.CreateSpeechRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	response, err := client.CreateSpeech(ctx, request)
	if err != nil {
		return nil, err
	}
	defer response.Close()
	return io.ReadAll(response)
}
