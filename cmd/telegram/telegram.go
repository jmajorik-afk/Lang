package telegram

import (
	"context"
	"database/sql"
	"fmt"
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

	token := os.Getenv("TELEGRAM_TOKEN")
	tgbotapi.SetLogger(maskedLogger{token: token})
	tgbot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Fatal(err)
	}

	_, err = tgbot.Request(tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "start", Description: "Начать"},
		tgbotapi.BotCommand{Command: "practice", Description: "Тренировка по выученным словам"},
		tgbotapi.BotCommand{Command: "ask", Description: "Вопрос по грамматике"},
		tgbotapi.BotCommand{Command: "vocab", Description: "Твой словарь"},
		tgbotapi.BotCommand{Command: "stats", Description: "Статистика и слабые места"},
		tgbotapi.BotCommand{Command: "speech_speed", Description: "Скорость озвучки"},
		tgbotapi.BotCommand{Command: "healthz", Description: "Проверка работы"},
	))
	if err != nil {
		log.Fatal("Error setting commands:", err)
	}

	clients := &bot.Clients{
		Claude:   claude_api.NewClient(os.Getenv("ANTHROPIC_API_KEY")),
		OpenAI:   openai.NewClient(os.Getenv("OPENAI_API_TOKEN")),
		DailyCap: envInt("DAILY_API_CAP", 300),
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
	if n, err := storage.NormalizeReminderTimes(db); err != nil {
		log.Println("normalizing reminder times:", err)
	} else if n > 0 {
		log.Printf("normalized %d reminder timestamps to UTC", n)
	}

	allowedUsers := parseAllowedUsers(os.Getenv("ALLOWED_TELEGRAM_USER_IDS"))

	scheduleBackups(db)
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

// maskedLogger keeps the bot token out of the journal: telegram-bot-api logs
// the full request URL on network errors, and that URL embeds the token.
type maskedLogger struct{ token string }

func (m maskedLogger) mask(s string) string {
	if m.token == "" {
		return s
	}
	return strings.ReplaceAll(s, m.token, "<token>")
}

func (m maskedLogger) Println(v ...interface{}) {
	log.Println(m.mask(strings.TrimRight(fmt.Sprintln(v...), "\n")))
}

func (m maskedLogger) Printf(format string, v ...interface{}) {
	log.Println(m.mask(fmt.Sprintf(format, v...)))
}

// envInt reads an integer env var, falling back to def when unset or invalid.
func envInt(name string, def int) int {
	s := strings.TrimSpace(os.Getenv(name))
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		log.Printf("invalid %s=%q, using default %d", name, s, def)
		return def
	}
	return n
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

// scheduleBackups snapshots the database right away and then once a day,
// keeping the 14 newest snapshots in ./backups. Losing the SRS state would
// wipe the whole learning history, so this runs unconditionally.
func scheduleBackups(db *sql.DB) {
	backup := func() {
		if path, err := storage.BackupDB(db, "backups", 14); err != nil {
			log.Println("DB backup failed:", err)
		} else {
			log.Println("DB backup written:", path)
		}
	}
	backup()
	ticker := time.NewTicker(24 * time.Hour)
	go func() {
		for range ticker.C {
			backup()
		}
	}()
}

// scheduleReminders checks once a minute for due reminders and starts at most
// one round per user, throttled to one per 30 minutes, never interrupting an
// active practice or another pending reminder. A round either announces the
// whole batch (several words due) or asks a single word — see
// bot.StartReminderRound.
func scheduleReminders(db *sql.DB, tgbot *tgbotapi.BotAPI) {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for range ticker.C {
			due, err := storage.GetDueReminders(db)
			if err != nil {
				log.Println("Error fetching reminders:", err)
				continue
			}
			started := map[int]bool{}
			for _, r := range due {
				if started[r.UserID] {
					continue
				}
				if last, ok := storage.GetLastReminderAt(db, r.UserID); ok && time.Since(last) < bot.ReminderThrottle {
					continue
				}
				if storage.GetState(db, r.UserID).Mode != "" {
					continue // user is mid-practice or already answering a reminder
				}
				bot.StartReminderRound(tgbot, db, r.UserID)
				started[r.UserID] = true
			}
		}
	}()
}
