package bot

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	storage "language-learning-bot/pkg/storage"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ---------- editing the vocabulary ----------
//
// /vocab and /vocab <поиск> carry «✏️ Изменить». It turns into one numbered
// button per word shown, and a number opens the word's card: «Исправить
// слово», «Исправить перевод», «Удалить». Every change waits for «Да» under its
// own confirmation, so a word typed an hour later as a lookup can never rename
// an entry by surprise.

const (
	modeEditWord    = "edit_word"    // waiting for the new Russian spelling of a saved word
	modeEditTr      = "edit_tr"      // waiting for the Japanese that replaces a word's translation
	modeEditConfirm = "edit_confirm" // the change is shown, waiting for «Да» / «Нет»
)

const vocabGone = "Этого слова уже нет в словаре."

// pendingEdit is the change waiting for «Да», kept in the state's task_text.
type pendingEdit struct {
	Op    string `json:"op"` // "word" renames the entry, "tr" replaces its translation
	Value string `json:"v"`  // the new Russian key, or the new translation line
	Msg   int    `json:"m"`  // the confirmation its buttons sit under: an older one is stale
}

// vocabEditKeyboard — «✏️ N» per word shown, numbered as in the list above it
// and keyed by vocab id like the 🔊 buttons, then «← Назад».
func vocabEditKeyboard(entries []storage.VocabEntry, first int, backData string) tgbotapi.InlineKeyboardMarkup {
	kb := tgbotapi.NewInlineKeyboardMarkup()
	var row []tgbotapi.InlineKeyboardButton
	for i, e := range entries {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("✏️ %d", first+i), fmt.Sprintf("vedit#%d", e.ID)))
		if len(row) == 4 || i == len(entries)-1 {
			kb.InlineKeyboard = append(kb.InlineKeyboard, row)
			row = nil
		}
	}
	kb.InlineKeyboard = append(kb.InlineKeyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("← Назад", backData)))
	return kb
}

// searchEditKeyboard — «✏️ Изменить» under /vocab <поиск>. The query rides in
// the button and Telegram allows 64 bytes of button data, so a very long query
// gets no button.
func searchEditKeyboard(q string) (tgbotapi.InlineKeyboardMarkup, bool) {
	if len("vfback#"+q) > 64 {
		return tgbotapi.InlineKeyboardMarkup{}, false
	}
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("✏️ Изменить", "vfind#"+q))), true
}

func vocabCard(e storage.VocabEntry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "«%s»", e.Word)
	if e.Translation != "" {
		sb.WriteString(" — " + e.Translation)
	}
	for _, alt := range strings.Split(e.Alternatives, "\n") {
		if alt = strings.TrimSpace(alt); alt != "" {
			sb.WriteString("\nещё: " + alt)
		}
	}
	sb.WriteString("\n\nЧто сделать?")
	return sb.String()
}

func vocabCardKeyboard(id int) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Исправить слово", fmt.Sprintf("vedit_word#%d", id))),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Исправить перевод", fmt.Sprintf("vedit_tr#%d", id))),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Удалить", fmt.Sprintf("vedit_del#%d", id))),
	)
}

func editConfirmKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("Да", "vedit_ok"),
		tgbotapi.NewInlineKeyboardButtonData("Нет", "vedit_no"),
	))
}

// handleVocabEdit answers every editing button: vocab_edit#<offset>,
// vfind#<query>, vfback#<query> and the vedit… family.
func handleVocabEdit(bot *tgbotapi.BotAPI, db *sql.DB, cq *tgbotapi.CallbackQuery, data string) {
	chatID, userID, msgID := cq.Message.Chat.ID, int(cq.From.ID), cq.Message.MessageID
	op, arg, _ := strings.Cut(data, "#")
	id, _ := strconv.Atoi(arg)

	switch op {
	case "vocab_edit": // the page's words as numbered ✏️ buttons
		if _, entries, err := storage.GetVocabPage(db, userID, id, vocabPageSize); err == nil && len(entries) > 0 {
			bot.Send(tgbotapi.NewEditMessageReplyMarkup(chatID, msgID,
				vocabEditKeyboard(entries, id+1, fmt.Sprintf("vocab_back#%d", id))))
		}

	case "vfind": // the same for the words a search found
		if entries, err := storage.SearchVocab(db, userID, arg, vocabSearchLimit); err == nil && len(entries) > 0 {
			bot.Send(tgbotapi.NewEditMessageReplyMarkup(chatID, msgID, vocabEditKeyboard(entries, 1, "vfback#"+arg)))
		}

	case "vfback":
		if kb, ok := searchEditKeyboard(arg); ok {
			bot.Send(tgbotapi.NewEditMessageReplyMarkup(chatID, msgID, kb))
		}

	case "vedit": // the word's card
		e, err := storage.GetVocabEntry(db, userID, id)
		if err != nil {
			send(bot, chatID, vocabGone)
			return
		}
		sendKb(bot, chatID, vocabCard(e), vocabCardKeyboard(e.ID))

	case "vedit_word", "vedit_tr":
		e, err := storage.GetVocabEntry(db, userID, id)
		if err != nil {
			send(bot, chatID, vocabGone)
			return
		}
		if op == "vedit_word" {
			storage.SetState(db, userID, modeEditWord, e.Word, "", 0)
			send(bot, chatID, fmt.Sprintf("Как записать «%s»? Напиши по-русски: слово или выражение до трёх слов.", e.Word))
		} else {
			storage.SetState(db, userID, modeEditTr, e.Word, "", 0)
			send(bot, chatID, fmt.Sprintf("Как будет «%s» по-японски? Напиши кандзи, каной или ромадзи.", e.Word))
		}

	case "vedit_del":
		e, err := storage.GetVocabEntry(db, userID, id)
		if err != nil {
			send(bot, chatID, vocabGone)
			return
		}
		sendKb(bot, chatID, fmt.Sprintf("Удалить «%s» из словаря? Повторять его больше не буду.", e.Word),
			tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Да, удалить", fmt.Sprintf("vedit_delok#%d", e.ID)),
				tgbotapi.NewInlineKeyboardButtonData("Нет", "vedit_delno"),
			)))

	case "vedit_delok":
		removeKeyboard(bot, chatID, msgID)
		e, err := storage.GetVocabEntry(db, userID, id)
		if err != nil {
			send(bot, chatID, vocabGone)
			return
		}
		if err := storage.DeleteVocab(db, userID, e.Word); err != nil {
			log.Printf("delete %q: %v", e.Word, err)
			send(bot, chatID, "Не получилось удалить, попробуй ещё раз.")
			return
		}
		// a question or a confirmation about the word may still be open, and an
		// old «Не помню» would put the word back on the schedule from the state
		if st := storage.GetState(db, userID); st.Word == e.Word {
			storage.SetCurrentWord(db, userID, "")
		}
		send(bot, chatID, fmt.Sprintf("Удалил «%s».", e.Word))

	case "vedit_delno":
		removeKeyboard(bot, chatID, msgID)
		send(bot, chatID, "Ок, не удаляю.")

	case "vedit_ok", "vedit_no":
		st := storage.GetState(db, userID)
		var p pendingEdit
		if st.Mode != modeEditConfirm || json.Unmarshal([]byte(st.TaskText), &p) != nil || p.Msg != msgID {
			send(bot, chatID, "Это подтверждение уже неактуально.")
			return
		}
		removeKeyboard(bot, chatID, msgID)
		storage.ClearMode(db, userID)
		if op == "vedit_no" {
			send(bot, chatID, "Ок, оставил как было.")
			return
		}
		applyEdit(bot, db, chatID, userID, st.Word, p)
	}
}

// applyEdit carries out a confirmed change. A new name keeps the word's
// progress; a new translation keeps it too when it is the same Japanese word
// (a fixed reading), and otherwise the word is learnt from the first step.
func applyEdit(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int, word string, p pendingEdit) {
	switch p.Op {
	case "word":
		switch err := storage.RenameVocab(db, userID, word, p.Value); {
		case err == nil:
			send(bot, chatID, fmt.Sprintf("Переименовал: «%s» → «%s».", word, p.Value))
		case errors.Is(err, storage.ErrVocabExists):
			send(bot, chatID, fmt.Sprintf("«%s» уже есть в словаре — ничего не менял.", p.Value))
		case errors.Is(err, sql.ErrNoRows):
			send(bot, chatID, vocabGone)
		default:
			log.Printf("rename %q → %q: %v", word, p.Value, err)
			send(bot, chatID, "Не получилось, попробуй ещё раз.")
		}

	case "tr":
		e, err := storage.FindVocab(db, userID, word)
		if err != nil {
			send(bot, chatID, vocabGone)
			return
		}
		if err := storage.SetVocabSenses(db, userID, word, p.Value, ""); err != nil {
			log.Printf("new translation for %q: %v", word, err)
			send(bot, chatID, "Не получилось, попробуй ещё раз.")
			return
		}
		msg := fmt.Sprintf("Сохранил: «%s» — %s.", word, p.Value)
		if japaneseHeadword(p.Value) != japaneseHeadword(e.Translation) {
			storage.ScheduleReminder(db, userID, word, 1, time.Now().Add(intervalFor(1)))
			msg += " Повторения начнутся заново."
		}
		sendKb(bot, chatID, msg, playKeyboard(e.ID))
	}
}

// askEditConfirm shows the change with «Да» / «Нет» and remembers it, together
// with the message, so only these buttons can confirm it.
func askEditConfirm(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int, word, text string, p pendingEdit) {
	p.Msg = sendKbID(bot, chatID, text, editConfirmKeyboard())
	data, _ := json.Marshal(p)
	storage.SetState(db, userID, modeEditConfirm, word, string(data), 0)
}

// handleEditWordText takes the new Russian spelling typed after «Исправить слово».
func handleEditWordText(bot *tgbotapi.BotAPI, db *sql.DB, message *tgbotapi.Message, st storage.State) {
	chatID, userID := message.Chat.ID, int(message.From.ID)
	e, err := storage.FindVocab(db, userID, st.Word)
	if err != nil {
		storage.ClearMode(db, userID)
		send(bot, chatID, vocabGone)
		return
	}
	key := vocabKey(message.Text)
	if !russianKey(key) {
		send(bot, chatID, "Нужно по-русски: слово или выражение до трёх слов. Напиши ещё раз.")
		return
	}
	if key == e.Word {
		storage.ClearMode(db, userID)
		send(bot, chatID, "Так и записано — ничего не меняю.")
		return
	}
	if _, err := storage.FindVocab(db, userID, key); err == nil {
		send(bot, chatID, fmt.Sprintf("«%s» уже есть в словаре — напиши другое.", key))
		return
	}
	askEditConfirm(bot, db, chatID, userID, e.Word,
		fmt.Sprintf("Переименовать «%s» в «%s»? Прогресс повторений сохранится.", e.Word, key),
		pendingEdit{Op: "word", Value: key})
}

// handleEditTrText takes the Japanese typed after «Исправить перевод».
func handleEditTrText(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message, st storage.State) {
	chatID, userID := message.Chat.ID, int(message.From.ID)
	e, err := storage.FindVocab(db, userID, st.Word)
	if err != nil {
		storage.ClearMode(db, userID)
		send(bot, chatID, vocabGone)
		return
	}
	think := sendThinking(bot, chatID)
	line, verified, ok := editTranslationLine(clients, e.Word, message.Text)
	if ok {
		learnKanji(clients, db, []string{line}, saveKanjiWait)
	}
	deleteMsg(bot, chatID, think)
	if !ok {
		send(bot, chatID, "Не разобрал японское слово. Напиши его кандзи, каной или ромадзи — например 鉛筆, えんぴつ или enpitsu.")
		return
	}
	if line == e.Translation && e.Alternatives == "" {
		storage.ClearMode(db, userID)
		send(bot, chatID, "Так и записано — ничего не меняю.")
		return
	}
	msg := fmt.Sprintf("Новый перевод «%s»: %s", e.Word, line)
	if verified {
		msg += "\n(чтение сверено с Jisho)"
	}
	msg += kanjiBlock(kanjiNotes(db, []string{line}))
	if japaneseHeadword(line) == japaneseHeadword(e.Translation) {
		msg += "\n\nСохранить? Прогресс повторений сохранится."
	} else {
		msg += "\n\nСохранить? Повторения этого слова начнутся заново."
	}
	askEditConfirm(bot, db, chatID, userID, e.Word, msg, pendingEdit{Op: "tr", Value: line})
}

// editTranslationLine turns the Japanese the user typed — kanji, kana or romaji
// — into a dictionary line, 鉛筆(えんぴつ) (enpitsu), with the reading checked
// against Jisho. It is the word the user typed, never one the model likes
// better: ok is false when the line names another word or the text isn't
// Japanese. A variable so tests can stand in for the model.
var editTranslationLine = func(clients *Clients, word, input string) (line string, verified, ok bool) {
	input = strings.TrimSpace(input)
	if input == "" || containsCyrillic(input) {
		return "", false, false
	}
	sys := "The learner is fixing the Japanese translation of the Russian «" + word + "» in their vocabulary and " +
		"typed the Japanese word to store, in kanji, kana or romaji. Write exactly that word — never a different " +
		"one — the way it is usually written: in kanji when it is usually written in kanji, even if typed in kana " +
		"or romaji. One line: the word with each run of kanji immediately followed by the hiragana reading of that " +
		"whole run in round brackets — 鉛筆(えんぴつ), never 鉛(えん)筆(ぴつ) — then one space, then the romaji in " +
		"round brackets. Kana gets no reading of its own. For romaji input pick the kanji that fit «" + word + "». " +
		"Examples: 鉛筆(えんぴつ) (enpitsu), 図書室(としょしつ) (toshoshitsu), 食(た)べる (taberu), コーヒー (koohii). " +
		"If the text is not a Japanese word, reply with exactly NONE. No other text."
	resp, err := claudeOne(clients, sys, input)
	if err != nil {
		log.Printf("edit translation: %v", err)
		return "", false, false
	}
	line = firstLine(tidy(resp))
	if strings.HasPrefix(strings.ToUpper(line), "NONE") || !sameJapaneseWord(input, line) {
		log.Printf("edit translation: %q came back as %q", input, line)
		return "", false, false
	}
	line, verified = verifyWithJisho(clients, line)
	if line, ok = withKanaRomaji(tidy(line)); !ok {
		log.Printf("edit translation: no romaji for %q", line)
		return "", false, false
	}
	return line, verified, true
}

// withKanaRomaji rewrites the romaji of a line from its own kana reading, keeping
// the model's word breaks: the model once ended トイレットペーパー's romaji with
// a Cyrillic «а», which the romaji check can't even see. A note after « — » is
// dropped: the line is the whole translation.
func withKanaRomaji(line string) (string, bool) {
	if i := strings.Index(line, " — "); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	jp, model := line, ""
	if i := strings.LastIndex(line, " ("); i >= 0 && strings.HasSuffix(line, ")") {
		jp, model = strings.TrimSpace(line[:i]), strings.TrimSuffix(line[i+len(" ("):], ")")
	}
	_, reading := splitHeadword(line)
	want, ok := kanaToRomaji(reading)
	if !ok || !containsJapanese(jp) {
		return "", false
	}
	if isRomaji(model) && normalizeRomaji(model) == normalizeRomaji(want) {
		want = keepSpacing(model, want) // «toiretto peepaa» reads better than «toirettopeepaa»
	}
	return jp + " (" + want + ")", true
}

// sameJapaneseWord: the line spells the word the user typed — the same written
// form or the same reading for Japanese, the same sound for romaji (long vowels
// and the usual spelling variants aside).
func sameJapaneseWord(input, line string) bool {
	written, reading := splitHeadword(line)
	if written == "" {
		return false
	}
	in := parenGroupRe.ReplaceAllString(input, "")
	if containsJapanese(in) {
		in = strings.Map(func(r rune) rune {
			if containsJapanese(string(r)) || r == '々' {
				return r
			}
			return -1 // romaji or spaces typed along with the word
		}, in)
		return in == written || toHiragana(in) == toHiragana(reading)
	}
	if !isRomaji(in) {
		return false
	}
	rom, ok := kanaToRomaji(reading)
	return ok && looseRomaji(rom) == looseRomaji(in)
}

// looseRomaji compares romaji by sound, the way a learner types it: a long
// vowel may be written once (kohi for koohii) and ei as ee.
func looseRomaji(s string) string {
	s = strings.ReplaceAll(normalizeRomaji(s), "ei", "ee")
	var sb strings.Builder
	var prev rune
	for _, r := range s {
		if r == prev && strings.ContainsRune("aiueo", r) {
			continue
		}
		sb.WriteRune(r)
		prev = r
	}
	return sb.String()
}
