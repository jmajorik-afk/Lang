package openai_api

import (
	"context"
	"io"
	"log"

	openai "github.com/sashabaranov/go-openai"
)

func GetTTSResponse(ctx context.Context, openaiClient *openai.Client, speechSpeed float64, req string) ([]byte, error) {
	request := openai.CreateSpeechRequest{
		Model: openai.TTSModel1,
		Input: req,
		Voice: openai.VoiceNova,
		Speed: speechSpeed,
	}
	log.Printf("GetTTSResponse request: speed=%.1f req=%s", speechSpeed, req)
	response, err := openaiClient.CreateSpeech(ctx, request)
	if err != nil {
		log.Println("error when requesting TTS api")
		return nil, err
	}
	defer response.Close()

	body, err := io.ReadAll(response)
	if err != nil {
		log.Println("error when reading TTS response body")
		return nil, err
	}
	return body, nil
}
