package bot

import (
	"testing"

	"language-learning-bot/pkg/storage"
)

// TestTidy — the lines are the bot's own, from the chat and the QA run.
func TestTidy(t *testing.T) {
	for in, want := range map[string]string{
		// romaji checked against the reading shown with it, long vowels in two letters
		"ブルーベリー (buruberii) — черника, голубика": "ブルーベリー (buruuberii) — черника, голубика",
		"トイレットペーパー (toiretto pēpā)":              "トイレットペーパー (toiretto peepaa)",
		"1. кофе — コーヒー (kōhī)":                  "1. кофе — コーヒー (koohii)",
		"学校(がっこう) (gakkō)":                       "学校(がっこう) (gakkou)",
		// a reading after kana that repeats it, and Markdown
		"の(の)る (noru) означает «садиться»":      "のる (noru) означает «садиться»",
		"Правильно: ブルーベリー(ぶるーべりー) (burūberī).": "Правильно: ブルーベリー (buruuberii).",
		"нужно ブル**ウ**, а не ブル":                "нужно ブルウ, а не ブル",
		"## Заголовок\n* пункт":                 "Заголовок\n• пункт",
		// right as they are
		"犬(いぬ) (inu) — собака":              "犬(いぬ) (inu) — собака",
		"元気(げんき)な (genki na)":               "元気(げんき)な (genki na)",
		"大丈夫(だいじょうぶ)ですか (daijoubu desu ka)": "大丈夫(だいじょうぶ)ですか (daijoubu desu ka)",
		"食(た)べ物(もの) (tabemono)":             "食(た)べ物(もの) (tabemono)",
		"こんにちは (konnichiwa)":                "こんにちは (konnichiwa)", // は read as wa
		// sentences and words without a full reading are never re-read
		"Пример: 猫(ねこ)が 窓(まど)の 上(うえ)に いる。(neko ga mado no ue ni iru.)": "Пример: 猫(ねこ)が 窓(まど)の 上(うえ)に いる。(neko ga mado no ue ni iru.)",
		"トイレットペーパーが ない (toiretto peepaa ga nai)":                       "トイレットペーパーが ない (toiretto peepaa ga nai)",
		"新しい (atarashii)":   "新しい (atarashii)", // 新 has no reading: nothing to check against
		"✓ ゆ — это ユ (yu).": "✓ ゆ — это ユ (yu).",
	} {
		if got := tidy(in); got != want {
			t.Errorf("tidy(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

// TestStoredEntryAcceptsAnyRomaji — a word saved as «pēpā» is answered «peepaa».
func TestStoredEntryAcceptsAnyRomaji(t *testing.T) {
	entry := storage.VocabEntry{Translation: "トイレットペーパー (toiretto pēpā)"}
	for _, a := range []string{"toiretto peepaa", "toirettopeepaa", "toiretto pēpā", "トイレットペーパー"} {
		if !matchesStoredEntry(entry, a) {
			t.Errorf("%q must match the stored entry", a)
		}
	}
	if matchesStoredEntry(entry, "toiretto pepa") {
		t.Error("short vowels are a different answer")
	}
	school := storage.VocabEntry{Translation: "学校(がっこう) (gakkou)"}
	for _, a := range []string{"gakkou", "gakkoo", "gakkō"} {
		if !matchesStoredEntry(school, a) {
			t.Errorf("%q must match gakkou", a)
		}
	}
}

func TestNormalizeWord(t *testing.T) {
	for in, want := range map[string]string{
		"как будешь карандаш":            "карандаш", // a slip in «как будет» made it an entry
		"Как будет карандаш?":            "карандаш",
		"как будут собаки":               "собаки",
		"как по-японски будет карандаш":  "карандаш",
		"как по японски карандаш":        "карандаш",
		"как сказать спасибо по-японски": "спасибо",
		"карандаш на японском":           "карандаш",
		"что такое кайдан":               "кайдан",
		"что значит 猫?":                  "猫",
		"переведи карандаш":              "карандаш",
		"перевод окна":                   "окна",
		"а как будет крыша":              "а крыша", // the filler goes later, in sanitizePhrase
		"как дела":                       "как дела",
		"как будто":                      "как будто",
		"переводчик":                     "переводчик",
		"как будет":                      "",
	} {
		if got := normalizeWord(in); got != want {
			t.Errorf("normalizeWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVocabKey(t *testing.T) {
	for raw, want := range map[string]string{
		"Как будет туалетная бумага": "туалетная бумага",
		"в порядке":                "в порядке",
		"А крыша ?":                "крыша",
		"я люблю кошек":            "", // a clause, not an entry
		"это очень большая собака": "", // four words before the filler is peeled
	} {
		if got := vocabKey(raw); got != want {
			t.Errorf("vocabKey(%q) = %q, want %q", raw, got, want)
		}
	}
	for key, want := range map[string]bool{
		"туалетная бумага": true, "в порядке": true,
		"トイレットペーパー": false, // the reminder would ask how to say it in Japanese
		"beautiful": false, "猫 кошка": false, "": false,
	} {
		if got := russianKey(key); got != want {
			t.Errorf("russianKey(%q) = %v, want %v", key, got, want)
		}
	}
	// clean Russian is resolved without asking the model
	if got := resolveHeadword(nil, "туалетная бумага"); got != "туалетная бумага" {
		t.Errorf("resolveHeadword kept %q, want the whole expression", got)
	}
}
