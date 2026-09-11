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
	"language-learning-bot/pkg/jisho"
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
	Claude   anthropic.Client
	OpenAI   *openai.Client
	DailyCap int // max user-initiated API interactions per user per day; 0 = unlimited
}

// styleRules is prepended to every model prompt so all answers share one style.
const styleRules = "Plain text only — no Markdown, no #, no *, no bold, no headers, no --- dividers. " +
	"Be short and focused: a few lines, answer only what was asked, no sections, no lecture. " +
	"Japanese style: every kanji is immediately followed by its reading in round brackets, like 育(そだ). " +
	"Give romaji for a whole word or sentence once, in pure Latin letters only — never use square brackets, " +
	"never mix Cyrillic into romaji, never leave part of a word without romaji. Example: 育(そだ)っています (sodatte imasu). " +
	"No emojis."

// uncertaintyRule (research 1a): a wrong explanation reads exactly as fluently
// as a right one, so the model must flag its own shaky points — but not hedge
// on textbook basics, which would just be noise.
const uncertaintyRule = " If you are not fully sure about a point — a rare usage, a dialect or register nuance, " +
	"something native speakers disagree on, or anything you might be misremembering — say so in one short phrase " +
	"(например: 'тут не уверен на 100%') instead of stating it as confidently as basic grammar. " +
	"Never invent a rule to sound complete. Do NOT hedge on standard textbook grammar (particles, basic conjugation, " +
	"common structures) — hedge only where the uncertainty is real."

const grammarSystem = styleRules + " You are a friendly Japanese tutor. " +
	"The user asks a grammar question in Russian. Answer in Russian, casually. Do not ask follow-up questions." +
	uncertaintyRule

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

// practiceRetryKeyboard — shown after a 'retry' verdict: the user may dispute it.
func practiceRetryKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Закончить", "exit"),
			tgbotapi.NewInlineKeyboardButtonData("Уточнить", "clarify"),
			tgbotapi.NewInlineKeyboardButtonData("Оспорить", "contest#practice"),
		),
	)
}

// playKeyboard — a lone 🔊 button that plays one saved vocab entry.
func playKeyboard(vocabID int) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔊", fmt.Sprintf("vocab_play#%d", vocabID)),
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

// reminderContestKeyboard — reminder buttons plus «Оспорить», shown when the
// answer was rejected or taken for a non-answer and the call may be wrong.
func reminderContestKeyboard(reminderID int) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Закончить", "exit"),
			tgbotapi.NewInlineKeyboardButtonData("Не помню", "dont_remember"),
			tgbotapi.NewInlineKeyboardButtonData("Оспорить", fmt.Sprintf("contest#rem#%d", reminderID)),
		),
	)
}

// srsOfferKeyboard — shown with the «сегодня повторяем N слов» announcement.
func srsOfferKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Начать", "srs_start"),
			tgbotapi.NewInlineKeyboardButtonData("Позже", "srs_later"),
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
		if capOK(bot, clients, db, chatID, userID) {
			startPracticeWeakest(bot, clients, db, chatID, userID, "")
		}
	case "ask":
		// an empty /ask only prints the usage hint — no API call, no budget spent
		if strings.TrimSpace(message.CommandArguments()) == "" || capOK(bot, clients, db, chatID, userID) {
			handleAsk(bot, clients, db, message)
		}
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

// vocabPageKeyboard — 🔊 to pick a word to hear, plus paging while more remain.
func vocabPageKeyboard(offset int, more bool) tgbotapi.InlineKeyboardMarkup {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔊 Озвучить слово", fmt.Sprintf("vocab_audio#%d", offset)),
		),
	)
	if more {
		kb.InlineKeyboard = append(kb.InlineKeyboard, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Показать ещё 10", fmt.Sprintf("vocab_more#%d", offset+vocabPageSize)),
		))
	}
	return kb
}

// vocabAudioKeyboard — one numbered 🔊 per word on the page, keyed by vocab id
// so the buttons stay valid even if the list shifts underneath.
func vocabAudioKeyboard(entries []storage.VocabEntry, offset int) tgbotapi.InlineKeyboardMarkup {
	return numberedAudioKeyboard(len(entries),
		func(i int) string { return fmt.Sprintf("vocab_play#%d", entries[i-1].ID) },
		fmt.Sprintf("vocab_back#%d", offset))
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
		if tr, _ := translateWord(clients, db, entries[i].Word); tr != "" {
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

	sendKb(bot, chatID, sb.String(), vocabPageKeyboard(offset, offset+len(entries) < total))
}

// playVocabWord voices the Japanese headword of one saved entry.
func playVocabWord(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID, id int) {
	e, err := storage.GetVocabEntry(db, userID, id)
	if err != nil {
		send(bot, chatID, "Не нашёл это слово в словаре.")
		return
	}
	jp := japaneseHeadword(e.Translation)
	if jp == "" {
		send(bot, chatID, "У этого слова ещё нет японского перевода — открой /vocab, он подтянется.")
		return
	}
	sendAudioMessage(clients, db, jp, userID, bot)
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

	st := storage.GetState(db, userID)
	switch st.Mode {
	case "paused_compose", "paused_translate", "reminder", modeSrsOffer:
		// paused/offer: just a re-prompt, no API; reminder: SRS answers are never capped
	default:
		if !capOK(bot, clients, db, chatID, userID) {
			return
		}
	}

	switch st.Mode {
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
		// modeConfirmSave and modeSrsOffer land here on purpose: if the user
		// ignores the buttons and types something, treat it as a fresh lookup
		// rather than trapping them. flow1Lookup resets the mode itself, so an
		// ignored announcement is simply re-offered after the usual throttle.
		if !capOK(bot, clients, db, chatID, userID) {
			return
		}
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
	translation, verified := translateWord(clients, db, word)
	deleteMsg(bot, chatID, think)
	askSaveConfirmation(bot, db, chatID, userID, word, translation, verified)
}

// handleAskFollowup answers the next question typed in /ask mode and keeps the
// user in the mode: every message is a follow-up until «Закончить».
func handleAskFollowup(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message, st storage.State) {
	chatID := message.Chat.ID
	userID := int(message.From.ID)

	think := sendThinking(bot, chatID)
	sys := styleRules + " You are a friendly Japanese tutor. Reply in Russian. " +
		"Earlier you explained this:\n" + st.TaskText + "\n" +
		"Answer the user's follow-up question about it in 2-4 short lines." + uncertaintyRule
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
		"Answer their question about it (grammar, particles, word meaning) in 2-4 short lines." + uncertaintyRule
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
// line "kanji(чтение) (romaji)", or "" on failure, plus whether Jisho confirmed
// the reading. Results are cached globally (a word's translation doesn't depend
// on the user); an unverified cached line is re-checked against Jisho on the
// next hit, so a temporary Jisho outage never sticks.
func translateWord(clients *Clients, db *sql.DB, word string) (string, bool) {
	if tr, verified, ok := storage.GetCachedTranslation(db, word); ok {
		if !verified {
			if fixed, v := verifyWithJisho(clients, tr); v {
				storage.SetCachedTranslation(db, word, fixed, true)
				return fixed, true
			}
		}
		return tr, verified
	}
	sys := "Translate the single Russian word «" + word + "» into Japanese. " +
		"Reply with EXACTLY one line and nothing else: the Japanese word with every kanji immediately " +
		"followed by its hiragana reading in round brackets, then one space, then the full romaji in round brackets. " +
		"No Russian, no explanation, no example. Example for «растение»: 植物(しょくぶつ) (shokubutsu)"
	resp, err := claudeOne(clients, sys, word)
	if err != nil {
		log.Printf("translateWord error: %v", err)
		return "", false
	}
	tr := firstLine(resp)
	if tr == "" {
		return "", false
	}
	tr, verified := verifyWithJisho(clients, tr)
	storage.SetCachedTranslation(db, word, tr, verified)
	return tr, verified
}

// kanjiReadingRe matches one kanji run with its bracketed reading: 遊(あそ).
var kanjiReadingRe = regexp.MustCompile(`[\p{Han}々〆ヶ]+[(（]([^)）]*)[)）]`)

// splitHeadword takes a translation line like "遊(あそ)び (asobi)" and returns
// the written form "遊び" and its full kana reading "あそび". A kana-only line
// ("ありがとう (arigatou)") yields the same string for both. Returns "", "" when
// there is no Japanese to work with.
func splitHeadword(tr string) (written, reading string) {
	jp := strings.TrimSpace(tr)
	if i := strings.LastIndex(jp, " ("); i >= 0 { // drop the trailing romaji group
		jp = strings.TrimSpace(jp[:i])
	}
	written = strings.TrimSpace(parenGroupRe.ReplaceAllString(jp, ""))
	reading = strings.TrimSpace(kanjiReadingRe.ReplaceAllString(jp, "$1"))
	if !containsJapanese(written) {
		return "", ""
	}
	return written, reading
}

// japaneseHeadword is the bare Japanese word from a translation line — what TTS
// should read aloud ("屋根(やね) (yane)" → "屋根").
func japaneseHeadword(tr string) string {
	written, _ := splitHeadword(tr)
	return written
}

// verifyWithJisho checks the model's translation against the Jisho dictionary:
// the written form must exist and its reading must match. On a mismatch the
// dictionary wins and the line is re-rendered with the correct reading. If
// Jisho is unreachable or doesn't know the word, the model's line is kept
// (graceful degradation). Returns the possibly-corrected line and whether the
// reading was confirmed.
func verifyWithJisho(clients *Clients, tr string) (string, bool) {
	written, reading := splitHeadword(tr)
	if written == "" {
		return tr, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entries, err := jisho.Lookup(ctx, written)
	if err != nil {
		log.Printf("jisho lookup %q failed, keeping model reading: %v", written, err)
		return tr, false
	}
	dict := jisho.VerifyReading(entries, written)
	if dict == "" {
		log.Printf("jisho: %q not in dictionary, keeping model reading", written)
		return tr, false
	}
	if dict == reading {
		return tr, true
	}
	log.Printf("jisho: reading mismatch for %q — model %q, dictionary %q; using dictionary", written, reading, dict)
	sys := styleRules + " The dictionary reading of «" + written + "» is «" + dict + "». " +
		"Output exactly one line and nothing else: «" + written + "» with every kanji immediately followed by its " +
		"reading in round brackets (the readings together must spell exactly «" + dict + "»), then a space, " +
		"then the romaji of that reading in round brackets."
	fixed, err := claudeOne(clients, sys, written)
	if err != nil || firstLine(fixed) == "" {
		return written + "(" + dict + ")", true // right reading even without romaji beats a wrong one
	}
	return firstLine(fixed), true
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// askSaveConfirmation puts the user in the confirm step (with the translation
// already shown) instead of saving blindly. The translation is stashed in
// task_text so "Да" can persist it without a second API call.
func askSaveConfirmation(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int, word, translation string, verified bool) {
	storage.SetState(db, userID, modeConfirmSave, word, translation, 0)
	msg := fmt.Sprintf("Запомнить слово «%s»?", word)
	if translation != "" {
		msg = fmt.Sprintf("Запомнить слово «%s» — %s?", word, translation)
		if verified {
			msg += "\n(чтение сверено с Jisho)"
		}
	}
	sendKb(bot, chatID, msg, confirmSaveKeyboard())
}

// capOK counts one user-initiated API interaction against the daily budget and
// tells the user when it is exhausted. A DB hiccup never locks anyone out.
func capOK(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID int) bool {
	if clients.DailyCap <= 0 {
		return true
	}
	n, err := storage.BumpDailyUsage(db, userID, time.Now().Format("2006-01-02"))
	if err != nil {
		log.Printf("usage counter error: %v", err)
		return true
	}
	if n > clients.DailyCap {
		send(bot, chatID, fmt.Sprintf("На сегодня лимит запросов исчерпан (%d в день). "+
			"Напоминания продолжат приходить, а новые слова и практика — завтра.", clients.DailyCap))
		return false
	}
	return true
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

// looksLikePracticeAnswer is the free, zero-false-positive pre-filter for "this
// cannot be an answer": pure Russian where Japanese/romaji is expected, or
// Japanese where a Russian translation is expected. False → offer to exit
// without spending an API call. Everything that passes is still classified by
// the judge, whose 'offtrack' verdict catches the ambiguous cases — e.g. a
// stray Russian word typed during the translate stage.
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
	v, fb := judgeAnswer(clients, composeTask(st.TaskText, message.Text), message.Text, true)
	deleteMsg(bot, chatID, think)
	switch v {
	case verdictOffTrack:
		offerPracticeExit(bot, db, chatID, userID, "paused_compose", st)
	case verdictRetry:
		storage.SetLastAnswer(db, userID, message.Text)
		sendKb(bot, chatID, fb+"\n\nПопробуй ещё раз.", practiceRetryKeyboard())
	default:
		advanceToTranslateStage(bot, clients, db, chatID, userID, st, "Правильно!\n"+fb)
	}
}

func composeTask(task, answer string) string {
	return "Translate the Russian sentence into Japanese.\nRussian task:\n" + task + "\n\nUser's Japanese attempt: " + answer
}

func translateTask(task, answer string) string {
	return "Translate the Japanese sentence into Russian.\nJapanese:\n" + task + "\n\nUser's Russian attempt: " + answer
}

// advanceToTranslateStage is the shared "compose answer accepted" path: show the
// praise, then generate the stage-2 Japanese sentence to translate into Russian.
func advanceToTranslateStage(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID int, st storage.State, praise string) {
	think := sendThinking(bot, chatID)
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
	send(bot, chatID, praise)
	sendTranslateTask(bot, chatID, jp)
}

func checkPracticeTranslate(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, message *tgbotapi.Message, st storage.State) {
	chatID := message.Chat.ID
	userID := int(message.From.ID)
	think := sendThinking(bot, chatID)

	v, fb := judgeAnswer(clients, translateTask(st.TaskText, message.Text), message.Text, false)
	deleteMsg(bot, chatID, think)
	switch v {
	case verdictOffTrack:
		offerPracticeExit(bot, db, chatID, userID, "paused_translate", st)
	case verdictRetry:
		storage.SetLastAnswer(db, userID, message.Text)
		sendKb(bot, chatID, fb+"\n\nПопробуй ещё раз.", practiceRetryKeyboard())
	default:
		storage.ClearMode(db, userID)
		sendKb(bot, chatID, "Правильно!\n"+fb+"\n\nЕщё раунд?", roundKeyboard())
	}
}

// handleContestPractice is «Оспорить» on a practice verdict: a second,
// independent and deliberately more permissive review of the same answer.
func handleContestPractice(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID int) {
	st := storage.GetState(db, userID)
	if st.LastAnswer == "" || (st.Mode != "practice_compose" && st.Mode != "practice_translate") {
		send(bot, chatID, "Оспаривать уже нечего — это задание закрыто.")
		return
	}
	task := composeTask(st.TaskText, st.LastAnswer)
	if st.Mode == "practice_translate" {
		task = translateTask(st.TaskText, st.LastAnswer)
	}
	think := sendThinking(bot, chatID)
	v, fb := judgeWith(clients, contestSystem, task)
	deleteMsg(bot, chatID, think)
	storage.SetLastAnswer(db, userID, "") // one appeal per answer
	if v != verdictOK {
		sendKb(bot, chatID, "Перепроверил внимательно — всё же ошибка.\n"+fb+"\n\nПопробуй ещё раз.", practiceKeyboard())
		return
	}
	if st.Mode == "practice_translate" {
		storage.ClearMode(db, userID)
		sendKb(bot, chatID, "Ты прав, засчитываю!\n"+fb+"\n\nЕщё раунд?", roundKeyboard())
		return
	}
	advanceToTranslateStage(bot, clients, db, chatID, userID, st, "Ты прав, засчитываю!\n"+fb)
}

// handleContestReminder is «Оспорить» on a reminder verdict. If overturned, the
// lapse that was scheduled is dropped and the word advances as if correct.
func handleContestReminder(bot *tgbotapi.BotAPI, clients *Clients, db *sql.DB, chatID int64, userID, reminderID int) {
	st := storage.GetState(db, userID)
	word, step, err := storage.GetReminder(db, reminderID)
	if err != nil || word == "" || st.LastAnswer == "" {
		send(bot, chatID, "Оспаривать уже нечего.")
		return
	}
	think := sendThinking(bot, chatID)
	v, fb := judgeWith(clients, contestSystem, "How do you say the word «"+word+"» in Japanese? User's answer: "+st.LastAnswer)
	deleteMsg(bot, chatID, think)
	storage.SetLastAnswer(db, userID, "")
	if v != verdictOK {
		send(bot, chatID, "Перепроверил внимательно — всё же не то.\n"+fb)
		return
	}
	storage.DeletePendingReminders(db, userID, word)
	next := step + 1
	storage.ScheduleReminder(db, userID, word, next, time.Now().Add(intervalFor(next)))
	session := st.TaskText == srsSessionFlag
	storage.ClearMode(db, userID)
	send(bot, chatID, "Ты прав, засчитываю!\n"+fb+"\n\nНапомню это слово ещё попозже.")
	continueSrsSession(bot, db, chatID, userID, session)
}

const judgeBase = styleRules + " You are a friendly Japanese tutor. Reply in Russian. Judge the user's answer to the task. "

// judgeGrading is shared by both judge prompts: lean toward accepting valid
// alternatives, and when rejecting name the exact mistake plus one natural
// alternative (research 6). Romaji tolerance is spelled out deliberately — a
// loose romanisation like "buruuberi" for ブルーベリー is a correct answer.
const judgeGrading = "'VERDICT: ok' — the answer is essentially correct: ignore minor typos, and treat romaji exactly " +
	"as well as kana or kanji — ANY readable romanisation of the right word counts, including one without macrons, " +
	"with doubled vowels, or not following Hepburn. Accept any phrasing a native speaker would consider fine — there " +
	"is usually more than one correct translation. When you are not confident the answer is actually wrong, prefer ok. " +
	"'VERDICT: retry' — ONLY for a real, identifiable mistake in a genuine attempt. " +
	"Then a blank line, then short feedback. If ok: confirm and show the natural Japanese with kanji(чтение) and romaji, " +
	"then, if a common natural alternative exists, ONE more line 'Ещё можно: ...' with kanji(чтение) and romaji. " +
	"If retry: name the exact mistake (which particle, conjugation or word, and why it is wrong) in 1-2 lines, " +
	"show the correct version with romaji, then, if a common natural alternative exists, ONE line 'Ещё можно: ...'."

// judgeSystem also classifies non-answers as 'offtrack' (research 5) so a stray
// lookup is never scored as a wrong answer. Used only where the message could
// genuinely not be an attempt — see offtrackAllowed.
const judgeSystem = judgeBase +
	"Your VERY FIRST line must be exactly one of: 'VERDICT: ok', 'VERDICT: retry', 'VERDICT: offtrack'. " +
	"'VERDICT: offtrack' — the message is not an attempt at the task at all: an unrelated word or phrase " +
	"(probably something the user wants looked up), a question, a request, or small talk. " +
	"A clumsy, misspelled, oddly romanised or plainly wrong attempt is NOT offtrack — judge it as ok or retry. " +
	"Output nothing after an offtrack verdict. " + judgeGrading

// judgeBinarySystem has no offtrack option: used when the message is certainly
// an attempt, so a real answer cannot be discarded as "not an answer".
const judgeBinarySystem = judgeBase +
	"Your VERY FIRST line must be exactly 'VERDICT: ok' or 'VERDICT: retry'. " + judgeGrading

// contestSystem is the second, independent review used when the user disputes
// a 'retry' verdict — explicitly generous about acceptable variants.
const contestSystem = styleRules + " You are a careful Japanese tutor doing a SECOND, independent review. Reply in Russian. " +
	"The user disputes an earlier 'incorrect' verdict on their answer. Re-examine it from scratch. " +
	"Accept the answer if ANY reasonable native speaker would consider it correct: casual or polite forms, " +
	"a different but natural word order, an omitted subject, an alternative particle where both are natural, " +
	"minor typos, romaji-vs-kana. " +
	"Your VERY FIRST line must be exactly 'VERDICT: ok' or 'VERDICT: retry'. Keep 'retry' ONLY for a definite " +
	"grammatical or meaning error. Then a blank line, then 1-3 lines: if ok — say the answer is fine and show its " +
	"natural form with kanji(чтение) and romaji; if retry — name the exact error and show the correct version with romaji."

// verdict is the judge's classification of a user message.
type verdict int

const (
	verdictRetry    verdict = iota // a genuine attempt with a real mistake (also the fallback for unparseable replies)
	verdictOK                      // essentially correct
	verdictOffTrack                // not an attempt at the task at all — never scored, never lapses SRS
)

// offtrackAllowed decides whether the judge may answer "this is not an attempt".
// When the task expects Japanese and the user wrote Japanese or Latin letters,
// it IS an attempt: allowing offtrack there made the model discard correct
// romaji answers ("buruuberi" for ブルーベリー) as gibberish. Where a valid
// answer and a stray lookup word look alike — Russian during the translate
// stage — the classification stays available, which is what it was added for.
func offtrackAllowed(expectJapanese bool, text string) bool {
	if !expectJapanese {
		return true
	}
	return !containsJapanese(text) && !containsLatin(text)
}

// judgeAnswer verdicts a user answer, picking the prompt that fits what the
// task expects.
func judgeAnswer(clients *Clients, task, answer string, expectJapanese bool) (verdict, string) {
	if offtrackAllowed(expectJapanese, answer) {
		return judgeWith(clients, judgeSystem, task)
	}
	return judgeWith(clients, judgeBinarySystem, task)
}

func judgeWith(clients *Clients, system, task string) (verdict, string) {
	resp, err := claudeOne(clients, system, task)
	if err != nil {
		return verdictRetry, "Ошибка проверки, попробуй ещё раз."
	}
	return parseVerdict(resp)
}

// parseVerdict splits the model reply into its first-line verdict and the
// feedback below it. Anything unrecognised counts as retry — the safe default
// that keeps the user in the current stage.
func parseVerdict(resp string) (verdict, string) {
	first, rest := resp, ""
	if i := strings.IndexByte(resp, '\n'); i >= 0 {
		first, rest = resp[:i], strings.TrimSpace(resp[i+1:])
	}
	switch strings.ToLower(strings.TrimSpace(first)) {
	case "verdict: ok":
		return verdictOK, rest
	case "verdict: offtrack":
		return verdictOffTrack, rest
	}
	return verdictRetry, rest
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
	v, fb := judgeAnswer(clients, "How do you say the word «"+word+"» in Japanese? User's answer: "+message.Text,
		message.Text, true)
	deleteMsg(bot, chatID, think)

	if v == verdictOffTrack {
		// not an answer at all — don't lapse the word, just ask again; «Оспорить»
		// is offered in case the classification itself was wrong
		storage.SetLastAnswer(db, userID, message.Text)
		sendKb(bot, chatID, fmt.Sprintf("Это не похоже на ответ. Как будет «%s» по-японски? Или нажми «Закончить».", word),
			reminderContestKeyboard(st.ReminderID))
		return
	}
	storage.ClearMode(db, userID)

	session := st.TaskText == srsSessionFlag
	if v == verdictOK {
		next := step + 1
		storage.ScheduleReminder(db, userID, word, next, time.Now().Add(intervalFor(next)))
		send(bot, chatID, "Правильно!\n"+fb+"\n\nНапомню это слово ещё попозже.")
		continueSrsSession(bot, db, chatID, userID, session)
	} else {
		back := lapseStep(step)
		storage.ScheduleReminder(db, userID, word, back, time.Now().Add(intervalFor(back)))
		storage.SetLastAnswer(db, userID, message.Text)
		// stay on this word: «Оспорить» needs the answer, and moving on would
		// bury a possibly wrong verdict
		sendKb(bot, chatID, fb+"\n\nНичего страшного — напомню это слово снова скоро.",
			reminderContestKeyboard(st.ReminderID))
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

// ---------- SRS rounds ----------

const (
	// srsSessionMin is the number of due words from which the bot announces the
	// whole batch once instead of trickling words out one at a time.
	srsSessionMin = 3
	// srsSessionFlag lives in user_state.task_text (unused during reminders) and
	// marks "we are working through a batch", so the next word follows straight
	// after an answer instead of waiting for the scheduler's throttle.
	srsSessionFlag = "session"
	modeSrsOffer   = "srs_offer" // announcement sent, waiting for «Начать»
)

// pluralRu picks the Russian plural form: 1 слово, 2 слова, 5 слов.
func pluralRu(n int, one, few, many string) string {
	n %= 100
	if n >= 11 && n <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	}
	return many
}

// StartReminderRound is the scheduler's entry point for one user. With several
// words due it announces the batch and waits for «Начать»; with one or two it
// just asks the first word, as before.
func StartReminderRound(bot *tgbotapi.BotAPI, db *sql.DB, userID int) {
	storage.SetLastReminderAt(db, userID, time.Now())
	n, err := storage.CountDueReminders(db, userID)
	if err != nil || n == 0 {
		return
	}
	if n >= srsSessionMin {
		storage.SetState(db, userID, modeSrsOffer, "", "", 0)
		sendKb(bot, int64(userID), fmt.Sprintf("Сегодня повторяем %d %s.\nНачнём, как будешь готов.",
			n, pluralRu(n, "слово", "слова", "слов")), srsOfferKeyboard())
		return
	}
	sendNextDueReminder(bot, db, userID, false)
}

// sendNextDueReminder asks the next due word, reporting how many are left when
// working through a batch. Returns false when nothing is due any more.
func sendNextDueReminder(bot *tgbotapi.BotAPI, db *sql.DB, userID int, session bool) bool {
	r, err := storage.NextDueReminder(db, userID)
	if err != nil {
		return false
	}
	storage.MarkReminderSent(db, r.ID)
	storage.SetLastReminderAt(db, userID, time.Now())
	flag := ""
	if session {
		flag = srsSessionFlag
	}
	storage.SetState(db, userID, "reminder", r.Word, flag, r.ID)

	text := fmt.Sprintf("Повторение!\nКак будет «%s» по-японски? Напиши свой вариант.", r.Word)
	if session {
		if left, err := storage.CountDueReminders(db, userID); err == nil && left > 0 {
			text += fmt.Sprintf("\n\nПосле этого останется ещё %d.", left)
		}
	}
	sendKb(bot, int64(userID), text, reminderKeyboard())
	return true
}

// continueSrsSession moves on to the next word of a batch, or closes it out.
func continueSrsSession(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int, session bool) {
	if !session {
		return
	}
	if !sendNextDueReminder(bot, db, userID, true) {
		storage.ClearMode(db, userID)
		send(bot, chatID, "На сегодня повторения закончились. Молодец!")
	}
}

// ---------- callbacks ----------

func HandleCallbackQuery(bot *tgbotapi.BotAPI, clients *Clients, callbackQuery *tgbotapi.CallbackQuery, db *sql.DB) {
	data := callbackQuery.Data
	userID := int(callbackQuery.From.ID)
	chatID := callbackQuery.Message.Chat.ID
	answerCallback(bot, callbackQuery)
	storage.EnsureUser(db, userID)

	// Buttons that lead to a model or TTS call count against the daily budget.
	apiCall := data == "memorize" || data == "ask_practice" || data == "practice_yes" || data == "round_yes" ||
		data == "dont_remember" || data == "audio" || strings.HasPrefix(data, "audplay#") ||
		strings.HasPrefix(data, "contest#") || strings.HasPrefix(data, "vocab_play#")
	if apiCall && !capOK(bot, clients, db, chatID, userID) {
		return
	}

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
		translation, verified := translateWord(clients, db, word)
		deleteMsg(bot, chatID, think)
		askSaveConfirmation(bot, db, chatID, userID, word, translation, verified)

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
		if e, err := storage.FindVocab(db, userID, word); err == nil && japaneseHeadword(e.Translation) != "" {
			sendKb(bot, chatID, msg, playKeyboard(e.ID))
		} else {
			send(bot, chatID, msg)
		}

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

	case strings.HasPrefix(data, "vocab_audio#"): // expand into one 🔊 per word on this page
		off, _ := strconv.Atoi(strings.TrimPrefix(data, "vocab_audio#"))
		if _, entries, err := storage.GetVocabPage(db, userID, off, vocabPageSize); err == nil && len(entries) > 0 {
			bot.Send(tgbotapi.NewEditMessageReplyMarkup(chatID, callbackQuery.Message.MessageID, vocabAudioKeyboard(entries, off)))
		}

	case strings.HasPrefix(data, "vocab_back#"): // collapse back to the page keyboard
		off, _ := strconv.Atoi(strings.TrimPrefix(data, "vocab_back#"))
		if total, entries, err := storage.GetVocabPage(db, userID, off, vocabPageSize); err == nil {
			bot.Send(tgbotapi.NewEditMessageReplyMarkup(chatID, callbackQuery.Message.MessageID, vocabPageKeyboard(off, off+len(entries) < total)))
		}

	case strings.HasPrefix(data, "vocab_play#"):
		if id, err := strconv.Atoi(strings.TrimPrefix(data, "vocab_play#")); err == nil {
			playVocabWord(bot, clients, db, chatID, userID, id)
		}

	case data == "contest#practice":
		handleContestPractice(bot, clients, db, chatID, userID)

	case strings.HasPrefix(data, "contest#rem#"):
		if id, err := strconv.Atoi(strings.TrimPrefix(data, "contest#rem#")); err == nil {
			handleContestReminder(bot, clients, db, chatID, userID, id)
		}

	case data == "dont_remember":
		st := storage.GetState(db, userID)
		word := st.Word
		if w, _, err := storage.GetReminder(db, st.ReminderID); err == nil && w != "" {
			word = w
		}
		storage.ClearMode(db, userID) // drops the session flag — st keeps a copy
		if word == "" {
			send(bot, chatID, "Окей.")
			return
		}
		think := sendThinking(bot, chatID)
		ans, _ := claudeOne(clients, styleRules+" Reply in Russian. Show how the word «"+word+"» is in Japanese: kanji(чтение) (romaji) — перевод, plus one short example.", word)
		deleteMsg(bot, chatID, think)
		storage.ScheduleReminder(db, userID, word, 1, time.Now().Add(intervalFor(1)))
		send(bot, chatID, "Ничего страшного, вот как это:\n\n"+ans+"\n\nНапомню это слово снова скоро.")
		continueSrsSession(bot, db, chatID, userID, st.TaskText == srsSessionFlag)

	case data == "srs_start":
		if !sendNextDueReminder(bot, db, userID, true) {
			storage.ClearMode(db, userID)
			send(bot, chatID, "Повторять пока нечего — напомню, когда придёт время.")
		}

	case data == "srs_later":
		storage.ClearMode(db, userID)
		send(bot, chatID, "Хорошо, напомню попозже.")

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

// audioNumberKeyboard — numbered 🔊 buttons for the sentences in a lookup message.
func audioNumberKeyboard(n int) tgbotapi.InlineKeyboardMarkup {
	return numberedAudioKeyboard(n, func(i int) string { return fmt.Sprintf("audplay#%d", i) }, "audback")
}

// numberedAudioKeyboard lays out n "🔊 i" buttons four per row plus a back button.
func numberedAudioKeyboard(n int, playData func(i int) string, backData string) tgbotapi.InlineKeyboardMarkup {
	keyboard := tgbotapi.NewInlineKeyboardMarkup()
	row := tgbotapi.NewInlineKeyboardRow()
	for i := 1; i <= n; i++ {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("🔊 %d", i), playData(i)))
		if i%4 == 0 || i == n {
			keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
			row = tgbotapi.NewInlineKeyboardRow()
		}
	}
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("← Назад", backData),
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
		// graceful degradation: say so instead of silently sending nothing
		log.Printf("TTS error: %v", err)
		send(bot, int64(userID), "Озвучка сейчас недоступна, попробуй чуть позже.")
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
