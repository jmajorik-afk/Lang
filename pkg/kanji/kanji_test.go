package kanji

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A trimmed real kanjiapi.dev response for 物.
const sampleJSON = `{"kanji":"物","grade":3,"stroke_count":8,"meanings":["thing","object","matter"],
  "kun_readings":["もの","もの-"],"on_readings":["ブツ","モツ"],"jlpt":4}`

func TestMeanings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/kanji/物":
			w.Write([]byte(sampleJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	BaseURL = srv.URL + "/v1/kanji/"

	m, err := Meanings(context.Background(), "物")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 || m[0] != "thing" {
		t.Errorf("meanings = %v, want [thing object matter]", m)
	}

	if _, err := Meanings(context.Background(), "𠮷"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown character: err = %v, want ErrNotFound", err)
	}
}

func TestMeaningsReportsServerTrouble(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	BaseURL = srv.URL + "/"

	_, err := Meanings(context.Background(), "物")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a transient error, not ErrNotFound", err)
	}
	if calls != 2 {
		t.Errorf("made %d calls, want 2 (one retry)", calls)
	}
}
