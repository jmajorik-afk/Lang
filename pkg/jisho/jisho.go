// Package jisho is a thin client for the public Jisho.org dictionary API. It is
// used to verify the Japanese side of a translation the model produced —
// mainly that the word exists and that the kanji reading is the dictionary one.
// Jisho only understands English and Japanese input, so it cannot translate
// from Russian; the model does that, Jisho checks the result.
package jisho

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// BaseURL is overridable for tests.
var BaseURL = "https://jisho.org/api/v1/search/words"

var httpClient = &http.Client{Timeout: 8 * time.Second}

// Entry is one written form of a dictionary word.
type Entry struct {
	Word    string   // kanji/written form; empty for kana-only words
	Reading string   // kana reading
	English []string // first sense's definitions
}

type apiResponse struct {
	Data []struct {
		Japanese []struct {
			Word    string `json:"word"`
			Reading string `json:"reading"`
		} `json:"japanese"`
		Senses []struct {
			English []string `json:"english_definitions"`
		} `json:"senses"`
	} `json:"data"`
}

// Lookup queries Jisho for keyword and flattens every written form of every
// result into entries. One retry on transient failure.
func Lookup(ctx context.Context, keyword string) ([]Entry, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		entries, err := lookupOnce(ctx, keyword)
		if err == nil {
			return entries, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func lookupOnce(ctx context.Context, keyword string) ([]Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"?keyword="+url.QueryEscape(keyword), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jisho: HTTP %d", resp.StatusCode)
	}
	var parsed apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	var out []Entry
	for _, d := range parsed.Data {
		var english []string
		if len(d.Senses) > 0 {
			english = d.Senses[0].English
		}
		for _, j := range d.Japanese {
			out = append(out, Entry{Word: j.Word, Reading: j.Reading, English: english})
		}
	}
	return out, nil
}

// VerifyReading returns the dictionary reading of the given written form, or
// "" when Jisho has no such word. A kana-only written form is verified when it
// equals the reading of any entry: Jisho often lists such words only under a
// rarely used kanji spelling (ありがとう appears as 有り難う / 有難う), so
// requiring a kanji-less variant would wrongly fail them. A kanji string never
// equals a kana reading, so this second pass cannot produce false matches.
func VerifyReading(entries []Entry, written string) string {
	for _, e := range entries {
		if e.Word == written {
			return e.Reading
		}
	}
	for _, e := range entries {
		if e.Reading == written {
			return e.Reading
		}
	}
	return ""
}
