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

// Clients bundles both API clients: Claude for text, OpenAI for TTS.
type Clients struct {
	Claude anthropic.Client
	OpenAI *openai.Client
}

func HandleCommand(ctx context.Context, bot *tgbotapi.BotAPI, message *tgbotapi.Message, db *sql.DB, clients *Clients) error {
	log.Printf("%d [%s] %s", message.From.ID, message.From.UserName, message.Text)
	response := ""
	switch message.Command() {
	case "healthz":
		response = "OK"
	case "start":
		if err := sendLanguageSelection(bot, message.Chat.ID); err != nil {
			log.Printf("Error sending language selection: %v\n", err)
			return err
		}
	case "speech_speed":
		if err := sendSpeechSpeedSelection(bot, message.Chat.ID); err != nil {
			log.Printf("Error sending speech speed selection: %v\n", err)
			return err
		}
	case "examples":
		if err := handleExamplesCommand(bot, message, db, clients); err != nil {
			log.Printf("Error handling examples command: %v\n", err)
			return err
		}
		response = "I will respond with examples of the word or phrase usage."
	case "translation":
		if err := handleTranslationCommand(bot, message, db, clients); err != nil {
			log.Printf("Error handling translation command: %v\n", err)
			return err
		}
		response = "I will respond with translations."
	case "pronunciation":
		if err := handlePronounciationCommand(bot, message, db, clients); err != nil {
			log.Printf("Error handling pronounciation command: %v\n", err)
			return err
		}
	case "inflection":
		if err := handleInflectionCommand(bot, message, db, clients); err != nil {
			log.Printf("Error handling inflection command: %v\n", err)
			return err
		}
		response = "I will respond with inflection (if applicable) for the provided word."
	}

	if response != "" {
		msg := tgbotapi.NewMessage(message.Chat.ID, response)
		if _, err := bot.Send(msg); err != nil {
			log.Printf("Error sending response: %v\n", err)
			return err
		}
	}
	return nil
}

func parseExamplesByNumber(message string) []string {
	lines := strings.Split(message, "\n")
	var examples []string
	re := regexp.MustCompile(`[0-9]+\. `)
	for _, line := range lines {
		if re.MatchString(line) {
			line = strings.Split(line, ". ")[1]
			examples = append(examples, line)
		}
	}
	return examples
}

func handlePronounciationCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message, db *sql.DB, clients *Clients) error {
	userID := int(message.From.ID)
	sendLastRequestAudio(db, userID, 0, message.Text, clients, bot)
	return nil
}

func handleInflectionCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message, db *sql.DB, clients *Clients) error {
	return storage.UpdateUserHelpType(db, int(message.From.ID), "inflection")
}

func handleExamplesCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message, db *sql.DB, clients *Clients) error {
	return storage.UpdateUserHelpType(db, int(message.From.ID), "examples")
}

func handleTranslationCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message, db *sql.DB, clients *Clients) error {
	return storage.UpdateUserHelpType(db, int(message.From.ID), "translation")
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

	if strings.HasPrefix(data, "action:") {
		action := strings.TrimPrefix(data, "action:")
		userID := int(callbackQuery.From.ID)
		chatID := callbackQuery.Message.Chat.ID

		lastQuery, err := storage.GetLastUserQuery(db, userID)
		if err != nil || lastQuery == nil {
			bot.Send(tgbotapi.NewMessage(chatID, "Не могу найти последнее слово. Напиши что-нибудь сначала."))
			return
		}

		var prompt string
		switch action {
		case "examples":
			prompt = lastQuery.Word
			storage.UpdateUserHelpType(db, userID, "examples")
		case "practice":
			prompt = fmt.Sprintf("Дай мне практическое задание для слова «%s». Одно простое предложение на русском, которое нужно перевести на японский. Укажи новые слова с читалкой. Не давай ответ.", lastQuery.Word)
		case "explain":
			prompt = fmt.Sprintf("Объясни слово «%s» ещё раз, по-другому — другие примеры, другой угол.", lastQuery.Word)
		}

		if action == "examples" {
			response, err := ProcessQuery("examples", lastQuery.Language, lastQuery.Word, db, userID, clients)
			if err != nil {
				log.Printf("Error processing examples: %v\n", err)
				return
			}
			msg := tgbotapi.NewMessage(chatID, response)
			msg.ReplyMarkup = postResponseKeyboard()
			bot.Send(msg)
		} else {
			ctx := context.Background()
			req := claude_api.ClaudeRequest{
				SystemPrompt: fmt.Sprintf("You are a friendly Japanese tutor. No emojis. The user is learning Japanese. Respond in Russian. No formal language."),
				UserMessage:  prompt,
			}
			response, err := claude_api.GetClaudeResponse(ctx, &clients.Claude, req)
			if err != nil {
				log.Printf("Error getting response: %v\n", err)
				return
			}
			msg := tgbotapi.NewMessage(chatID, response)
			if action == "practice" {
				msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("Сдаюсь, покажи ответ", "action:show_answer"),
					),
				)
			} else {
				msg.ReplyMarkup = postResponseKeyboard()
			}
			bot.Send(msg)
		}
		return
	}

	if strings.HasPrefix(data, "action:show_answer") {
		userID := int(callbackQuery.From.ID)
		chatID := callbackQuery.Message.Chat.ID
		lastQuery, err := storage.GetLastUserQuery(db, userID)
		if err != nil || lastQuery == nil {
			return
		}
		ctx := context.Background()
		req := claude_api.ClaudeRequest{
			SystemPrompt: "You are a friendly Japanese tutor. No emojis. Respond in Russian.",
			UserMessage:  fmt.Sprintf("Покажи правильный перевод последнего задания со словом «%s» с полным разбором.", lastQuery.Word),
		}
		response, err := claude_api.GetClaudeResponse(ctx, &clients.Claude, req)
		if err != nil {
			return
		}
		msg := tgbotapi.NewMessage(chatID, response)
		msg.ReplyMarkup = postResponseKeyboard()
		bot.Send(msg)
		return
	}

	if strings.HasPrefix(data, "language:") {
		language := strings.Split(data, ":")[1]
		updateLanguagePreference(bot, callbackQuery, db, language)
	}

	if strings.HasPrefix(data, "pronunciation:") {
		exampleNumber, err := strconv.Atoi(strings.Split(data, ":")[1])
		if err != nil {
			log.Printf("Error parsing example number: %v\n", err)
			return
		}
		msg := tgbotapi.NewEditMessageText(callbackQuery.Message.Chat.ID,
			callbackQuery.Message.MessageID,
			fmt.Sprintf("You picked number %d. The pronunciation will be sent shortly. "+
				"If it doesn't appear, use /pronunciation and try again.", exampleNumber))
		if _, err = bot.Send(msg); err != nil {
			log.Printf("Error sending confirmation message: %v\n", err)
		}
		userID := int(callbackQuery.From.ID)
		if sendLastRequestAudio(db, userID, exampleNumber, callbackQuery.Message.Text, clients, bot) {
			log.Printf("Error sending last request audio")
		}
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

func sendLastRequestAudio(db *sql.DB, userId int, exampleNumber int, message string, clients *Clients, bot *tgbotapi.BotAPI) bool {
	lastQuery, err := storage.GetLastUserQuery(db, userId)
	if err != nil {
		log.Printf("Error getting last query: %v\n", err)
		return true
	}
	lastResponse, err := storage.GetCachedResponseByWordLangAndType(db, lastQuery.Language, lastQuery.Type, lastQuery.Word)
	if err != nil {
		log.Printf("Error getting cached response: %v\n", err)
		return true
	}

	if lastQuery.Type == "examples" {
		examples := parseExamplesByNumber(lastResponse)
		if exampleNumber == 0 && len(examples) > 0 {
			if err := sendExamplesSelection(bot, int64(userId), len(examples)); err != nil {
				log.Printf("Error sending examples selection: %v\n", err)
				return true
			}
		} else {
			pronunciationString := lastResponse
			if len(examples) >= exampleNumber && exampleNumber > 0 {
				pronunciationString = examples[exampleNumber-1]
			}
			if err := sendAudioMessage(clients, db, pronunciationString, userId, bot); err != nil {
				log.Printf("Error sending audio message: %v\n", err)
				return true
			}
		}
	} else if lastQuery.Type == "translation" {
		lines := strings.Split(lastResponse, "\n")
		if len(lines) > 0 {
			if err := sendAudioMessage(clients, db, lines[0], userId, bot); err != nil {
				log.Printf("Error sending audio message: %v\n", err)
				return true
			}
		}
	}
	return false
}

func sendLanguageSelection(bot *tgbotapi.BotAPI, chatID int64) error {
	msg := tgbotapi.NewMessage(chatID, "Please choose a language you want help learning:")
	msg.ReplyMarkup = languageInlineKeyboard()
	_, err := bot.Send(msg)
	return err
}

func sendSpeechSpeedSelection(bot *tgbotapi.BotAPI, chatID int64) error {
	msg := tgbotapi.NewMessage(chatID, "Please choose a speech speed:")
	msg.ReplyMarkup = speechSpeedInlineKeyboard()
	_, err := bot.Send(msg)
	return err
}

func sendExamplesSelection(bot *tgbotapi.BotAPI, chatID int64, total int) error {
	msg := tgbotapi.NewMessage(chatID, "Please choose an example:")
	msg.ReplyMarkup = examplesInlineKeyboard(total)
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

func postResponseKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Примеры", "action:examples"),
			tgbotapi.NewInlineKeyboardButtonData("Попробую сам", "action:practice"),
			tgbotapi.NewInlineKeyboardButtonData("Объясни ещё раз", "action:explain"),
		),
	)
}

func languageInlineKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Japanese", "language:Japanese"),
			tgbotapi.NewInlineKeyboardButtonData("Spanish", "language:Spanish"),
			tgbotapi.NewInlineKeyboardButtonData("French", "language:French"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("German", "language:German"),
			tgbotapi.NewInlineKeyboardButtonData("Dutch", "language:Dutch"),
			tgbotapi.NewInlineKeyboardButtonData("Estonian", "language:Estonian"),
		),
	)
}

func examplesInlineKeyboard(total int) tgbotapi.InlineKeyboardMarkup {
	keyboard := tgbotapi.NewInlineKeyboardMarkup()
	row := tgbotapi.NewInlineKeyboardRow()
	for i := 1; i <= total; i++ {
		if i%3 == 0 {
			keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
			row = tgbotapi.NewInlineKeyboardRow()
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d", i), fmt.Sprintf("pronunciation:%d", i)))
		if i == total {
			keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
		}
	}
	return keyboard
}

func updateLanguagePreference(bot *tgbotapi.BotAPI, callbackQuery *tgbotapi.CallbackQuery, db *sql.DB, language string) {
	userID := int(callbackQuery.From.ID)
	if err := storage.UpdateUserLanguage(db, userID, language); err != nil {
		log.Printf("Error updating language preference: %v\n", err)
		return
	}
	text := fmt.Sprintf(
		"Great, you picked %s! Type a word or phrase to get examples, or use /translation for translations. Enjoy!",
		language,
	)
	msg := tgbotapi.NewEditMessageText(callbackQuery.Message.Chat.ID, callbackQuery.Message.MessageID, text)
	if _, err := bot.Send(msg); err != nil {
		log.Printf("Error sending confirmation message: %v\n", err)
	}
}

type GptTemplateData struct {
	Language    string
	MessageText string
}

func HandleMessage(ctx context.Context, bot *tgbotapi.BotAPI, message *tgbotapi.Message, clients *Clients, db *sql.DB) {
	userID := int(message.From.ID)
	language, err := storage.GetUserLanguage(db, userID)
	if err != nil {
		log.Printf("Error getting user language: %v\n", err)
		return
	}

	helpType, err := GetUserHelpType(db, userID)
	if err != nil {
		return
	}

	thinkMsgResponse, shouldReturn := sendThinkingMessage(message, bot)
	if shouldReturn {
		return
	}
	defer deleteThinkingMessage(message, thinkMsgResponse, bot)

	gptresponse, err := ProcessQuery(helpType, language, message.Text, db, userID, clients)
	if err != nil {
		log.Printf("Error processing query: %v\n", err)
		return
	}

	msg := tgbotapi.NewMessage(message.Chat.ID, gptresponse)
	msg.ReplyMarkup = postResponseKeyboard()
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

func ProcessQuery(helpType string, language string, message string, db *sql.DB, userID int, clients *Clients) (string, error) {
	cfg := config.NewConfig()
	if message == "" {
		return "", errors.New("message is empty")
	}

	log.Printf("Checking cache: language=%s, type=%s, word=%s\n", language, helpType, message)
	cachedResponse, err := storage.GetCachedResponseByWordLangAndType(db, language, helpType, message)
	if err != nil {
		log.Printf("Error getting cached response: %v\n", err)
		return "", err
	}
	if cachedResponse != "" {
		log.Printf("Found cached response")
		if _, err := storage.StoreQuery(db, userID, helpType, language, message); err != nil {
			log.Printf("Error storing query: %v\n", err)
		}
		return cachedResponse, nil
	}

	var gpt *config.GptRequestType
	switch helpType {
	case "examples":
		gpt = cfg.GptTemplateWordUsageExamples
	case "translation":
		gpt = cfg.GptTemplateWordTranslation
	case "inflection":
		gpt = cfg.GptTemplateInflection
	default:
		return "", fmt.Errorf("invalid help type: %s", helpType)
	}

	data := GptTemplateData{Language: language, MessageText: message}
	var systemPrompt strings.Builder
	if err = gpt.PromptTemplate.Execute(&systemPrompt, data); err != nil {
		log.Printf("Error executing template: %v\n", err)
		return "", err
	}

	queryID, err := storage.StoreQuery(db, userID, helpType, language, message)
	if err != nil {
		log.Printf("Error storing query: %v\n", err)
	}

	// schedule spaced-repetition reminders (fire-and-forget)
	go func() {
		if err := storage.ScheduleReminders(db, userID, message, language, helpType); err != nil {
			log.Printf("Error scheduling reminders: %v\n", err)
		}
	}()

	var fewShot []claude_api.ChatMessage
	if tunings, ok := cfg.GptPromptTunings[language]; ok {
		if tuning, ok := tunings[helpType]; ok {
			for _, m := range tuning.Messages {
				fewShot = append(fewShot, claude_api.ChatMessage{Role: m.Role, Content: m.Content})
			}
		}
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
		return "", err
	}

	if err = storage.CacheResponse(db, queryID, response); err != nil {
		log.Printf("Error caching response: %v\n", err)
	}
	return response, nil
}

func GetUserHelpType(db *sql.DB, userID int) (string, error) {
	helpType, err := storage.GetUserHelpType(db, userID)
	if err != nil {
		log.Printf("Error getting user help_type: %v\n", err)
		return "", err
	}
	if helpType == "" {
		helpType = "translation"
		if err = storage.UpdateUserHelpType(db, userID, helpType); err != nil {
			log.Printf("Error updating user help_type: %v\n", err)
			return "", err
		}
	}
	return helpType, nil
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
func SendReminderMessage(bot *tgbotapi.BotAPI, userID int, word, language, helpType string, step int) {
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
	msg := tgbotapi.NewMessage(int64(userID), text)
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
