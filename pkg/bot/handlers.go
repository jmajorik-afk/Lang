package bot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"language-learning-bot/pkg/config"
	claude_api "language-learning-bot/pkg/claude"
	openai_api "language-learning-bot/pkg/openai"
	storage "language-learning-bot/pkg/storage"

	"github.com/anthropics/anthropic-sdk-go"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	openai "github.com/sashabaranov/go-openai"
)

// historyTurns is how many recent (user, bot) exchanges are replayed to Claude as
// context. Short on purpose — see the comment at the injection site in ProcessQuery.
const historyTurns = 6

// Clients bundles both API clients: Claude for text, OpenAI for TTS.
type Clients struct {
	Claude anthropic.Client
	OpenAI *openai.Client
}

// mdMsg creates a Telegram message with Markdown parsing enabled.
func mdMsg(chatID int64, text string) tgbotapi.MessageConfig {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeMarkdown
	return msg
}

func HandleCommand(ctx context.Context, bot *tgbotapi.BotAPI, message *tgbotapi.Message, db *sql.DB, clients *Clients) error {
	log.Printf("%d [%s] %s", message.From.ID, message.From.UserName, message.Text)
	response := ""
	switch message.Command() {
	case "healthz":
		response = "OK"
	case "start":
		if err := storage.EnsureUser(db, int(message.From.ID)); err != nil {
			log.Printf("Error ensuring user: %v\n", err)
		}
		response = "Привет! Пиши японское или русское слово — переведу и разберу. Кнопки под ответом помогут с примерами, практикой и произношением."
	case "speech_speed":
		if err := sendSpeechSpeedSelection(bot, message.Chat.ID); err != nil {
			log.Printf("Error sending speech speed selection: %v\n", err)
			return err
		}
	case "vocab":
		if err := handleVocabCommand(bot, message, db); err != nil {
			log.Printf("Error handling vocab command: %v\n", err)
			return err
		}
	}

	if response != "" {
		msg := mdMsg(message.Chat.ID, response)
		if _, err := bot.Send(msg); err != nil {
			log.Printf("Error sending response: %v\n", err)
			return err
		}
	}
	return nil
}

func handleVocabCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message, db *sql.DB) error {
	return sendVocabList(bot, int(message.From.ID), message.Chat.ID, db)
}

func sendVocabList(bot *tgbotapi.BotAPI, userID int, chatID int64, db *sql.DB) error {
	vocab, err := storage.GetUserVocab(db, userID)
	if err != nil {
		return err
	}
	if len(vocab) == 0 {
		msg := mdMsg(chatID, "Ты ещё ничего не спрашивал. Напиши любое слово чтобы начать.")
		_, err = bot.Send(msg)
		return err
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Слова которые ты изучал* (%d):\n\n", len(vocab)))
	for i, q := range vocab {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, q.Word))
	}
	msg := mdMsg(chatID, sb.String())
	_, err = bot.Send(msg)
	return err
}

func sendAudioMessage(clients *Clients, db *sql.DB, firstLine string, userid int, bot *tgbotapi.BotAPI) error {
	userSpeechSpeed, err := storage.GetUserSpeechSpeed(db, userid)
	if err != nil {
		log.Println("Failed to get user speech speed: ", err)
		userSpeechSpeed = 1.0
	}

	audioBytes, err := openai_api.GetTTSResponse(context.Background(), clients.OpenAI, userSpeechSpeed, firstLine)
	if err != nil {
		log.Printf("Error getting TTS response: %v\n", err)
		return err
	}

	audio := tgbotapi.FileBytes{Name: fmt.Sprintf("%s.mp3", firstLine), Bytes: audioBytes}
	audioMsg := tgbotapi.NewVoice(int64(userid), audio)
	if _, err = bot.Send(audioMsg); err != nil {
		log.Printf("Error sending audio message: %v\n", err)
		return err
	}
	return nil
}

func HandleCallbackQuery(bot *tgbotapi.BotAPI, clients *Clients, callbackQuery *tgbotapi.CallbackQuery, db *sql.DB) {
	data := callbackQuery.Data
	userID := int(callbackQuery.From.ID)
	chatID := callbackQuery.Message.Chat.ID

	// 🔊 pressed — extract Japanese from this message and speak it
	if strings.HasPrefix(data, "aud#") {
		code := strings.TrimPrefix(data, "aud#")
		sentences := extractJapaneseSentences(callbackQuery.Message.Text)
		if len(sentences) == 0 {
			return
		}
		if len(sentences) == 1 {
			sendAudioMessage(clients, db, sentences[0], userID, bot)
			return
		}
		// multiple sentences — swap keyboard to numbered picks (with Back)
		edit := tgbotapi.NewEditMessageReplyMarkup(chatID, callbackQuery.Message.MessageID, audioNumberKeyboard(len(sentences), code))
		bot.Send(edit)
		return
	}

	// audplay#N — speak the Nth Japanese sentence of this message
	if strings.HasPrefix(data, "audplay#") {
		n, err := strconv.Atoi(strings.TrimPrefix(data, "audplay#"))
		if err != nil {
			return
		}
		sentences := extractJapaneseSentences(callbackQuery.Message.Text)
		if n >= 1 && n <= len(sentences) {
			sendAudioMessage(clients, db, sentences[n-1], userID, bot)
		}
		return
	}

	// audback#CODE — restore the original keyboard for this message
	if strings.HasPrefix(data, "audback#") {
		code := strings.TrimPrefix(data, "audback#")
		edit := tgbotapi.NewEditMessageReplyMarkup(chatID, callbackQuery.Message.MessageID, keyboardForKind(codeKind(code)))
		bot.Send(edit)
		return
	}

	if strings.HasPrefix(data, "action:") {
		action := strings.TrimPrefix(data, "action:")

		lastQuery, err := storage.GetLastUserQuery(db, userID)
		if err != nil || lastQuery == nil {
			bot.Send(mdMsg(chatID, "Не могу найти последнее слово. Напиши что-нибудь сначала."))
			return
		}

		// Immediate, non-LLM actions
		switch action {
		case "know":
			if err := storage.MarkWordLearned(db, userID, lastQuery.Word); err != nil {
				log.Printf("Error marking word learned: %v\n", err)
			}
			bot.Send(mdMsg(chatID, fmt.Sprintf("Окей, «%s» — больше не напомню.", lastQuery.Word)))
			return
		case "remind":
			bot.Send(mdMsg(chatID, fmt.Sprintf("Напомню про «%s» по расписанию.", lastQuery.Word)))
			return
		}

		// Examples go through the full templated pipeline — one-off, does not change mode
		if action == "examples" {
			response, kind, err := ProcessQuery("examples", lastQuery.Word, db, userID, clients)
			if err != nil {
				log.Printf("Error processing examples: %v\n", err)
				return
			}
			msg := mdMsg(chatID, response)
			msg.ReplyMarkup = keyboardForKind(kind)
			bot.Send(msg)
			return
		}

		// Free-form LLM actions
		const tutorSystem = "You are a friendly Japanese tutor. No emojis. Respond in Russian. No formal language. Every kanji with hiragana in brackets, full romaji in parentheses on the same line, romaji in Latin letters only."
		var prompt string
		var kb tgbotapi.InlineKeyboardMarkup
		switch action {
		case "practice":
			prompt = fmt.Sprintf("Дай практическое задание для слова «%s». Одно простое предложение на русском для перевода на японский. Новые слова с читалкой. Не давай ответ.", lastQuery.Word)
			kb = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Сдаюсь, покажи ответ", "action:show_answer"),
			))
		case "explain":
			prompt = fmt.Sprintf("Объясни «%s» ещё раз, по-другому — другой угол, другие примеры.", lastQuery.Word)
			kb = keyboardForKind("грамматика")
		case "show_answer":
			prompt = fmt.Sprintf("Покажи правильный перевод последнего задания со словом «%s» с полным разбором.", lastQuery.Word)
			kb = keyboardForKind("грамматика")
		default:
			return
		}

		response, err := claude_api.GetClaudeResponse(context.Background(), &clients.Claude, claude_api.ClaudeRequest{
			SystemPrompt: tutorSystem,
			UserMessage:  prompt,
		})
		if err != nil {
			log.Printf("Error getting response: %v\n", err)
			return
		}
		msg := mdMsg(chatID, response)
		msg.ReplyMarkup = kb
		bot.Send(msg)
		return
	}

	if strings.HasPrefix(data, "speech_speed:") {
		speechSpeed, err := strconv.ParseFloat(strings.Split(data, ":")[1], 64)
		if err != nil {
			log.Printf("Error parsing speech speed: %v\n", err)
			return
		}
		speedValues := getSpeechSpeedValues()
		if speechSpeedText, ok := speedValues[speechSpeed]; ok {
			msg := tgbotapi.NewEditMessageText(callbackQuery.Message.Chat.ID,
				callbackQuery.Message.MessageID,
				fmt.Sprintf("You picked %s speech speed.", speechSpeedText))
			if _, err = bot.Send(msg); err != nil {
				log.Printf("Error sending confirmation message: %v\n", err)
			}
			if err = storage.UpdateUserSpeechSpeed(db, int(callbackQuery.From.ID), speechSpeed); err != nil {
				log.Printf("Error updating user speech speed: %v\n", err)
			}
		}
	}
}

func sendSpeechSpeedSelection(bot *tgbotapi.BotAPI, chatID int64) error {
	msg := tgbotapi.NewMessage(chatID, "Please choose a speech speed:")
	msg.ReplyMarkup = speechSpeedInlineKeyboard()
	_, err := bot.Send(msg)
	return err
}

func getSpeechSpeedValues() map[float64]string {
	return map[float64]string{
		0.5: "Slow",
		0.7: "Normal",
		1.0: "Fast",
	}
}

func speechSpeedInlineKeyboard() tgbotapi.InlineKeyboardMarkup {
	keyboard := tgbotapi.NewInlineKeyboardMarkup()
	row := tgbotapi.NewInlineKeyboardRow()

	keys := []float64{0.5, 0.7, 1.0}
	sort.Float64s(keys)
	values := getSpeechSpeedValues()
	for _, k := range keys {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(values[k], fmt.Sprintf("speech_speed:%.1f", k)))
	}
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
	return keyboard
}

// kindCode/codeKind map the kind to a compact code carried in audio callback data
// (so the "back" button can rebuild the original keyboard).
func kindCode(kind string) string {
	switch kind {
	case "грамматика":
		return "g"
	case "практика":
		return "p"
	default:
		return "w"
	}
}

func codeKind(code string) string {
	switch code {
	case "g":
		return "грамматика"
	case "p":
		return "практика"
	default:
		return "слово"
	}
}

// keyboardForKind picks buttons based on the response type Claude classified.
func keyboardForKind(kind string) tgbotapi.InlineKeyboardMarkup {
	audio := tgbotapi.NewInlineKeyboardButtonData("🔊", "aud#"+kindCode(kind))
	switch kind {
	case "слово":
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Примеры", "action:examples"),
				tgbotapi.NewInlineKeyboardButtonData("Попробую сам", "action:practice"),
				audio,
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Выучить", "action:remind"),
				tgbotapi.NewInlineKeyboardButtonData("Уже знаю", "action:know"),
			),
		)
	case "практика":
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Попробую сам", "action:practice"),
				audio,
			),
		)
	default: // грамматика
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Объясни ещё раз", "action:explain"),
				tgbotapi.NewInlineKeyboardButtonData("Попробую сам", "action:practice"),
				audio,
			),
		)
	}
}

// audioNumberKeyboard builds 1..n audio buttons plus a Back button that restores
// the original keyboard (rebuilt from the carried kind code).
func audioNumberKeyboard(n int, code string) tgbotapi.InlineKeyboardMarkup {
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
		tgbotapi.NewInlineKeyboardButtonData("← Назад", "audback#"+code),
	))
	return keyboard
}

var parenGroupRe = regexp.MustCompile(`[(（][^)）]*[)）]`)

// extractJapaneseSentences pulls clean Japanese (kanji+kana, no readings, no romaji,
// no Russian) out of a bot message, one entry per line that contains Japanese.
func extractJapaneseSentences(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		// drop the Russian translation / label after an em-dash
		if idx := strings.Index(line, "—"); idx >= 0 {
			line = line[:idx]
		}
		// drop all parenthetical groups (kana readings and romaji)
		line = parenGroupRe.ReplaceAllString(line, "")
		line = strings.TrimSpace(line)
		if containsJapanese(line) {
			out = append(out, line)
		}
	}
	return out
}

func containsJapanese(s string) bool {
	for _, r := range s {
		// Hiragana + Katakana (0x3040–0x30FF) or CJK kanji (0x4E00–0x9FFF)
		if (r >= 0x3040 && r <= 0x30FF) || (r >= 0x4E00 && r <= 0x9FFF) {
			return true
		}
	}
	return false
}

func HandleMessage(ctx context.Context, bot *tgbotapi.BotAPI, message *tgbotapi.Message, clients *Clients, db *sql.DB) {
	userID := int(message.From.ID)
	if err := storage.EnsureUser(db, userID); err != nil {
		log.Printf("Error ensuring user: %v\n", err)
	}

	// Intercept vocab-related questions before sending to Claude
	if isVocabRequest(message.Text) {
		sendVocabList(bot, userID, message.Chat.ID, db)
		return
	}

	// Intercept meta-comments — don't translate corrections or objections
	if isMetaComment(message.Text) {
		msg := mdMsg(message.Chat.ID, "понял. напиши /vocab чтобы посмотреть что реально есть в базе.")
		bot.Send(msg)
		return
	}

	thinkMsgResponse, shouldReturn := sendThinkingMessage(message, bot)
	if shouldReturn {
		return
	}
	defer deleteThinkingMessage(message, thinkMsgResponse, bot)

	// Typed text is always a translation/lookup. Examples are a one-off button action.
	gptresponse, kind, err := ProcessQuery("translation", message.Text, db, userID, clients)
	if err != nil {
		log.Printf("Error processing query: %v\n", err)
		return
	}

	if err = storage.SaveConversationTurn(db, userID, message.Text, gptresponse); err != nil {
		log.Printf("Error saving conversation turn: %v\n", err)
	}

	msg := mdMsg(message.Chat.ID, gptresponse)
	msg.ReplyMarkup = keyboardForKind(kind)
	if _, err = bot.Send(msg); err != nil {
		log.Printf("Error sending response: %v\n", err)
	}
}

func deleteThinkingMessage(message *tgbotapi.Message, thinkMsgResponse tgbotapi.Message, bot *tgbotapi.BotAPI) {
	deleteMsg := tgbotapi.NewDeleteMessage(message.Chat.ID, thinkMsgResponse.MessageID)
	if _, err := bot.Request(deleteMsg); err != nil {
		log.Printf("Error deleting thinking message: %v\n", err)
	}
}

func sendThinkingMessage(message *tgbotapi.Message, bot *tgbotapi.BotAPI) (tgbotapi.Message, bool) {
	thinkMsg := tgbotapi.NewMessage(message.Chat.ID, "Thinking...")
	thinkMsgResponse, err := bot.Send(thinkMsg)
	if err != nil {
		log.Printf("Error sending thinking message: %v\n", err)
		return tgbotapi.Message{}, true
	}
	return thinkMsgResponse, false
}

// ProcessQuery returns the cleaned response, the content kind (слово/грамматика/практика
// parsed from Claude's classification tag), and an error. mode is "translation" or "examples".
func ProcessQuery(mode string, message string, db *sql.DB, userID int, clients *Clients) (string, string, error) {
	cfg := config.NewConfig()
	if message == "" {
		return "", "", errors.New("message is empty")
	}

	var gpt *config.GptRequestType
	switch mode {
	case "examples":
		gpt = cfg.GptTemplateWordUsageExamples
	case "translation":
		gpt = cfg.GptTemplateWordTranslation
	default:
		return "", "", fmt.Errorf("invalid mode: %s", mode)
	}

	var systemPrompt strings.Builder
	if err := gpt.PromptTemplate.Execute(&systemPrompt, nil); err != nil {
		log.Printf("Error executing template: %v\n", err)
		return "", "", err
	}

	// Tell the model which words the user already knows, so it stops presenting
	// them as "new vocabulary" in examples and practice tasks.
	if known, _ := storage.GetUserVocab(db, userID); len(known) > 0 {
		var words []string
		for i, q := range known {
			if i >= 40 {
				break
			}
			words = append(words, q.Word)
		}
		systemPrompt.WriteString("\n\nWORDS THE USER ALREADY KNOWS (never present these as new): " + strings.Join(words, ", "))
	}

	// Word lookups are forced deterministically: the model keeps wanting to add
	// examples and mis-classifies, so when the input clearly asks for one word we
	// hard-constrain the output AND force the "слово" keyboard regardless of its tag.
	isWordLookup := mode == "translation" && looksLikeWordLookup(message)
	if isWordLookup {
		systemPrompt.WriteString("\n\nTHIS REQUEST IS A SINGLE-WORD LOOKUP. Output ONLY one line: word(reading) — meaning (romaji in Latin). Absolutely no example sentences, no grammar notes, no \"пара примеров\".")
	}

	// Record the bare word, not the lookup phrase: "что значит окно" → "окно".
	vocabWord := normalizeWord(message)
	if vocabWord == "" {
		vocabWord = message
	}
	if storage.IsRealWord(vocabWord) {
		if _, err := storage.StoreQuery(db, userID, mode, "Japanese", vocabWord); err != nil {
			log.Printf("Error storing query: %v\n", err)
		}
		// Schedule reminders only for short real words, not sentences or practice attempts
		if len([]rune(vocabWord)) <= 20 {
			go func() {
				if err := storage.ScheduleReminders(db, userID, vocabWord); err != nil {
					log.Printf("Error scheduling reminders: %v\n", err)
				}
			}()
		}
	}

	var fewShot []claude_api.ChatMessage
	if tuning, ok := cfg.GptPromptTunings["Japanese"][mode]; ok {
		for _, m := range tuning.Messages {
			fewShot = append(fewShot, claude_api.ChatMessage{Role: m.Role, Content: m.Content})
		}
	}

	// Inject recent conversation history. Kept short on purpose: a long window both
	// costs tokens and feeds the model its own past mistakes to imitate. 6 exchanges
	// is enough to remember "we just discussed 妻" without poisoning style.
	history, _ := storage.GetConversationHistory(db, userID, historyTurns)
	for _, turn := range history {
		fewShot = append(fewShot,
			claude_api.ChatMessage{Role: "user", Content: turn.UserMessage},
			claude_api.ChatMessage{Role: "assistant", Content: turn.BotResponse},
		)
	}

	req := claude_api.ClaudeRequest{
		SystemPrompt: systemPrompt.String(),
		Messages:     fewShot,
		UserMessage:  message,
	}

	ctx := context.Background()
	response, err := claude_api.GetClaudeResponse(ctx, &clients.Claude, req)
	if err != nil {
		log.Printf("Error getting Claude response: %v\n", err)
		return "", "", err
	}

	cleaned, kind := extractKind(response)
	if isWordLookup {
		kind = "слово" // deterministic — don't trust the model's tag for word lookups
	}
	return cleaned, kind, nil
}

// lookupPhrases are the leading/embedded phrases that mark a single-word lookup,
// e.g. "что значит окно". Shared by looksLikeWordLookup and normalizeWord.
var lookupPhrases = []string{"что значит", "как переводится", "как будет", "как сказать", "перевод "}

// looksLikeWordLookup returns true when the user is plainly asking for a single
// word's translation, rather than a sentence or a practice attempt.
func looksLikeWordLookup(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	for _, p := range lookupPhrases {
		if strings.Contains(m, p) {
			return true
		}
	}
	// bare short input of one or two words, no sentence punctuation
	if len([]rune(m)) <= 20 && !strings.ContainsAny(m, ".!?。") && len(strings.Fields(m)) <= 2 {
		return true
	}
	return false
}

// normalizeWord strips lookup phrases so that "что значит окно" → "окно" before
// the word is recorded in vocab / scheduled for reminders.
func normalizeWord(message string) string {
	m := strings.TrimSpace(message)
	for _, p := range lookupPhrases {
		if idx := strings.Index(strings.ToLower(m), p); idx >= 0 {
			m = strings.TrimSpace(m[:idx] + m[idx+len(p):])
		}
	}
	return m
}

var kindTagRe = regexp.MustCompile(`\[\[kind:([а-яёА-ЯЁ]+)\]\]`)

// extractKind pulls Claude's hidden classification tag out of the response,
// returning the cleaned text (tag removed) and the kind. Defaults to "слово".
func extractKind(resp string) (string, string) {
	kind := "слово"
	if m := kindTagRe.FindStringSubmatch(resp); m != nil {
		kind = strings.ToLower(m[1])
		resp = strings.TrimSpace(kindTagRe.ReplaceAllString(resp, ""))
	}
	return resp, kind
}

func isVocabRequest(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	triggers := []string{
		"что мы учили", "что я учил", "что мы выучили", "что я выучил",
		"мои слова", "покажи слова", "список слов", "что изучали",
		"что я изучал", "что мы изучали", "какие слова", "что ты помнишь",
		"что мы прошли", "what did we learn", "my words", "show words", "vocab",
		"что мы знаем", "что я знаю",
	}
	for _, t := range triggers {
		if strings.Contains(text, t) {
			return true
		}
	}
	return false
}

func isMetaComment(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	triggers := []string{
		"нет мы", "нет я", "мы не учили", "я не учил", "это неправильно",
		"ты ошибся", "ты придумал", "мы такого не", "я такого не",
		"это не так", "стоп", "подожди", "погоди",
	}
	for _, t := range triggers {
		if strings.Contains(text, t) {
			return true
		}
	}
	return false
}

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

// SendReminderMessage sends a spaced-repetition reminder with different prompts per step.
func SendReminderMessage(bot *tgbotapi.BotAPI, userID int, word string, step int) {
	var text string
	switch step {
	case 1: // 3 hours — ask in Russian
		text = fmt.Sprintf("как будет «%s» по-японски?", word)
	case 2: // 1 day — give a hint
		text = fmt.Sprintf("помнишь это слово? подсказка: первый символ — %s", firstChar(word))
	case 3: // 1 week — ask in Japanese
		text = fmt.Sprintf("「%s」を使って文(ぶん)を作(つく)って", word)
	default: // 1 month — no hints
		text = word
	}
	msg := mdMsg(int64(userID), text)
	if _, err := bot.Send(msg); err != nil {
		log.Printf("Error sending reminder to user %d: %v\n", userID, err)
	}
}

func firstChar(s string) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return s
	}
	return string(runes[0]) + "..."
}
