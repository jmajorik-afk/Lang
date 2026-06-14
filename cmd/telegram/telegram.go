package telegram

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"language-learning-bot/pkg/bot"
	claude_api "language-learning-bot/pkg/claude"
	"language-learning-bot/pkg/storage"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	openai "github.com/sashabaranov/go-openai"
)

func StartTelegramBot() {
	if err := godotenv.Load(); err != nil {
		log.Printf("Error loading .env file: %v\n", err)
	}

	tgbot, err := tgbotapi.NewBotAPI(os.Getenv("TELEGRAM_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}

	_, err = tgbot.Request(tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "start", Description: "Configure the preferred language"},
		tgbotapi.BotCommand{Command: "inflection", Description: "Give inflection of a given word"},
		tgbotapi.BotCommand{Command: "translation", Description: "Provide translation of a phrase or a word"},
		tgbotapi.BotCommand{Command: "examples", Description: "Provide 3-4 examples of a word or a phrase"},
		tgbotapi.BotCommand{Command: "pronunciation", Description: "Pronounce a word or a phrase"},
		tgbotapi.BotCommand{Command: "speech_speed", Description: "Set speech speed"},
		tgbotapi.BotCommand{Command: "healthz", Description: "Check service health status"},
	))
	if err != nil {
		log.Fatal("Error setting commands:", err)
	}

	claudeClient := claude_api.NewClient(os.Getenv("ANTHROPIC_API_KEY"))
	clients := &bot.Clients{
		Claude: claudeClient,
		OpenAI: openai.NewClient(os.Getenv("OPENAI_API_TOKEN")),
	}

	db, err := sql.Open("sqlite3", os.Getenv("SQLITE_PATH"))
	if err != nil {
		log.Fatal("Error opening database:", err)
	}
	defer db.Close()

	initDBSQL, err := os.ReadFile("scripts/init_db.sql")
	if err != nil {
		log.Fatal("Error reading init_db.sql:", err)
	}
	if _, err = db.Exec(string(initDBSQL)); err != nil {
		log.Fatal("Error executing init_db.sql:", err)
	}

	allowedUsers := parseAllowedUsers(os.Getenv("ALLOWED_TELEGRAM_USER_IDS"))

	scheduleQueriesRemoval(db)
	scheduleReminders(db, tgbot)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := tgbot.GetUpdatesChan(u)

	log.Println("Running...")

	for update := range updates {
		go func(update tgbotapi.Update) {
			defer func() {
				if r := recover(); r != nil {
					log.Println("Recovered panic:", r)
				}
			}()

			ctx := context.Background()

			if !bot.IsAllowedUser(update, allowedUsers) {
				if update.Message != nil {
					log.Printf("User %d is not allowed", update.Message.From.ID)
				}
				return
			}

			if update.Message != nil {
				if update.Message.IsCommand() {
					if err := bot.HandleCommand(ctx, tgbot, update.Message, db, clients); err != nil {
						log.Printf("Error handling command: %v\n", err)
					}
				} else {
					bot.HandleMessage(ctx, tgbot, update.Message, clients, db)
				}
			} else if update.CallbackQuery != nil {
				bot.HandleCallbackQuery(tgbot, clients, update.CallbackQuery, db)
			}
		}(update)
	}
}

func parseAllowedUsers(s string) []int64 {
	var users []int64
	for _, id := range strings.Split(s, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			log.Fatal("Invalid ALLOWED_TELEGRAM_USER_IDS:", err)
		}
		users = append(users, n)
	}
	return users
}

func scheduleQueriesRemoval(db *sql.DB) {
	intervalStr := os.Getenv("CACHE_CLEAN_INTERVAL_HOURS")
	if intervalStr == "" {
		intervalStr = "24"
	}
	hours, err := strconv.Atoi(intervalStr)
	if err != nil {
		log.Fatal(err)
	}
	ticker := time.NewTicker(time.Duration(hours) * time.Hour)
	go func() {
		for range ticker.C {
			if err := storage.CleanOldCachedResponses(db); err != nil {
				log.Println("Error cleaning cached responses:", err)
			}
		}
	}()
}

// scheduleReminders polls every 5 minutes and sends due spaced-repetition reminders.
func scheduleReminders(db *sql.DB, tgbot *tgbotapi.BotAPI) {
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for range ticker.C {
			reminders, err := storage.GetDueReminders(db)
			if err != nil {
				log.Println("Error fetching reminders:", err)
				continue
			}
			for _, r := range reminders {
				bot.SendReminderMessage(tgbot, db, r.UserID, r.Word, r.Language, r.HelpType)
				if err := storage.MarkReminderSent(db, r.ID); err != nil {
					log.Printf("Error marking reminder %d sent: %v\n", r.ID, err)
				}
			}
		}
	}()
}
