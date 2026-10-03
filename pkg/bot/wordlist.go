package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	storage "language-learning-bot/pkg/storage"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ---------- several words at once ----------
//
// «собака, кошка, дом, окно» — a list separated by commas, semicolons or line
// breaks — gets one compact answer, a dictionary line per word, with «Запомнить
// все» and a button per word. What is saved is exactly what was shown: the same
// translateWord lines the single-word confirmation uses, kept in the state.

const (
	modeWordList  = "word_list" // a list of words was shown, waiting for the save buttons
	wordListLimit = 10
)

type listWord struct {
	Word         string `json:"w"`
	Translation  string `json:"t,omitempty"`
	Alternatives string `json:"a,omitempty"`
	Verified     bool   `json:"v,omitempty"`
	Saved        bool   `json:"s,omitempty"` // in the dictionary, from before or just now
}

// splitWordList recognises a message that is a list of words and returns them,
// or nil for anything else, which stays a normal lookup: every item must reduce
// to one Russian word, so «красная машина» (a phrase) and «я дома, но устал» (a
// sentence) are not lists.
func splitWordList(text string) []string {
	if !strings.ContainsAny(text, ",;\n") {
		return nil
	}
	var words []string
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		w := sanitizeWord(part)
		if w == "" || !containsCyrillic(w) {
			return nil
		}
		if !seen[w] {
			seen[w] = true
			words = append(words, w)
		}
	}
	if len(words) < 2 {
		return nil
	}
	return words
}

// lookupWordList translates the words — a few at a time, cached ones for free —
// and shows them with the save buttons.
func lookupWordList(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID int, words []string) {
	rest := 0
	if len(words) > wordListLimit {
		rest = len(words) - wordListLimit
		words = words[:wordListLimit]
	}
	think := sendThinking(bot, chatID)
	list := make([]listWord, len(words))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, w := range words {
		list[i].Word = w
		if e, err := storage.FindVocab(db, userID, w); err == nil {
			list[i].Translation, list[i].Alternatives, list[i].Saved = e.Translation, e.Alternatives, true
			continue
		}
		wg.Add(1)
		go func(i int, w string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			list[i].Translation, list[i].Alternatives, list[i].Verified = translateWord(clients, db, w)
		}(i, w)
	}
	wg.Wait()
	var senses []string
	for _, lw := range list {
		senses = append(senses, sensesOf(lw.Translation, lw.Alternatives)...)
	}
	learnKanji(clients, db, senses, saveKanjiWait)
	deleteMsg(bot, chatID, think)

	text := renderWordList(db, list, rest)
	kb, open := wordListKeyboard(list)
	if !open {
		send(bot, chatID, text)
		return
	}
	data, _ := json.Marshal(list)
	storage.SetState(db, userID, modeWordList, "", string(data), 0)
	sendKb(bot, chatID, text, kb)
}

func renderWordList(db *sql.DB, list []listWord, rest int) string {
	var sb strings.Builder
	var unverified []string
	fresh := 0
	for i, lw := range list {
		switch {
		case lw.Saved:
			fmt.Fprintf(&sb, "%d. %s — %s (уже в словаре)\n", i+1, lw.Word, lw.Translation)
		case lw.Translation == "":
			fmt.Fprintf(&sb, "%d. %s — не нашёл перевод\n", i+1, lw.Word)
		default:
			fresh++
			vocabLine(&sb, db, i+1, storage.VocabEntry{Word: lw.Word, Translation: lw.Translation, Alternatives: lw.Alternatives})
			if !lw.Verified {
				unverified = append(unverified, lw.Word)
			}
		}
	}
	switch {
	case fresh > 0 && len(unverified) == 0:
		sb.WriteString("\nЧтение сверено с Jisho.")
	case fresh > 0 && len(unverified) < fresh:
		sb.WriteString("\nЧтение сверено с Jisho, кроме: " + strings.Join(unverified, ", ") + ".")
	}
	if rest > 0 {
		fmt.Fprintf(&sb, "\nЕщё %d — отправь следующим сообщением: за раз до %d слов.", rest, wordListLimit)
	}
	return strings.TrimSpace(sb.String())
}

// wordListKeyboard — «Запомнить все» when more than one word can be saved, then
// one button per word. Reports false when nothing is left to save.
func wordListKeyboard(list []listWord) (tgbotapi.InlineKeyboardMarkup, bool) {
	var rows [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	open := 0
	for i, lw := range list {
		if lw.Saved || lw.Translation == "" {
			continue
		}
		open++
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("+ "+lw.Word, fmt.Sprintf("list_one#%d", i)))
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	if open == 0 {
		return tgbotapi.InlineKeyboardMarkup{}, false
	}
	if open > 1 {
		all := tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("Запомнить все (%d)", open), "list_all"))
		rows = append([][]tgbotapi.InlineKeyboardButton{all}, rows...)
	}
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}, true
}

// saveFromWordList handles «list_all» and «list_one#<i>»: saves the shown lines
// exactly, schedules each new word's first review, and leaves buttons only for
// what is still unsaved.
func saveFromWordList(bot *tgbotapi.BotAPI, db *sql.DB, cq *tgbotapi.CallbackQuery, data string) {
	chatID, userID := cq.Message.Chat.ID, int(cq.From.ID)
	st := storage.GetState(db, userID)
	var list []listWord
	if st.Mode != modeWordList || json.Unmarshal([]byte(st.TaskText), &list) != nil {
		send(bot, chatID, "Этот список уже неактуален — отправь слова ещё раз.")
		return
	}
	var picked []int
	if data == "list_all" {
		for i, lw := range list {
			if !lw.Saved && lw.Translation != "" {
				picked = append(picked, i)
			}
		}
	} else if i, err := strconv.Atoi(strings.TrimPrefix(data, "list_one#")); err == nil &&
		i >= 0 && i < len(list) && !list[i].Saved && list[i].Translation != "" {
		picked = []int{i}
	}
	if len(picked) == 0 {
		return
	}
	var added []string
	for _, i := range picked {
		lw := list[i]
		if err := storage.SaveVocab(db, userID, lw.Word, lw.Translation, lw.Alternatives); err != nil {
			log.Printf("save %q: %v", lw.Word, err)
			continue
		}
		if !storage.HasPendingReminder(db, userID, lw.Word) {
			storage.ScheduleReminder(db, userID, lw.Word, 1, time.Now().Add(intervalFor(1)))
		}
		list[i].Saved = true
		added = append(added, lw.Word)
	}
	if kb, open := wordListKeyboard(list); open {
		bot.Send(tgbotapi.NewEditMessageReplyMarkup(chatID, cq.Message.MessageID, kb))
		state, _ := json.Marshal(list)
		storage.SetState(db, userID, modeWordList, "", string(state), 0)
	} else {
		removeKeyboard(bot, chatID, cq.Message.MessageID)
		storage.ClearMode(db, userID)
	}
	if len(added) > 0 {
		send(bot, chatID, "Добавил в словарь: "+strings.Join(added, ", ")+". Напомню по расписанию.")
	}
}
