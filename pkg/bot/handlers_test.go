package bot

import (
	"testing"
	"time"
)

// TestSanitizeWord covers the junk that actually made it into the vocabulary
// before the "Запомнить" flow normalized what it saved.
func TestSanitizeWord(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// real rows recovered from languagebot.db
		{"растение", "растение"},
		{"Дерево ?", "дерево"},
		{"А крыша ?", "крыша"},
		{"ребенок", "ребенок"},

		// lookup phrasings
		{"что значит окно", "окно"},
		{"как будет стул?", "стул"},
		{"а вот крыша", "крыша"},
		{"  СТОЛ  ", "стол"},

		// unresolvable locally → "" means "ask Claude / ask the user"
		{"А игра компьютерная", ""},
		{"это очень длинное предложение про всё сразу", ""},
		{"", ""},
		{"?!.", ""},
	}

	for _, c := range cases {
		if got := sanitizeWord(c.in); got != c.want {
			t.Errorf("sanitizeWord(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLooksLikePracticeAnswer covers the "did the user forget they're in
// practice?" heuristic that decides whether to offer to end practice.
func TestLooksLikePracticeAnswer(t *testing.T) {
	cases := []struct {
		mode string
		text string
		want bool
	}{
		// compose expects Japanese or romaji; a Russian word is a distraction
		{"practice_compose", "растение", false},
		{"practice_compose", "私は学生です", true},
		{"practice_compose", "watashi wa gakusei desu", true},
		{"practice_compose", "犬", true},
		{"practice_compose", "", false},

		// translate expects a Russian phrase; Japanese input is a distraction
		{"practice_translate", "я студент", true},
		{"practice_translate", "собака", true},
		{"practice_translate", "植物", false},
		{"practice_translate", "", false},
	}
	for _, c := range cases {
		if got := looksLikePracticeAnswer(c.mode, c.text); got != c.want {
			t.Errorf("looksLikePracticeAnswer(%q, %q) = %v, want %v", c.mode, c.text, got, c.want)
		}
	}
}

// TestIntervalFor pins the SRS schedule: growing intervals past the old 30-day
// ceiling, capped at 180 days, and never shrinking as the step rises.
func TestIntervalFor(t *testing.T) {
	day := 24 * time.Hour
	fixed := map[int]time.Duration{
		0: 3 * time.Hour, 1: 3 * time.Hour, 2: day, 3: 7 * day,
		4: 30 * day, 5: 60 * day, 6: 120 * day, 7: 180 * day, 42: 180 * day,
	}
	for step, want := range fixed {
		if got := intervalFor(step); got != want {
			t.Errorf("intervalFor(%d) = %v, want %v", step, got, want)
		}
	}
	for step := 1; step < 12; step++ {
		if intervalFor(step+1) < intervalFor(step) {
			t.Errorf("intervalFor must not shrink: step %d→%d went %v→%v",
				step, step+1, intervalFor(step), intervalFor(step+1))
		}
	}
}

// TestSplitHeadword pins how a translation line is taken apart — the written
// form goes to Jisho and to TTS, the reading is compared with the dictionary.
func TestSplitHeadword(t *testing.T) {
	cases := []struct{ in, written, reading string }{
		{"屋根(やね) (yane)", "屋根", "やね"},
		{"遊(あそ)び (asobi)", "遊び", "あそび"}, // okurigana stays in the written form
		{"植物(しょくぶつ) (shokubutsu)", "植物", "しょくぶつ"},
		{"食(た)べ物(もの) (tabemono)", "食べ物", "たべもの"}, // two kanji runs
		{"ありがとう (arigatou)", "ありがとう", "ありがとう"},   // kana-only word
		{"", "", ""},
		{"just latin", "", ""},
	}
	for _, c := range cases {
		w, r := splitHeadword(c.in)
		if w != c.written || r != c.reading {
			t.Errorf("splitHeadword(%q) = (%q, %q), want (%q, %q)", c.in, w, r, c.written, c.reading)
		}
	}
	if got := japaneseHeadword("屋根(やね) (yane)"); got != "屋根" {
		t.Errorf("japaneseHeadword = %q, want 屋根", got)
	}
}

// TestLapseStep — a wrong answer drops two steps (floor 1), not a full reset.
func TestLapseStep(t *testing.T) {
	cases := map[int]int{1: 1, 2: 1, 3: 1, 4: 2, 5: 3, 6: 4, 7: 5}
	for step, want := range cases {
		if got := lapseStep(step); got != want {
			t.Errorf("lapseStep(%d) = %d, want %d", step, got, want)
		}
	}
}
