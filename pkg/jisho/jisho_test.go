package jisho

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A trimmed real-shaped Jisho response for 屋根 (roof).
const sampleJSON = `{"meta":{"status":200},"data":[
  {"slug":"屋根","japanese":[{"word":"屋根","reading":"やね"}],
   "senses":[{"english_definitions":["roof"]}]},
  {"slug":"ありがとう","japanese":[{"reading":"ありがとう"},{"word":"有り難う","reading":"ありがとう"}],
   "senses":[{"english_definitions":["thank you"]}]}
]}`

func TestLookupParsesEveryWrittenForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("keyword"); got != "屋根" {
			t.Errorf("keyword = %q, want 屋根", got)
		}
		w.Write([]byte(sampleJSON))
	}))
	defer srv.Close()
	BaseURL = srv.URL

	entries, err := Lookup(context.Background(), "屋根")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (one per written form)", len(entries))
	}
	if entries[0].Word != "屋根" || entries[0].Reading != "やね" || entries[0].English[0] != "roof" {
		t.Errorf("first entry = %+v", entries[0])
	}
}

func TestVerifyReading(t *testing.T) {
	// Mirrors real Jisho output: ありがとう has NO kanji-less variant, only the
	// rare kanji spellings — a kana-only lookup must still verify via reading.
	entries := []Entry{
		{Word: "屋根", Reading: "やね"},
		{Word: "有り難う", Reading: "ありがとう"},
		{Word: "有難う", Reading: "ありがとう"},
	}
	cases := map[string]string{
		"屋根":      "やね",    // kanji form found
		"ありがとう":   "ありがとう", // kana-only form matches by reading
		"有り難う":    "ありがとう",
		"存在しない単語": "", // not in dictionary → cannot verify
	}
	for written, want := range cases {
		if got := VerifyReading(entries, written); got != want {
			t.Errorf("VerifyReading(%q) = %q, want %q", written, got, want)
		}
	}
}

func TestLookupRetriesThenFails(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	BaseURL = srv.URL

	if _, err := Lookup(context.Background(), "x"); err == nil {
		t.Fatal("expected an error after retries")
	}
	if calls != 2 {
		t.Errorf("made %d requests, want 2 (one retry)", calls)
	}
}
