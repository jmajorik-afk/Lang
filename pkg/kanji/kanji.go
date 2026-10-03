// Package kanji is a thin client for kanjiapi.dev, a free JSON API over
// KANJIDIC — the reference dictionary of single kanji. It supplies the English
// meanings of one character; the bot glosses them into Russian, so the meaning
// shown next to a word comes from a dictionary rather than from the model's
// memory. Like Jisho, it does not understand Russian.
package kanji

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// BaseURL is overridable for tests.
var BaseURL = "https://kanjiapi.dev/v1/kanji/"

var httpClient = &http.Client{Timeout: 8 * time.Second}

// ErrNotFound means the dictionary has no such character.
var ErrNotFound = errors.New("kanji: not in the dictionary")

// Meanings returns the dictionary's English meanings of one kanji. One retry on
// a transient failure; a character the dictionary doesn't have is not retried.
func Meanings(ctx context.Context, char string) ([]string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		m, err := meaningsOnce(ctx, char)
		if err == nil || errors.Is(err, ErrNotFound) {
			return m, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func meaningsOnce(ctx context.Context, char string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+url.PathEscape(char), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kanji: HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Meanings []string `json:"meanings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if len(parsed.Meanings) == 0 {
		return nil, ErrNotFound
	}
	return parsed.Meanings, nil
}
