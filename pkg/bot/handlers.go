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
	"unicode/utf8"

	claude_api "language-learning-bot/pkg/claude"
	"language-learning-bot/pkg/config"
	openai_api "language-learning-bot/pkg/openai"
	storage "language-learning-bot/pkg/storage"

	"github.com/anthropics/anthropic-sdk-go"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	openai "github.com/sashabaranov/go-openai"
)

const historyTurns = 6

// Interaction modes kept in user_state.mode for the "Запомнить" confirmation.
const (
	modeConfirmSave = "confirm_save" // bot asked «Запомнить слово «X»?»
	modeAwaitWord   = "await_word"   // user rejected the guess and types the right word
)

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

// askKeyboard — shown under every answer in /ask mode. Follow-up questions are
// typed directly (no button needed); «Закончить» leaves the mode.
func askKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Закончить", "exit"),
			tgbotapi.NewInlineKeyboardButtonData("Потренироваться", "ask_practice"),
		),
	)
}

// confirmSaveKeyboard — shown under «Запомнить слово «X»?».
func confirmSaveKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Да", "save_yes"),
			tgbotapi.NewInlineKeyboardButtonData("Нет", "save_no"),
		),
	)
}

// practiceExitKeyboard — shown when the user types something that doesn't look
// like a practice answer (probably forgot they were mid-practice).
func practiceExitKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Да, закончить", "end_practice"),
			tgbotapi.NewInlineKeyboardButtonData("Нет, продолжить", "resume_practice"),
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
			"/ask — вопрос по грамматике (дальше уточняй просто сообщениями)\n"+
			"/vocab — твой словарь; /vocab <часть слова> — поиск\n"+
			"/speech_speed — скорость озвучки")
	case "practice":
		startPracticeWeakest(bot, clients, db, chatID, userID, "")
	case "ask":
		handleAsk(bot, clients, db, message)
	case "vocab":
		if q := strings.TrimSpace(message.CommandArguments()); q != "" {
			sendVocabSearch(bot, db, chatID, userID, q)
		} else {
			sendVocab(bot, clients, db, chatID, userID, 0)
		}
	case "speech_speed":
		sendKb(bot, chatID, "Выбери скорость озвучки:", speechSpeedInlineKeyboard())
	case "healthz":
		send(bot, chatID, "OK")
	}
	return nil
}

// vocabPageSize keeps /vocab pages far under Telegram's 4096-char message cap.
const vocabPageSize = 10

func vocabMoreKeyboard(offset int) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Показать ещё 10", fmt.Sprintf("vocab_more#%d", offset)),
		),
	)
}

// backfillTranslations fills missing Japanese for the rows about to be shown —
// bounded by the page size, so at most a page worth of API calls.
func backfillTranslations(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID int, entries []storage.VocabEntry) {
	missing := false
	for _, e := range entries {
		if e.Translation == "" {
			missing = true
			break
		}
	}
	if !missing {
		return
	}
	think := sendThinking(bot, chatID)
	for i := range entries {
		if entries[i].Translation != "" {
			continue
		}
		if tr := translateWord(clients, entries[i].Word); tr != "" {
			storage.SetVocabTranslation(db, userID, entries[i].Word, tr)
			entries[i].Translation = tr
		}
	}
	deleteMsg(bot, chatID, think)
}

func vocabLine(sb *strings.Builder, n int, e storage.VocabEntry) {
	if e.Translation != "" {
		fmt.Fprintf(sb, "%d. %s — %s\n", n, e.Word, e.Translation)
	} else {
		fmt.Fprintf(sb, "%d. %s\n", n, e.Word)
	}
}

// sendVocab shows one page of the vocabulary (newest first) with a
// «Показать ещё 10» button while more pages remain.
func sendVocab(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID, offset int) {
	total, entries, err := storage.GetVocabPage(db, userID, offset, vocabPageSize)
	if err != nil || total == 0 {
		send(bot, chatID, "Словарь пуст. Напиши слово и нажми «Запомнить».")
		return
	}
	if len(entries) == 0 { // stale «ещё» button pointing past the end
		send(bot, chatID, "Это уже всё — слов больше нет.")
		return
	}

	backfillTranslations(bot, clients, db, chatID, userID, entries)

	var sb strings.Builder
	switch {
	case offset == 0 && total <= vocabPageSize:
		fmt.Fprintf(&sb, "Твой словарь (%d):\n\n", total)
	case offset == 0:
		fmt.Fprintf(&sb, "Твой словарь (%d), последние %d:\n\n", total, len(entries))
	default:
		fmt.Fprintf(&sb, "Твой словарь (%d), слова %d–%d:\n\n", total, offset+1, offset+len(entries))
	}
	for i, e := range entries {
		vocabLine(&sb, offset+i+1, e)
	}

	if next := offset + len(entries); next < total {
		sendKb(bot, chatID, sb.String(), vocabMoreKeyboard(next))
		return
	}
	send(bot, chatID, sb.String())
}

// sendVocabSearch handles «/vocab <часть слова>» — a plain local substring
// search over saved words, no API calls involved.
func sendVocabSearch(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int, q string) {
	entries, err := storage.SearchVocab(db, userID, strings.ToLower(q), 20)
	if err != nil || len(entries) == 0 {
		send(bot, chatID, fmt.Sprintf("По «%s» ничего не нашёл. /vocab — весь список.", q))
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Нашёл (%d):\n\n", len(entries))
	for i, e := range entries {
		vocabLine(&sb, i+1, e)
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
	// Enter ask mode: the topic (question) and answer are kept so follow-up
	// messages and «Потренироваться» build on them. The user stays in this
	// mode — typing more questions, no buttons — until «Закончить».
	storage.SetState(db, userID, "ask", q, resp, 0)
	sendKb(bot, chatID, resp, askKeyboard())
}

// ---------- message entry (state router) ----------

func HandleMessage(ctx context.Context, bot *tgbotapi.BotAPI, message *tgbotapi.Message, clients *Clients, db *sql.DB) {
	userID := int(message.From.ID)
	chatID := message.Chat.ID
	storage.EnsureUser(db, userID)

	switch st := storage.GetState(db, userID); st.Mode {
	case "practice_compose":
		if !looksLikePracticeAnswer(st.Mode, message.Text) {
			offerPracticeExit(bot, db, chatID, userID, "paused_compose", st)
			return
		}
		checkPracticeCompose(bot, clients, db, message, st)
	case "practice_translate":
		if !looksLikePracticeAnswer(st.Mode, message.Text) {
			offerPracticeExit(bot, db, chatID, userID, "paused_translate", st)
			return
		}
		checkPracticeTranslate(bot, clients, db, message, st)
	case "paused_compose", "paused_translate":
		// user typed instead of choosing a button — re-ask the exit question
		// rather than leaving them stuck.
		offerPracticeExit(bot, db, chatID, userID, st.Mode, st)
	case "clarify_compose", "clarify_translate":
		handleClarify(bot, clients, db, message, st)
	case "ask", "clarify_ask": // clarify_ask: legacy state left by the old «Уточнить» button
		handleAskFollowup(bot, clients, db, message, st)
	case "reminder":
		checkReminder(bot, clients, db, message, st)
	case modeAwaitWord:
		handleAwaitWord(bot, clients, db, message)
	default:
		// modeConfirmSave lands here on purpose: if the user ignores the
		// confirmation buttons and types something, treat it as a fresh lookup
		// rather than trapping them in the save flow.
		flow1Lookup(bot, clients, db, message)
	}
}

// handleAwaitWord takes the word typed after the user rejected the bot's guess
// and re-asks for confirmation instead of saving it blindly.
func handleAwaitWord(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message) {
	chatID := message.Chat.ID
	userID := int(message.From.ID)

	think := sendThinking(bot, chatID)
	word := resolveHeadword(clients, message.Text)
	if word == "" {
		deleteMsg(bot, chatID, think)
		send(bot, chatID, "Нужно одно слово, без лишнего. Попробуй ещё раз.")
		return
	}
	translation := translateWord(clients, word)
	deleteMsg(bot, chatID, think)
	askSaveConfirmation(bot, db, chatID, userID, word, translation)
}

// handleAskFollowup answers the next question typed in /ask mode and keeps the
// user in the mode: every message is a follow-up until «Закончить».
func handleAskFollowup(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message, st storage.State) {
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
	// keep the topic, remember the latest answer as context for the next question
	storage.SetState(db, userID, "ask", st.Word, resp, 0)
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

// ---------- vocabulary word sanitation ----------

// fillerPrefixes are conversational lead-ins typed before the actual word,
// e.g. "А крыша ?" — without stripping these they end up saved verbatim.
var fillerPrefixes = []string{"а ", "и ", "ну ", "вот ", "это ", "слово ", "ещё ", "еще "}

// sanitizeWord reduces raw text to one clean lowercase word, or "" if it cannot
// be reduced to a single word. "" is the signal that the caller must resolve it
// (via Claude or by asking the user) rather than saving junk.
func sanitizeWord(raw string) string {
	w := strings.ToLower(normalizeWord(raw))
	// peel repeated fillers: "а вот крыша" → "крыша"
	for {
		stripped := w
		for _, p := range fillerPrefixes {
			stripped = strings.TrimPrefix(stripped, p)
		}
		stripped = strings.TrimSpace(stripped)
		if stripped == w {
			break
		}
		w = stripped
	}
	w = strings.Trim(w, " \t?!.,;:«»\"'()")
	if w == "" || strings.ContainsAny(w, " \t") {
		return "" // still a phrase, not a single word
	}
	if utf8.RuneCountInString(w) > 32 {
		return ""
	}
	return w
}

// resolveHeadword picks the single dictionary word to store in the vocabulary.
// Clean single-word input is handled locally; only genuinely messy input costs
// a Claude call.
func resolveHeadword(clients *Clients, raw string) string {
	if w := sanitizeWord(raw); w != "" {
		return w
	}
	sys := "Extract the single Russian word the user asked about. " +
		"Reply with exactly ONE word in dictionary form (nominative singular; infinitive for verbs), " +
		"lowercase, no punctuation, no quotes, no explanation. " +
		"If several words are present, pick the main noun or verb."
	resp, err := claudeOne(clients, sys, raw)
	if err != nil {
		log.Printf("headword extraction error: %v", err)
		return ""
	}
	return sanitizeWord(resp)
}

// translateWord returns the Japanese for a single Russian word as one compact
// line "kanji(чтение) (romaji)", or "" on failure.
func translateWord(clients *Clients, word string) string {
	sys := "Translate the single Russian word «" + word + "» into Japanese. " +
		"Reply with EXACTLY one line and nothing else: the Japanese word with every kanji immediately " +
		"followed by its hiragana reading in round brackets, then one space, then the full romaji in round brackets. " +
		"No Russian, no explanation, no example. Example for «растение»: 植物(しょくぶつ) (shokubutsu)"
	resp, err := claudeOne(clients, sys, word)
	if err != nil {
		log.Printf("translateWord error: %v", err)
		return ""
	}
	for _, line := range strings.Split(resp, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// askSaveConfirmation puts the user in the confirm step (with the translation
// already shown) instead of saving blindly. The translation is stashed in
// task_text so "Да" can persist it without a second API call.
func askSaveConfirmation(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int, word, translation string) {
	storage.SetState(db, userID, modeConfirmSave, word, translation, 0)
	msg := fmt.Sprintf("Запомнить слово «%s»?", word)
	if translation != "" {
		msg = fmt.Sprintf("Запомнить слово «%s» — %s?", word, translation)
	}
	sendKb(bot, chatID, msg, confirmSaveKeyboard())
}

// ---------- Flow 2 — practice ----------

// startPracticeWeakest drills the word the user knows least well (lowest SRS
// step; never-practiced words first) instead of a random one, so practice time
// goes where it helps most.
func startPracticeWeakest(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID int, exclude string) {
	word, err := storage.GetWeakVocabWord(db, userID, exclude)
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
		startPracticeWeakest(bot, clients, db, chatID, userID, "")
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

// looksLikePracticeAnswer guesses whether the message is a genuine attempt at
// the current practice task, versus the user forgetting they are in practice and
// typing something else (e.g. a Russian word to look up). False → offer to exit.
func looksLikePracticeAnswer(mode, text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	switch mode {
	case "practice_compose":
		// a compose answer is Japanese (kana/kanji) or romaji (Latin letters);
		// pure Russian is never a valid answer to "compose this in Japanese".
		return containsJapanese(t) || containsLatin(t)
	case "practice_translate":
		// a translate answer is a Russian phrase, so Cyrillic is expected here —
		// only flag Japanese input, which clearly isn't a Russian translation.
		return !containsJapanese(t)
	}
	return true
}

func containsLatin(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// offerPracticeExit pauses practice and asks whether to finish it. The task is
// preserved so "Нет, продолжить" can resume exactly where the user was.
func offerPracticeExit(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int, pausedMode string, st storage.State) {
	storage.SetState(db, userID, pausedMode, st.Word, st.TaskText, 0)
	sendKb(bot, chatID, "Ты сейчас на практике. Закончить её?", practiceExitKeyboard())
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
		"Your VERY FIRST line must be exactly 'VERDICT: ok' or 'VERDICT: retry'. " +
		"Say 'VERDICT: ok' if the answer is essentially correct: ignore minor typos and romaji-vs-kana, and accept any " +
		"phrasing a native speaker would consider fine — there is usually more than one correct translation. " +
		"When you are not confident the answer is actually wrong, prefer 'VERDICT: ok'. " +
		"Say 'VERDICT: retry' ONLY for a real, identifiable mistake. " +
		"Then a blank line, then short feedback: if ok — confirm and show the natural Japanese with kanji(чтение) and romaji. " +
		"If retry — name the exact mistake (which particle, conjugation or word, and why it is wrong) in 1-2 lines " +
		"and show the correct version with romaji."
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
		storage.ScheduleReminder(db, userID, word, next, time.Now().Add(intervalFor(next)))
		send(bot, chatID, "Правильно!\n"+fb+"\n\nНапомню это слово ещё попозже.")
	} else {
		back := lapseStep(step)
		storage.ScheduleReminder(db, userID, word, back, time.Now().Add(intervalFor(back)))
		send(bot, chatID, fb+"\n\nНичего страшного — напомню это слово снова скоро.")
	}
}

func intervalFor(step int) time.Duration {
	switch {
	case step <= 1:
		return 3 * time.Hour
	case step == 2:
		return 24 * time.Hour
	case step == 3:
		return 7 * 24 * time.Hour
	case step == 4:
		return 30 * 24 * time.Hour
	case step == 5:
		return 60 * 24 * time.Hour
	case step == 6:
		return 120 * 24 * time.Hour
	default:
		return 180 * 24 * time.Hour
	}
}

// lapseStep is the SRS step after a wrong answer: drop two steps instead of a
// full reset, so one bad day doesn't erase months of progress (Anki-style lapse).
// An explicit «Не помню» still resets to step 1 — the user said they forgot.
func lapseStep(step int) int {
	if step <= 3 {
		return 1
	}
	return step - 2
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
		think := sendThinking(bot, chatID)
		word := resolveHeadword(clients, st.Word)
		if word == "" {
			deleteMsg(bot, chatID, think)
			storage.SetState(db, userID, modeAwaitWord, "", "", 0)
			send(bot, chatID, "Не понял, какое слово запомнить. Напиши его одним словом.")
			return
		}
		translation := translateWord(clients, word)
		deleteMsg(bot, chatID, think)
		askSaveConfirmation(bot, db, chatID, userID, word, translation)

	case data == "save_yes":
		st := storage.GetState(db, userID)
		if st.Mode != modeConfirmSave {
			send(bot, chatID, "Это подтверждение устарело. Нажми «Запомнить» ещё раз.")
			return
		}
		word := sanitizeWord(st.Word)
		if word == "" {
			send(bot, chatID, "Не понял, какое слово сохранить. Напиши слово ещё раз.")
			return
		}
		translation := st.TaskText // stashed by askSaveConfirmation
		storage.SaveVocab(db, userID, word, translation)
		if !storage.HasPendingReminder(db, userID, word) {
			storage.ScheduleReminder(db, userID, word, 1, time.Now().Add(intervalFor(1)))
		}
		storage.SetCurrentWord(db, userID, word) // also clears the confirm mode
		msg := fmt.Sprintf("Добавил «%s» в словарь. Напомню по расписанию.", word)
		if translation != "" {
			msg = fmt.Sprintf("Добавил «%s» — %s в словарь. Напомню по расписанию.", word, translation)
		}
		send(bot, chatID, msg)

	case data == "save_no":
		storage.SetState(db, userID, modeAwaitWord, "", "", 0)
		send(bot, chatID, "Ок, не добавляю. Напиши одним словом, что запомнить.")

	case data == "know":
		sendKb(bot, chatID, "Хочешь попрактиковаться?", practiceOfferKeyboard())

	case data == "practice_yes":
		st := storage.GetState(db, userID)
		startPractice(bot, clients, db, chatID, userID, st.Word)

	case data == "practice_no":
		send(bot, chatID, "Хорошо. Пиши, когда увидишь что-то интересное.")

	case data == "ask_practice":
		startPracticeFromTopic(bot, clients, db, chatID, userID, storage.GetState(db, userID).Word)

	case data == "ask_clarify": // legacy «Уточнить» buttons on old messages
		st := storage.GetState(db, userID)
		ctx := st.TaskText
		if ctx == "" {
			ctx = callbackQuery.Message.Text
		}
		storage.SetState(db, userID, "ask", st.Word, ctx, 0)
		send(bot, chatID, "Просто напиши свой вопрос сообщением.")

	case strings.HasPrefix(data, "vocab_more#"):
		if off, err := strconv.Atoi(strings.TrimPrefix(data, "vocab_more#")); err == nil && off > 0 {
			sendVocab(bot, clients, db, chatID, userID, off)
		}

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

	case data == "end_practice":
		storage.ClearMode(db, userID)
		send(bot, chatID, "Практика завершена. Напиши слово — переведу и предложу запомнить.")

	case data == "resume_practice":
		st := storage.GetState(db, userID)
		switch st.Mode {
		case "paused_translate":
			storage.SetState(db, userID, "practice_translate", st.Word, st.TaskText, 0)
			send(bot, chatID, "Продолжаем практику.")
			sendTranslateTask(bot, chatID, st.TaskText)
		case "paused_compose":
			storage.SetState(db, userID, "practice_compose", st.Word, st.TaskText, 0)
			send(bot, chatID, "Продолжаем практику.")
			sendComposeTask(bot, chatID, st.TaskText)
		default:
			send(bot, chatID, "Ок.")
		}

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
		startPracticeWeakest(bot, clients, db, chatID, userID, st.Word)

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
