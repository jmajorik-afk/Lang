package storage

import (
	"database/sql"
	"strings"
	"time"
)

type Reminder struct {
	ID       int
	UserID   int
	Word     string
	Language string
	HelpType string
	SendAt   time.Time
	Step     int // 1=3h, 2=1d, 3=7d, 4=30d
}

// ScheduleReminders creates spaced-repetition reminders for a word if not already scheduled.
func ScheduleReminders(db *sql.DB, userID int, word string) error {
	// Only schedule if this is the first time this word is queried by this user
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM reminders WHERE user_id=? AND word=?`,
		userID, word).Scan(&count)
	if err != nil || count > 0 {
		return err
	}

	now := time.Now()
	intervals := []time.Duration{
		3 * time.Hour,
		24 * time.Hour,
		7 * 24 * time.Hour,
		30 * 24 * time.Hour,
	}
	for step, d := range intervals {
		_, err := db.Exec(
			`INSERT INTO reminders (user_id, word, language, help_type, send_at, step) VALUES (?,?,?,?,?,?)`,
			userID, word, "Japanese", "", now.Add(d), step+1,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetDueReminders returns all unsent reminders whose send_at <= now.
func GetDueReminders(db *sql.DB) ([]Reminder, error) {
	rows, err := db.Query(
		`SELECT id, user_id, word, language, help_type, send_at, step FROM reminders WHERE sent=0 AND send_at <= ?`,
		time.Now(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reminders []Reminder
	for rows.Next() {
		var r Reminder
		if err := rows.Scan(&r.ID, &r.UserID, &r.Word, &r.Language, &r.HelpType, &r.SendAt, &r.Step); err != nil {
			return nil, err
		}
		reminders = append(reminders, r)
	}
	return reminders, nil
}

// GetUserVocab returns unique real words the user has studied (filters out meta-questions).
func GetUserVocab(db *sql.DB, userID int) ([]LastUserQuery, error) {
	rows, err := db.Query(`
		SELECT DISTINCT word, help_type, language
		FROM queries
		WHERE user_id = ?
		ORDER BY timestamp DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vocab []LastUserQuery
	seen := map[string]bool{}
	for rows.Next() {
		var q LastUserQuery
		if err := rows.Scan(&q.Word, &q.Type, &q.Language); err != nil {
			return nil, err
		}
		if !seen[q.Word] && IsRealWord(q.Word) {
			seen[q.Word] = true
			vocab = append(vocab, q)
		}
	}
	return vocab, nil
}

// IsRealWord returns true if the entry looks like a word/phrase being studied,
// not a meta-question or chat message.
func IsRealWord(w string) bool {
	// Too long — likely a full sentence question
	runes := []rune(w)
	if len(runes) > 30 {
		return false
	}
	// Contains question mark
	if strings.Contains(w, "?") {
		return false
	}
	// Starts with common Russian question words
	lower := strings.ToLower(strings.TrimSpace(w))
	metaPrefixes := []string{
		"что ", "как ", "почему ", "где ", "когда ", "а как", "а что",
		"какие", "нет ", "стоп", "погоди", "подожди", "ты написал",
		"что значит", "а кровать", "а жена",
	}
	for _, p := range metaPrefixes {
		if strings.HasPrefix(lower, p) {
			return false
		}
	}
	return true
}

type ConversationTurn struct {
	UserMessage string
	BotResponse string
}

// SaveConversationTurn stores one user↔bot exchange.
func SaveConversationTurn(db *sql.DB, userID int, userMessage, botResponse string) error {
	_, err := db.Exec(
		`INSERT INTO conversations (user_id, user_message, bot_response) VALUES (?,?,?)`,
		userID, userMessage, botResponse,
	)
	return err
}

// GetConversationHistory returns the last n turns for a user, oldest first.
func GetConversationHistory(db *sql.DB, userID int, n int) ([]ConversationTurn, error) {
	rows, err := db.Query(`
		SELECT user_message, bot_response FROM (
			SELECT user_message, bot_response, created_at
			FROM conversations
			WHERE user_id = ?
			ORDER BY created_at DESC
			LIMIT ?
		) ORDER BY created_at ASC
	`, userID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []ConversationTurn
	for rows.Next() {
		var t ConversationTurn
		if err := rows.Scan(&t.UserMessage, &t.BotResponse); err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	return turns, nil
}

// MarkReminderSent marks a reminder as sent.
func MarkReminderSent(db *sql.DB, id int) error {
	_, err := db.Exec(`UPDATE reminders SET sent=1 WHERE id=?`, id)
	return err
}

// MarkWordLearned cancels all future reminders for a word for this user.
func MarkWordLearned(db *sql.DB, userID int, word string) error {
	_, err := db.Exec(
		`UPDATE reminders SET sent=1 WHERE user_id=? AND word=? AND sent=0`,
		userID, word,
	)
	return err
}

type LastUserQuery struct {
	Word     string
	Type     string
	Language string
}

// EnsureUser creates the user row (Japanese, default speed) if it doesn't exist yet.
func EnsureUser(db *sql.DB, userID int) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO users (id, language, help_type, speech_speed) VALUES (?, 'Japanese', '', 0.0)`,
		userID,
	)
	return err
}

func UpdateUserSpeechSpeed(db *sql.DB, userID int, speech_speed float64) error {
	query := `
	UPDATE users SET speech_speed = ?
	WHERE id = ?;
	`
	_, err := db.Exec(query, speech_speed, userID)
	if err != nil {
		return err
	}
	return nil
}

func GetUserSpeechSpeed(db *sql.DB, userID int) (float64, error) {
	query := `
	SELECT speech_speed FROM users WHERE id = ?;
	`
	var speechSpeed float64
	err := db.QueryRow(query, userID).Scan(&speechSpeed)
	if err != nil {
		return 1.0, err
	}
	if speechSpeed <= 0 {
		return 1.0, nil
	}
	return speechSpeed, nil
}

func StoreQuery(db *sql.DB, userID int, helpType, language, word string) (int, error) {
	query := `
	INSERT INTO queries (user_id, help_type, language, word)
	VALUES (?, ?, ?, ?)
	RETURNING id;
	`
	var queryID int
	err := db.QueryRow(query, userID, helpType, language, word).Scan(&queryID)
	if err != nil {
		return 0, err
	}

	return queryID, nil
}

func GetLastUserQuery(db *sql.DB, userID int) (*LastUserQuery, error) {
	query := `
  SELECT q.word, q.help_type, q.language
  FROM queries q
  WHERE q.user_id = ?
  ORDER BY q.timestamp DESC
  LIMIT 1;
  `
	var lastQuery LastUserQuery
	qr := db.QueryRow(query, userID)
	if qr.Err() != nil {
		return nil, qr.Err()
	}
	qr.Scan(&lastQuery.Word, &lastQuery.Type, &lastQuery.Language)
	return &lastQuery, nil
}

