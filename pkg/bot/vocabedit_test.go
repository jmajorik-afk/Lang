package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"language-learning-bot/pkg/kanji"
	"language-learning-bot/pkg/storage"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func editTap(bot *tgbotapi.BotAPI, db *sql.DB, data string, msgID int) {
	HandleCallbackQuery(bot, &Clients{}, &tgbotapi.CallbackQuery{ID: "1", Data: data,
		From:    &tgbotapi.User{ID: kanaTestUser},
		Message: &tgbotapi.Message{MessageID: msgID, Chat: &tgbotapi.Chat{ID: kanaTestUser}}}, db)
}

func editType(bot *tgbotapi.BotAPI, db *sql.DB, text string) {
	HandleMessage(bot, &tgbotapi.Message{Text: text, Chat: &tgbotapi.Chat{ID: kanaTestUser},
		From: &tgbotapi.User{ID: kanaTestUser}}, &Clients{}, db)
}

// pendingOf reads the change waiting for «Да».
func pendingOf(t *testing.T, db *sql.DB) pendingEdit {
	t.Helper()
	st := storage.GetState(db, kanaTestUser)
	var p pendingEdit
	if st.Mode != modeEditConfirm || json.Unmarshal([]byte(st.TaskText), &p) != nil {
		t.Fatalf("no confirmation open: %+v", st)
	}
	return p
}

func pendingStep(db *sql.DB, word string) int {
	var step int
	db.QueryRow(`SELECT step FROM reminders WHERE user_id=? AND word=? AND sent=0`, kanaTestUser, word).Scan(&step)
	return step
}

func TestVocabEditKeyboards(t *testing.T) {
	page := vocabPageKeyboard(10, true)
	if b := page.InlineKeyboard[0]; len(b) != 2 || b[1].Text != "✏️ Изменить" || *b[1].CallbackData != "vocab_edit#10" {
		t.Errorf("page keyboard: %+v", page.InlineKeyboard[0])
	}
	entries := []storage.VocabEntry{{ID: 5}, {ID: 6}, {ID: 9}, {ID: 12}, {ID: 13}}
	kb := vocabEditKeyboard(entries, 11, "vocab_back#10")
	var got []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			got = append(got, b.Text+"="+*b.CallbackData)
		}
	}
	want := "✏️ 11=vedit#5|✏️ 12=vedit#6|✏️ 13=vedit#9|✏️ 14=vedit#12|✏️ 15=vedit#13|← Назад=vocab_back#10"
	if strings.Join(got, "|") != want || len(kb.InlineKeyboard) != 3 {
		t.Errorf("edit keyboard: %s in %d rows — the numbers must match the list", strings.Join(got, "|"), len(kb.InlineKeyboard))
	}
	if _, ok := searchEditKeyboard("собака"); !ok {
		t.Error("a short search must get «✏️ Изменить»")
	}
	if _, ok := searchEditKeyboard(strings.Repeat("я", 40)); ok {
		t.Error("button data over 64 bytes must be left out")
	}
}

func TestSameJapaneseWord(t *testing.T) {
	const pencil = "鉛筆(えんぴつ) (enpitsu)"
	for _, c := range []struct {
		in, line string
		want     bool
	}{
		{"鉛筆", pencil, true},
		{"えんぴつ", pencil, true},
		{"enpitsu", pencil, true},
		{"Empitsu", pencil, true},
		{"鉛筆 enpitsu", pencil, true},
		{"鉛筆(えんぴつ) (enpitsu)", pencil, true},
		{"食べる", "食(た)べる (taberu)", true},
		{"kohi", "コーヒー (koohii)", true},
		{"こーひー", "コーヒー (koohii)", true},
		{"sensee", "先生(せんせい) (sensei)", true},
		{"筆", pencil, false},                            // the model swapped in another word
		{"toshokan", "図書室(としょしつ) (toshoshitsu)", false}, // …and here
		{"pencil", pencil, false},                       // English, translated by the model
		{"карандаш", pencil, false},
		{"鉛筆", "Не знаю такого слова", false},
	} {
		if got := sameJapaneseWord(c.in, c.line); got != c.want {
			t.Errorf("sameJapaneseWord(%q, %q) = %v, want %v", c.in, c.line, got, c.want)
		}
	}
}

func TestWithKanaRomaji(t *testing.T) {
	for in, want := range map[string]string{
		"トイレットペーパー (toirettopeepaа)":    "トイレットペーパー (toirettopeepaa)",  // a Cyrillic «а» at the end
		"トイレットペーパー (toiretto pēpā)":     "トイレットペーパー (toiretto peepaa)", // the model's word break stays
		"鉛筆(えんぴつ) (enpitsu) — карандаш": "鉛筆(えんぴつ) (enpitsu)",
		"図書館(としょかん)":                    "図書館(としょかん) (toshokan)",
		"コーヒー (kohi)":                   "コーヒー (koohii)",
		"pencil":                        "",
	} {
		got, ok := withKanaRomaji(in)
		if got != want || ok != (want != "") {
			t.Errorf("withKanaRomaji(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// TestVocabRename — the broken «карандаш и библиотека» gets its name fixed and
// keeps its reminders, answers and step.
func TestVocabRename(t *testing.T) {
	bot, db, fake := botTestEnv(t)
	const old = "карандаш и библиотека"
	storage.SaveVocab(db, kanaTestUser, old, "鉛筆(えんぴつ) (enpitsu) — карандаш", "図書館(としょかん) (toshokan) — библиотека")
	storage.SaveVocab(db, kanaTestUser, "собака", "犬(いぬ) (inu)", "")
	storage.ScheduleReminder(db, kanaTestUser, old, 2, time.Now().Add(time.Hour))
	storage.LogOutcome(db, kanaTestUser, "reminder", old, true, true, "", "", "")
	e, _ := storage.FindVocab(db, kanaTestUser, old)

	editTap(bot, db, fmt.Sprintf("vedit#%d", e.ID), 1)
	if card := fake.last(); !strings.Contains(card, "«карандаш и библиотека» — 鉛筆(えんぴつ)") || !strings.Contains(card, "ещё: 図書館") {
		t.Errorf("card: %q", card)
	}
	editTap(bot, db, fmt.Sprintf("vedit_word#%d", e.ID), 2)
	if st := storage.GetState(db, kanaTestUser); st.Mode != modeEditWord || st.Word != old {
		t.Fatalf("after «Исправить слово»: %+v", st)
	}
	editType(bot, db, "pencil")
	if !strings.Contains(fake.last(), "по-русски") || storage.GetState(db, kanaTestUser).Mode != modeEditWord {
		t.Errorf("English accepted as a name: %q", fake.last())
	}
	editType(bot, db, "Собака")
	if !strings.Contains(fake.last(), "уже есть") || storage.GetState(db, kanaTestUser).Mode != modeEditWord {
		t.Errorf("a taken name: %q", fake.last())
	}
	editType(bot, db, "Карандаш")
	p := pendingOf(t, db)
	if p.Op != "word" || p.Value != "карандаш" || !strings.Contains(fake.last(), "Переименовать «карандаш и библиотека» в «карандаш»?") {
		t.Fatalf("confirmation %+v: %q", p, fake.last())
	}
	if _, err := storage.FindVocab(db, kanaTestUser, old); err != nil {
		t.Fatal("renamed before «Да»")
	}

	editTap(bot, db, "vedit_ok", p.Msg+50) // «Да» under some other message
	if !strings.Contains(fake.last(), "неактуально") {
		t.Errorf("a stale «Да»: %q", fake.last())
	}
	editTap(bot, db, "vedit_ok", p.Msg)
	if !strings.Contains(fake.last(), "Переименовал") {
		t.Errorf("after «Да»: %q", fake.last())
	}
	got, err := storage.FindVocab(db, kanaTestUser, "карандаш")
	if err != nil || got.ID != e.ID || got.Translation != e.Translation {
		t.Fatalf("renamed entry %+v, %v", got, err)
	}
	if pendingStep(db, "карандаш") != 2 || pendingStep(db, old) != 0 {
		t.Error("the reminder must follow the word with its step")
	}
	if storage.GetState(db, kanaTestUser).Mode != "" {
		t.Error("the edit mode must close")
	}
	editTap(bot, db, "vedit_ok", p.Msg) // a second tap does nothing
	if !strings.Contains(fake.last(), "неактуально") {
		t.Errorf("second «Да»: %q", fake.last())
	}
}

// TestVocabDelete — the word goes with its reminders, the open question about
// it closes, and an old «Не помню» can't put it back on the schedule.
func TestVocabDelete(t *testing.T) {
	bot, db, fake := botTestEnv(t)
	const word = "карандаш и библиотека"
	storage.SaveVocab(db, kanaTestUser, word, "鉛筆(えんぴつ) (enpitsu)", "")
	storage.ScheduleReminder(db, kanaTestUser, word, 1, time.Now().Add(-time.Hour))
	r, _ := storage.NextDueReminder(db, kanaTestUser, time.Now(), time.Now(), -1)
	storage.MarkReminderSent(db, r.ID)
	storage.SetState(db, kanaTestUser, "reminder", word, "", r.ID) // the question is open
	e, _ := storage.FindVocab(db, kanaTestUser, word)

	editTap(bot, db, fmt.Sprintf("vedit_del#%d", e.ID), 1)
	if !strings.Contains(fake.last(), "Удалить «карандаш и библиотека»") {
		t.Fatalf("delete question: %q", fake.last())
	}
	editTap(bot, db, "vedit_delno", 2)
	if _, err := storage.FindVocab(db, kanaTestUser, word); err != nil {
		t.Fatal("«Нет» deleted the word")
	}
	editTap(bot, db, fmt.Sprintf("vedit_delok#%d", e.ID), 3)
	if !strings.Contains(fake.last(), "Удалил") {
		t.Errorf("after «Да, удалить»: %q", fake.last())
	}
	if _, err := storage.FindVocab(db, kanaTestUser, word); err == nil {
		t.Fatal("the word is still there")
	}
	if st := storage.GetState(db, kanaTestUser); st.Mode != "" || st.Word != "" {
		t.Errorf("the open question about the word must close: %+v", st)
	}
	editTap(bot, db, "dont_remember", 4) // an old «Не помню» under that question
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM reminders WHERE word=?`, word).Scan(&n)
	if n != 0 {
		t.Errorf("%d reminders left for a deleted word", n)
	}
	editTap(bot, db, fmt.Sprintf("vedit_delok#%d", e.ID), 3)
	if !strings.Contains(fake.last(), "уже нет") {
		t.Errorf("deleting twice: %q", fake.last())
	}
}

// TestVocabNewTranslation — the typed word becomes the only translation; a
// different word starts over from the first step, a fixed reading keeps it.
func TestVocabNewTranslation(t *testing.T) {
	bot, db, fake := botTestEnv(t)
	kanji.BaseURL = "http://127.0.0.1:1/" // never reach the real dictionary from a test
	for k, m := range map[string]string{"図": "схема", "書": "писать", "館": "здание"} {
		storage.SetKanjiMeaning(db, k, m)
	}
	lines := map[string]string{
		"toshokan": "図書館(としょかん) (toshokan)",
		"とし":       "図書館(としよかん) (toshiyokan)", // the same word with another reading
	}
	saved := editTranslationLine
	t.Cleanup(func() { editTranslationLine = saved })
	editTranslationLine = func(_ *Clients, _, input string) (string, bool, bool) {
		line, ok := lines[input]
		return line, ok, ok
	}

	const word = "библиотека"
	storage.SaveVocab(db, kanaTestUser, word, "鉛筆(えんぴつ) (enpitsu) — карандаш", "図書館(としょかん) (toshokan) — библиотека")
	storage.ScheduleReminder(db, kanaTestUser, word, 3, time.Now().Add(time.Hour))
	e, _ := storage.FindVocab(db, kanaTestUser, word)

	editTap(bot, db, fmt.Sprintf("vedit_tr#%d", e.ID), 1)
	editType(bot, db, "абв")
	if !strings.Contains(fake.last(), "Не разобрал") || storage.GetState(db, kanaTestUser).Mode != modeEditTr {
		t.Errorf("not Japanese: %q", fake.last())
	}
	editType(bot, db, "toshokan")
	p := pendingOf(t, db)
	for _, must := range []string{"Новый перевод «библиотека»: 図書館(としょかん) (toshokan)", "сверено с Jisho",
		"図 — схема", "начнутся заново"} {
		if !strings.Contains(fake.last(), must) {
			t.Errorf("confirmation lacks %q:\n%s", must, fake.last())
		}
	}
	editTap(bot, db, "vedit_no", p.Msg)
	if got, _ := storage.FindVocab(db, kanaTestUser, word); got.Translation != e.Translation {
		t.Fatal("«Нет» changed the translation")
	}

	editTap(bot, db, fmt.Sprintf("vedit_tr#%d", e.ID), 1)
	editType(bot, db, "toshokan")
	editTap(bot, db, "vedit_ok", pendingOf(t, db).Msg)
	got, _ := storage.FindVocab(db, kanaTestUser, word)
	if got.Translation != "図書館(としょかん) (toshokan)" || got.Alternatives != "" {
		t.Fatalf("saved %+v — want the typed word as the only translation", got)
	}
	if step := pendingStep(db, word); step != 1 {
		t.Errorf("a different word must start over, pending step %d", step)
	}

	editTap(bot, db, fmt.Sprintf("vedit_tr#%d", e.ID), 1)
	editType(bot, db, "toshokan")
	if !strings.Contains(fake.last(), "Так и записано") || storage.GetState(db, kanaTestUser).Mode != "" {
		t.Errorf("the same translation again: %q", fake.last())
	}

	storage.ScheduleReminder(db, kanaTestUser, word, 3, time.Now().Add(time.Hour))
	editTap(bot, db, fmt.Sprintf("vedit_tr#%d", e.ID), 1)
	editType(bot, db, "とし")
	if !strings.Contains(fake.last(), "Прогресс повторений сохранится") {
		t.Errorf("a fixed reading of the same word: %q", fake.last())
	}
	editTap(bot, db, "vedit_ok", pendingOf(t, db).Msg)
	if step := pendingStep(db, word); step != 3 {
		t.Errorf("a fixed reading must keep the progress, pending step %d", step)
	}
}
