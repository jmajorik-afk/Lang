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
