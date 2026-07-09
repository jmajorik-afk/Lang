package bot

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	claude_api "language-learning-bot/pkg/claude"
	"language-learning-bot/pkg/config"
	openai_api "language-learning-bot/pkg/openai"
	storage "language-learning-bot/pkg/storage"

	"github.com/anthropics/anthropic-sdk-go"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	openai "github.com/sashabaranov/go-openai"
)

const historyTurns = 6

// Clients bundles both API clients: Claude for text, OpenAI for TTS.
type Clients struct {
	Claude anthropic.Client
	OpenAI *openai.Client
}

// styleRules is prepended to every model prompt so all answers share one style.
const styleRules = "Plain text only — no Markdown, no #, no *, no bold, no headers, no --- dividers. " +
	"Be short and focused: a few lines, answer only what was asked, no sections, no lecture. " +
	"Japanese style: every kanji is immediately followed by its reading in round brackets, like 育(そだ). " +
	"Give romaji for a whole word or sentence once, in pure Latin letters only — never use square brackets, " +
	"never mix Cyrillic into romaji, never leave part of a word without romaji. Example: 育(そだ)っています (sodatte imasu). " +
	"No emojis."

const grammarSystem = styleRules + " You are a friendly Japanese tutor. " +
	"The user asks a grammar question in Russian. Answer in Russian, casually. Do not ask follow-up questions."

// ---------- small send helpers ----------

func send(bot *tgbotapi.BotAPI, chatID int64, text string) {
	if _, err := bot.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		log.Printf("send error: %v", err)
	}
}

func sendKb(bot *tgbotapi.BotAPI, chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	m := tgbotapi.NewMessage(chatID, text)
	m.ReplyMarkup = kb
	if _, err := bot.Send(m); err != nil {
		log.Printf("send error: %v", err)
	}
}

func answerCallback(bot *tgbotapi.BotAPI, cq *tgbotapi.CallbackQuery) {
	bot.Request(tgbotapi.NewCallback(cq.ID, ""))
}

func sendThinking(bot *tgbotapi.BotAPI, chatID int64) int {
	m, err := bot.Send(tgbotapi.NewMessage(chatID, "Думаю..."))
	if err != nil {
		return 0
	}
	return m.MessageID
}

func deleteMsg(bot *tgbotapi.BotAPI, chatID int64, msgID int) {
	if msgID != 0 {
		bot.Request(tgbotapi.NewDeleteMessage(chatID, msgID))
	}
}

func claudeOne(clients *Clients, system, user string) (string, error) {
	return claude_api.GetClaudeResponse(context.Background(), &clients.Claude, claude_api.ClaudeRequest{
		SystemPrompt: system,
		UserMessage:  user,
	})
}

// ---------- keyboards ----------

func wordKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Запомнить", "memorize"),
			tgbotapi.NewInlineKeyboardButtonData("Знаю", "know"),
			tgbotapi.NewInlineKeyboardButtonData("🔊", "audio"),
		),
	)
}

func practiceOfferKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Да", "practice_yes"),
			tgbotapi.NewInlineKeyboardButtonData("Нет", "practice_no"),
		),
	)
}

// practiceKeyboard — "Закончить" + "Уточнить" (used during practice stages).
func practiceKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Закончить", "exit"),
			tgbotapi.NewInlineKeyboardButtonData("Уточнить", "clarify"),
		),
	)
}

func sendComposeTask(bot *tgbotapi.BotAPI, chatID int64, task string) {
	sendKb(bot, chatID, "Составь это предложение по-японски:\n\n"+task, practiceKeyboard())
}

func sendTranslateTask(bot *tgbotapi.BotAPI, chatID int64, task string) {
	sendKb(bot, chatID, "Переведи на русский:\n\n"+task, practiceKeyboard())
}

func roundKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Ещё раунд", "round_yes"),
			tgbotapi.NewInlineKeyboardButtonData("Хватит", "round_no"),
		),
	)
}

// askKeyboard — shown under a /ask answer: clarify further or practice the topic.
func askKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Уточнить", "ask_clarify"),
			tgbotapi.NewInlineKeyboardButtonData("Потренироваться", "ask_practice"),
		),
	)
}

// reminderKeyboard — shown under an SRS reminder.
func reminderKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Закончить", "exit"),
			tgbotapi.NewInlineKeyboardButtonData("Не помню", "dont_remember"),
		),
	)
}

// ---------- command entry ----------

func HandleCommand(ctx context.Context, bot *tgbotapi.BotAPI, message *tgbotapi.Message, db *sql.DB, clients *Clients) error {
	log.Printf("%d [%s] %s", message.From.ID, message.From.UserName, message.Text)
	userID := int(message.From.ID)
	chatID := message.Chat.ID
	storage.EnsureUser(db, userID)

	switch message.Command() {
	case "start":
		send(bot, chatID, "Привет! Напиши любое слово — переведу, дам пример и предложу запомнить.\n\n"+
			"/practice — тренировка по выученным словам\n"+
			"/ask — вопрос по грамматике\n"+
			"/vocab — твой словарь\n"+
			"/speech_speed — скорость озвучки")
	case "practice":
		startPracticeRandom(bot, clients, db, chatID, userID, "")
	case "ask":
		handleAsk(bot, clients, db, message)
	case "vocab":
		sendVocab(bot, db, chatID, userID)
	case "speech_speed":
		sendKb(bot, chatID, "Выбери скорость озвучки:", speechSpeedInlineKeyboard())
	case "healthz":
		send(bot, chatID, "OK")
	}
	return nil
}

func sendVocab(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int) {
	words, _ := storage.GetVocabList(db, userID)
	if len(words) == 0 {
		send(bot, chatID, "Словарь пуст. Напиши слово и нажми «Запомнить».")
		return
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Твой словарь (%d):\n\n", len(words)))
	for i, w := range words {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, w))
	}
	send(bot, chatID, sb.String())
}

func handleAsk(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message) {
	chatID := message.Chat.ID
	userID := int(message.From.ID)
	q := strings.TrimSpace(message.CommandArguments())
	if q == "" {
		send(bot, chatID, "Спроси что-нибудь про грамматику, например:\n/ask как работает частица は")
		return
	}
	think := sendThinking(bot, chatID)
	resp, err := claudeOne(clients, grammarSystem, q)
	deleteMsg(bot, chatID, think)
	if err != nil {
		send(bot, chatID, "Ошибка, попробуй ещё раз.")
		return
	}
	// Remember the topic (question) and the answer so "Уточнить"/"Потренироваться"
	// can build on them. Mode stays idle so plain word lookups still work.
	storage.SetState(db, userID, "", q, resp, 0)
	sendKb(bot, chatID, resp, askKeyboard())
}

// ---------- message entry (state router) ----------

func HandleMessage(ctx context.Context, bot *tgbotapi.BotAPI, message *tgbotapi.Message, clients *Clients, db *sql.DB) {
	userID := int(message.From.ID)
	storage.EnsureUser(db, userID)

	switch st := storage.GetState(db, userID); st.Mode {
	case "practice_compose":
		checkPracticeCompose(bot, clients, db, message, st)
	case "practice_translate":
		checkPracticeTranslate(bot, clients, db, message, st)
	case "clarify_compose", "clarify_translate":
		handleClarify(bot, clients, db, message, st)
	case "clarify_ask":
		handleAskClarify(bot, clients, db, message, st)
	case "reminder":
		checkReminder(bot, clients, db, message, st)
	default:
		flow1Lookup(bot, clients, db, message)
	}
}

// handleAskClarify answers a follow-up question about the previous /ask answer,
// then keeps the user in the /ask context (clarify again or practice the topic).
func handleAskClarify(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message, st storage.State) {
	chatID := message.Chat.ID
	userID := int(message.From.ID)

	think := sendThinking(bot, chatID)
	sys := styleRules + " You are a friendly Japanese tutor. Reply in Russian. " +
		"Earlier you explained this:\n" + st.TaskText + "\n" +
		"Answer the user's follow-up question about it in 2-4 short lines."
	resp, err := claudeOne(clients, sys, message.Text)
	deleteMsg(bot, chatID, think)
	if err != nil {
		resp = "Не смог объяснить, попробуй переформулировать."
	}
	// stay in /ask context: keep the topic, update the answer for further drilling
	storage.SetState(db, userID, "", st.Word, resp, 0)
	sendKb(bot, chatID, resp, askKeyboard())
}

// handleClarify answers the user's question about the current practice sentence,
// then puts them back into the practice stage they came from.
func handleClarify(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message, st storage.State) {
	chatID := message.Chat.ID
	userID := int(message.From.ID)

	think := sendThinking(bot, chatID)
	sys := styleRules + " You are a friendly Japanese tutor. Reply in Russian. " +
		"The user is practicing with this task/sentence:\n" + st.TaskText + "\n" +
		"Answer their question about it (grammar, particles, word meaning) in 2-4 short lines."
	resp, err := claudeOne(clients, sys, message.Text)
	deleteMsg(bot, chatID, think)
	if err != nil {
		resp = "Не смог объяснить, попробуй переформулировать."
	}
	send(bot, chatID, resp)

	if st.Mode == "clarify_translate" {
		storage.SetState(db, userID, "practice_translate", st.Word, st.TaskText, 0)
		sendTranslateTask(bot, chatID, st.TaskText)
	} else {
		storage.SetState(db, userID, "practice_compose", st.Word, st.TaskText, 0)
		sendComposeTask(bot, chatID, st.TaskText)
	}
}

// Flow 1 — learn a word
func flow1Lookup(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message) {
	chatID := message.Chat.ID
	userID := int(message.From.ID)

	think := sendThinking(bot, chatID)

	word := normalizeWord(message.Text)
	if word == "" {
		word = strings.TrimSpace(message.Text)
	}

	cfg := config.Load()
	var msgs []claude_api.ChatMessage
	for _, m := range cfg.FewShot {
		msgs = append(msgs, claude_api.ChatMessage{Role: m.Role, Content: m.Content})
	}
	msgs = append(msgs, buildHistory(db, userID)...)

	resp, err := claude_api.GetClaudeResponse(context.Background(), &clients.Claude, claude_api.ClaudeRequest{
		SystemPrompt: cfg.WordSystemPrompt,
		Messages:     msgs,
		UserMessage:  message.Text,
	})
	deleteMsg(bot, chatID, think)
	if err != nil {
		log.Printf("flow1 claude error: %v", err)
		send(bot, chatID, "Ошибка, попробуй ещё раз.")
		return
	}

	storage.SetCurrentWord(db, userID, word)
	storage.SaveConversationTurn(db, userID, message.Text, resp)
	sendKb(bot, chatID, resp, wordKeyboard())
}

func buildHistory(db *sql.DB, userID int) []claude_api.ChatMessage {
	turns, _ := storage.GetConversationHistory(db, userID, historyTurns)
	var msgs []claude_api.ChatMessage
	for _, t := range turns {
		msgs = append(msgs,
			claude_api.ChatMessage{Role: "user", Content: t.UserMessage},
			claude_api.ChatMessage{Role: "assistant", Content: t.BotResponse},
		)
	}
	return msgs
}

// normalizeWord strips lookup phrases so that "что значит окно" → "окно".
var lookupPhrases = []string{"что значит", "как переводится", "как будет", "как сказать", "перевод "}

func normalizeWord(message string) string {
	m := strings.TrimSpace(message)
	for _, p := range lookupPhrases {
		if idx := strings.Index(strings.ToLower(m), p); idx >= 0 {
			m = strings.TrimSpace(m[:idx] + m[idx+len(p):])
		}
	}
	// drop trailing/leading punctuation and collapse inner whitespace
	m = strings.Trim(m, " \t?!.,;:")
	return strings.Join(strings.Fields(m), " ")
}

// ---------- Flow 2 — practice ----------

func startPracticeRandom(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID int, exclude string) {
	word, err := storage.GetRandomVocabWord(db, userID, exclude)
	if err != nil || word == "" {
		send(bot, chatID, "Сначала выучи пару слов — напиши слово и нажми «Запомнить».")
		return
	}
	startPractice(bot, clients, db, chatID, userID, word)
}

// startPracticeFromTopic builds a practice task around a grammar topic/question
// (used by the /ask "Потренироваться" button) instead of a random vocab word.
func startPracticeFromTopic(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID int, topic string) {
	if strings.TrimSpace(topic) == "" {
		startPracticeRandom(bot, clients, db, chatID, userID, "")
		return
	}
	think := sendThinking(bot, chatID)
	sys := styleRules + " You are a Japanese tutor. The user wants to practice this topic/question: «" + topic + "». " +
		"Make a SHORT practice task in Russian: line 1 — one simple natural Russian sentence (5-8 words) that requires this grammar point or word. " +
		"Then 2-3 helper words 'русское — японский(чтение, romaji)'. Do NOT translate the whole sentence into Japanese."
	task, err := claudeOne(clients, sys, topic)
	deleteMsg(bot, chatID, think)
	if err != nil {
		send(bot, chatID, "Ошибка, попробуй ещё раз.")
		return
	}
	storage.SetState(db, userID, "practice_compose", topic, task, 0)
	sendComposeTask(bot, chatID, task)
}

func startPractice(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID int, word string) {
	if word == "" {
		send(bot, chatID, "Сначала найди слово — просто напиши его.")
		return
	}
	think := sendThinking(bot, chatID)
	sys := styleRules + " You are a Japanese tutor. Make a SHORT practice task in Russian for the word «" + word + "». " +
		"Output exactly: line 1 — one simple natural Russian sentence (5-8 words) that uses «" + word + "». " +
		"Then 2-3 lines, each a helper word the user will need: 'русское слово — японский(чтение, romaji)'. " +
		"Do NOT translate the whole sentence into Japanese. No extra text."
	task, err := claudeOne(clients, sys, word)
	deleteMsg(bot, chatID, think)
	if err != nil {
		send(bot, chatID, "Ошибка, попробуй /practice ещё раз.")
		return
	}
	storage.SetState(db, userID, "practice_compose", word, task, 0)
	sendComposeTask(bot, chatID, task)
}

func checkPracticeCompose(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message, st storage.State) {
	chatID := message.Chat.ID
	userID := int(message.From.ID)
	think := sendThinking(bot, chatID)

	ok, fb := judge(clients, "Translate the Russian sentence into Japanese.\nRussian task:\n"+st.TaskText+
		"\n\nUser's Japanese attempt: "+message.Text)
	if !ok {
		deleteMsg(bot, chatID, think)
		sendKb(bot, chatID, fb+"\n\nПопробуй ещё раз.", practiceKeyboard())
		return
	}

	// correct → move to stage 2 (translate a Japanese sentence to Russian)
	sys := styleRules + " Generate ONE short simple Japanese sentence that uses «" + st.Word + "» for the user to translate into Russian. " +
		"Output ONLY the Japanese sentence (every kanji with its reading in brackets) followed by ' (' + full romaji + ')'. " +
		"Then optional helper lines 'японский(чтение, romaji) — русский'. Do NOT give the Russian translation of the sentence."
	jp, err := claudeOne(clients, sys, st.Word)
	deleteMsg(bot, chatID, think)
	if err != nil {
		storage.ClearMode(db, userID)
		send(bot, chatID, "Ошибка. Напиши /practice.")
		return
	}
	storage.SetState(db, userID, "practice_translate", st.Word, jp, 0)
	send(bot, chatID, "Правильно!\n"+fb)
	sendTranslateTask(bot, chatID, jp)
}

func checkPracticeTranslate(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message, st storage.State) {
	chatID := message.Chat.ID
	userID := int(message.From.ID)
	think := sendThinking(bot, chatID)

	ok, fb := judge(clients, "Translate the Japanese sentence into Russian.\nJapanese:\n"+st.TaskText+
		"\n\nUser's Russian attempt: "+message.Text)
	deleteMsg(bot, chatID, think)
	if !ok {
		sendKb(bot, chatID, fb+"\n\nПопробуй ещё раз.", practiceKeyboard())
		return
	}
	storage.ClearMode(db, userID)
	sendKb(bot, chatID, "Правильно!\n"+fb+"\n\nЕщё раунд?", roundKeyboard())
}

// judge asks Claude to verdict an answer. Returns (ok, feedback-in-Russian).
func judge(clients *Clients, task string) (bool, string) {
	sys := styleRules + " You are a friendly Japanese tutor. Reply in Russian. Judge the user's answer to the task. " +
		"Your VERY FIRST line must be exactly 'VERDICT: ok' if the answer is essentially correct " +
		"(ignore minor typos and romaji-vs-kana), or 'VERDICT: retry' if it is wrong. " +
		"Then a blank line, then short feedback: if ok — confirm and show the natural Japanese with kanji(чтение) and romaji. " +
		"If retry — say what is off in 1-2 lines and show the correct version with romaji."
	resp, err := claudeOne(clients, sys, task)
	if err != nil {
		return false, "Ошибка проверки, попробуй ещё раз."
	}
	ok := strings.HasPrefix(strings.ToLower(strings.TrimSpace(resp)), "verdict: ok")
	if i := strings.IndexByte(resp, '\n'); i >= 0 {
		resp = strings.TrimSpace(resp[i+1:])
	} else {
		resp = ""
	}
	return ok, resp
}

// ---------- Flow 3 — SRS reminder answer ----------

func checkReminder(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message, st storage.State) {
	chatID := message.Chat.ID
	userID := int(message.From.ID)

	word := st.Word
	step := 1
	if w, s, err := storage.GetReminder(db, st.ReminderID); err == nil && w != "" {
		word, step = w, s
	}

	think := sendThinking(bot, chatID)
	ok, fb := judge(clients, "How do you say the word «"+word+"» in Japanese? User's answer: "+message.Text)
	deleteMsg(bot, chatID, think)
	storage.ClearMode(db, userID)

	if ok {
		next := step + 1
		if next > 4 {
			next = 4
		}
		storage.ScheduleReminder(db, userID, word, next, time.Now().Add(intervalFor(next)))
		send(bot, chatID, "Правильно!\n"+fb+"\n\nНапомню это слово ещё попозже.")
	} else {
		storage.ScheduleReminder(db, userID, word, 1, time.Now().Add(intervalFor(1)))
		send(bot, chatID, fb+"\n\nНичего страшного — напомню это слово снова скоро.")
	}
}

func intervalFor(step int) time.Duration {
	switch step {
	case 1:
		return 3 * time.Hour
	case 2:
		return 24 * time.Hour
	case 3:
		return 7 * 24 * time.Hour
	default:
		return 30 * 24 * time.Hour
	}
}

// SendReminder is called by the scheduler to ask the spaced-repetition question.
func SendReminder(bot *tgbotapi.BotAPI, userID int, word string) {
	sendKb(bot, int64(userID),
		fmt.Sprintf("Повторение!\nКак будет «%s» по-японски? Напиши свой вариант.", word),
		reminderKeyboard())
}

// ---------- callbacks ----------

func HandleCallbackQuery(bot *tgbotapi.BotAPI, clients *Clients, callbackQuery *tgbotapi.CallbackQuery, db *sql.DB) {
	data := callbackQuery.Data
	userID := int(callbackQuery.From.ID)
	chatID := callbackQuery.Message.Chat.ID
	answerCallback(bot, callbackQuery)
	storage.EnsureUser(db, userID)

	switch {
	case data == "memorize":
		st := storage.GetState(db, userID)
		if st.Word == "" {
			send(bot, chatID, "Не понял, какое слово сохранить. Напиши слово ещё раз.")
			return
		}
		storage.SaveVocab(db, userID, st.Word)
		if !storage.HasPendingReminder(db, userID, st.Word) {
			storage.ScheduleReminder(db, userID, st.Word, 1, time.Now().Add(intervalFor(1)))
		}
		send(bot, chatID, fmt.Sprintf("Добавил «%s» в словарь. Напомню по расписанию.", st.Word))

	case data == "know":
		sendKb(bot, chatID, "Хочешь попрактиковаться?", practiceOfferKeyboard())

	case data == "practice_yes":
		st := storage.GetState(db, userID)
		startPractice(bot, clients, db, chatID, userID, st.Word)

	case data == "practice_no":
		send(bot, chatID, "Хорошо. Пиши, когда увидишь что-то интересное.")

	case data == "ask_practice":
		startPracticeFromTopic(bot, clients, db, chatID, userID, storage.GetState(db, userID).Word)

	case data == "ask_clarify":
		st := storage.GetState(db, userID)
		ctx := st.TaskText
		if ctx == "" {
			ctx = callbackQuery.Message.Text
		}
		storage.SetState(db, userID, "clarify_ask", st.Word, ctx, 0)
		send(bot, chatID, "Что непонятно? Напиши вопрос.")

	case data == "dont_remember":
		st := storage.GetState(db, userID)
		word := st.Word
		if w, _, err := storage.GetReminder(db, st.ReminderID); err == nil && w != "" {
			word = w
		}
		storage.ClearMode(db, userID)
		if word == "" {
			send(bot, chatID, "Окей.")
			return
		}
		think := sendThinking(bot, chatID)
		ans, _ := claudeOne(clients, styleRules+" Reply in Russian. Show how the word «"+word+"» is in Japanese: kanji(чтение) (romaji) — перевод, plus one short example.", word)
		deleteMsg(bot, chatID, think)
		storage.ScheduleReminder(db, userID, word, 1, time.Now().Add(intervalFor(1)))
		send(bot, chatID, "Ничего страшного, вот как это:\n\n"+ans+"\n\nНапомню это слово снова скоро.")

	case data == "exit":
		storage.ClearMode(db, userID)
		send(bot, chatID, "Окей, закончили. Пиши слово, когда захочешь.")

	case data == "clarify":
		st := storage.GetState(db, userID)
		switch st.Mode {
		case "practice_compose":
			storage.SetState(db, userID, "clarify_compose", st.Word, st.TaskText, 0)
			send(bot, chatID, "Что именно непонятно в этом предложении? Напиши вопрос.")
		case "practice_translate":
			storage.SetState(db, userID, "clarify_translate", st.Word, st.TaskText, 0)
			send(bot, chatID, "Что именно непонятно в этом предложении? Напиши вопрос.")
		}

	case data == "round_yes":
		st := storage.GetState(db, userID)
		startPracticeRandom(bot, clients, db, chatID, userID, st.Word)

	case data == "round_no":
		storage.ClearMode(db, userID)
		send(bot, chatID, "Хорошо, на сегодня хватит. Молодец!")

	case data == "audio" || strings.HasPrefix(data, "audplay#") || data == "audback":
		handleAudio(bot, clients, db, callbackQuery, data)

	case strings.HasPrefix(data, "speech_speed:"):
		handleSpeechSpeed(bot, db, callbackQuery, data)
	}
}

// ---------- audio ----------

func handleAudio(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, cq *tgbotapi.CallbackQuery, data string) {
	chatID := cq.Message.Chat.ID
	userID := int(cq.From.ID)

	if data == "audback" {
		bot.Send(tgbotapi.NewEditMessageReplyMarkup(chatID, cq.Message.MessageID, wordKeyboard()))
		return
	}

	sentences := extractJapaneseSentences(cq.Message.Text)
	if len(sentences) == 0 {
		return
	}

	if data == "audio" {
		if len(sentences) == 1 {
			sendAudioMessage(clients, db, sentences[0], userID, bot)
			return
		}
		bot.Send(tgbotapi.NewEditMessageReplyMarkup(chatID, cq.Message.MessageID, audioNumberKeyboard(len(sentences))))
		return
	}

	// audplay#N
	if n, err := strconv.Atoi(strings.TrimPrefix(data, "audplay#")); err == nil && n >= 1 && n <= len(sentences) {
		sendAudioMessage(clients, db, sentences[n-1], userID, bot)
	}
}

func audioNumberKeyboard(n int) tgbotapi.InlineKeyboardMarkup {
	keyboard := tgbotapi.NewInlineKeyboardMarkup()
	row := tgbotapi.NewInlineKeyboardRow()
	for i := 1; i <= n; i++ {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("🔊 %d", i), fmt.Sprintf("audplay#%d", i)))
		if i%4 == 0 || i == n {
			keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
			row = tgbotapi.NewInlineKeyboardRow()
		}
	}
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("← Назад", "audback"),
	))
	return keyboard
}

var parenGroupRe = regexp.MustCompile(`[(（][^)）]*[)）]`)

// extractJapaneseSentences pulls clean Japanese (no readings, no romaji, no Russian)
// out of a bot message, one entry per line that contains Japanese.
func extractJapaneseSentences(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if idx := strings.Index(line, "—"); idx >= 0 {
			line = line[:idx]
		}
		line = parenGroupRe.ReplaceAllString(line, "")
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Пример:"))
		if containsJapanese(line) {
			out = append(out, line)
		}
	}
	return out
}

func containsJapanese(s string) bool {
	for _, r := range s {
		if (r >= 0x3040 && r <= 0x30FF) || (r >= 0x4E00 && r <= 0x9FFF) {
			return true
		}
	}
	return false
}

func sendAudioMessage(clients *Clients, db *sql.DB, text string, userID int, bot *tgbotapi.BotAPI) {
	speed := storage.GetUserSpeechSpeed(db, userID)
	audio, err := openai_api.GetTTSResponse(context.Background(), clients.OpenAI, speed, text)
	if err != nil {
		log.Printf("TTS error: %v", err)
		return
	}
	voice := tgbotapi.NewVoice(int64(userID), tgbotapi.FileBytes{Name: "audio.mp3", Bytes: audio})
	if _, err := bot.Send(voice); err != nil {
		log.Printf("send audio error: %v", err)
	}
}

// ---------- speech speed ----------

func getSpeechSpeedValues() map[float64]string {
	return map[float64]string{0.5: "Медленно", 0.75: "Нормально", 1.0: "Быстро"}
}

func speechSpeedInlineKeyboard() tgbotapi.InlineKeyboardMarkup {
	keyboard := tgbotapi.NewInlineKeyboardMarkup()
	row := tgbotapi.NewInlineKeyboardRow()
	keys := []float64{0.5, 0.75, 1.0}
	sort.Float64s(keys)
	values := getSpeechSpeedValues()
	for _, k := range keys {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(values[k], fmt.Sprintf("speech_speed:%.2f", k)))
	}
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
	return keyboard
}

func handleSpeechSpeed(bot *tgbotapi.BotAPI, db *sql.DB, cq *tgbotapi.CallbackQuery, data string) {
	speed, err := strconv.ParseFloat(strings.TrimPrefix(data, "speech_speed:"), 64)
	if err != nil {
		return
	}
	if err := storage.UpdateUserSpeechSpeed(db, int(cq.From.ID), speed); err != nil {
		log.Printf("update speed error: %v", err)
		return
	}
	label := getSpeechSpeedValues()[speed]
	bot.Send(tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID,
		fmt.Sprintf("Скорость озвучки: %s.", label)))
}

// ---------- access control ----------

func IsAllowedUser(update tgbotapi.Update, allowedUsers []int64) bool {
	var userID int64
	if update.Message != nil {
		userID = update.Message.From.ID
	} else if update.CallbackQuery != nil {
		userID = update.CallbackQuery.From.ID
	} else {
		return false
	}
	for _, id := range allowedUsers {
		if userID == id {
			return true
		}
	}
	return false
}
