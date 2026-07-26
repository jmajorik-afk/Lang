package bot

import "testing"

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
