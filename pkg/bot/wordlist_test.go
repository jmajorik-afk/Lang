package bot

import (
	"encoding/json"
	"strings"
	"testing"

	"language-learning-bot/pkg/kanji"
	"language-learning-bot/pkg/storage"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestSplitWordList(t *testing.T) {
	for text, want := range map[string]string{
		"собака, кошка, дом, окно": "собака кошка дом окно",
		"собака\nкошка":            "собака кошка",
		"Собака; КОШКА.":           "собака кошка",
		"собака, собака, кошка":    "собака кошка", // repeats dropped
		"как будет собака, кошка?": "собака кошка", // lookup phrasing peeled off
		"собака":           "", // one word: the usual lookup
		"я дома, но устал": "", // a sentence with a comma
		"dog, cat":         "", // not Russian
		"собака, собака":   "", // one word after all
		// the real message that came through as a single lookup
		"человек, студент, друг, в порядке": "человек студент друг в порядке",
		"красная машина, доброе утро":       "красная машина доброе утро",
		"я, ты, он":        "я ты он", // lone pronouns are words to learn
		"привет, как дела": "привет как дела",
		"если пойдёт дождь, я останусь дома": "", // a clause
		"собака, это очень большая собака":   "", // four words: a sentence
	} {
		if got := strings.Join(splitWordList(text), " "); got != want {
			t.Errorf("splitWordList(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestWordListKeyboard(t *testing.T) {
	list := []listWord{
		{Word: "собака", Translation: "犬(いぬ) (inu)"},
		{Word: "окно", Translation: "窓(まど) (mado)", Saved: true},
		{Word: "кошка", Translation: "猫(ねこ) (neko)"},
		{Word: "абырвалг"}, // no translation: nothing to save
	}
	kb, open := wordListKeyboard(list)
	if !open {
		t.Fatal("two words can be saved")
	}
	var labels []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			labels = append(labels, b.Text)
		}
	}
	if got := strings.Join(labels, "|"); got != "Запомнить все (2)|+ собака|+ кошка" {
		t.Errorf("buttons = %s", got)
	}
	list[0].Saved = true
	kb, _ = wordListKeyboard(list)
	if len(kb.InlineKeyboard) != 1 || kb.InlineKeyboard[0][0].Text != "+ кошка" {
		t.Errorf("one word left: no «Запомнить все», got %+v", kb.InlineKeyboard)
	}
	list[2].Saved = true
	if _, open := wordListKeyboard(list); open {
		t.Error("nothing left to save, the keyboard must go")
	}
}

// TestWordListEndToEnd — the list is shown with its lines, one word is saved by
// its own button, the rest by «Запомнить все», each with its first review.
func TestWordListEndToEnd(t *testing.T) {
	bot, db, fake := botTestEnv(t)
	kanji.BaseURL = "http://127.0.0.1:1/" // never reach the real dictionary from a test
	for word, tr := range map[string]string{"собака": "犬(いぬ) (inu)", "кошка": "猫(ねこ) (neko)", "дом": "家(いえ) (ie)"} {
		storage.SetCachedTranslation(db, word, tr, "", true)
	}
	for k, m := range map[string]string{"犬": "собака", "猫": "кошка", "家": "дом", "窓": "окно"} {
		storage.SetKanjiMeaning(db, k, m)
	}
	storage.SaveVocab(db, kanaTestUser, "окно", "窓(まど) (mado)", "")

	lookupWordList(bot, &Clients{}, db, kanaTestUser, kanaTestUser, splitWordList("собака, кошка, окно, дом"))
	shown := fake.last()
	for _, must := range []string{"1. собака — 犬(いぬ) (inu)", "犬 — собака", "окно — 窓(まど) (mado) (уже в словаре)",
		"4. дом — 家(いえ) (ie)", "Чтение сверено с Jisho."} {
		if !strings.Contains(shown, must) {
			t.Errorf("list message lacks %q:\n%s", must, shown)
		}
	}
	st := storage.GetState(db, kanaTestUser)
	var list []listWord
	if st.Mode != modeWordList || json.Unmarshal([]byte(st.TaskText), &list) != nil || len(list) != 4 {
		t.Fatalf("state after the list: %+v", st)
	}

	cq := &tgbotapi.CallbackQuery{From: &tgbotapi.User{ID: kanaTestUser},
		Message: &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: kanaTestUser}}}
	saveFromWordList(bot, db, cq, "list_one#1") // кошка
	if e, err := storage.FindVocab(db, kanaTestUser, "кошка"); err != nil || e.Translation != "猫(ねこ) (neko)" {
		t.Fatalf("кошка saved as %+v, %v — must be exactly the shown line", e, err)
	}
	if _, err := storage.FindVocab(db, kanaTestUser, "собака"); err == nil {
		t.Fatal("one button saved more than its word")
	}
	saveFromWordList(bot, db, cq, "list_all")
	for _, w := range []string{"собака", "кошка", "дом"} {
		if !storage.HasPendingReminder(db, kanaTestUser, w) {
			t.Errorf("%s has no first review scheduled", w)
		}
	}
	if !strings.Contains(fake.last(), "собака, дом") {
		t.Errorf("«Запомнить все» reported %q", fake.last())
	}
	if storage.GetState(db, kanaTestUser).Mode != "" {
		t.Error("everything saved, the list mode must close")
	}
	saveFromWordList(bot, db, cq, "list_all")
	if !strings.Contains(fake.last(), "неактуален") {
		t.Errorf("a button of a finished list said %q", fake.last())
	}
}

func TestWordListLimit(t *testing.T) {
	bot, db, fake := botTestEnv(t)
	kanji.BaseURL = "http://127.0.0.1:1/"
	var words []string
	for i := 0; i < 12; i++ {
		w := "слово" + string(rune('а'+i))
		storage.SetCachedTranslation(db, w, "言葉(ことば) (kotoba)", "", true)
		words = append(words, w)
	}
	storage.SetKanjiMeaning(db, "言", "слово")
	storage.SetKanjiMeaning(db, "葉", "лист")
	lookupWordList(bot, &Clients{}, db, kanaTestUser, kanaTestUser, words)
	if !strings.Contains(fake.last(), "Ещё 2") || strings.Contains(fake.last(), "11.") {
		t.Errorf("over the limit: %s", fake.last())
	}
}
