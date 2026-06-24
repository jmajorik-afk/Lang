package storage

import (
	"database/sql"
	"time"
)

// --- Users ---

// EnsureUser creates the user row (default speed) if it doesn't exist yet.
func EnsureUser(db *sql.DB, userID int) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO users (id, speech_speed) VALUES (?, 1.0)`, userID)
	return err
}

func GetUserSpeechSpeed(db *sql.DB, userID int) float64 {
	var s float64
	if err := db.QueryRow(`SELECT speech_speed FROM users WHERE id=?`, userID).Scan(&s); err != nil || s <= 0 {
		return 1.0
	}
	return s
}

func UpdateUserSpeechSpeed(db *sql.DB, userID int, speed float64) error {
	_, err := db.Exec(`UPDATE users SET speech_speed=? WHERE id=?`, speed, userID)
	return err
}

// --- Interaction state ---

type State struct {
	Mode       string
	Word       string
	TaskText   string
	ReminderID int
}

func GetState(db *sql.DB, userID int) State {
	var s State
	err := db.QueryRow(`SELECT mode, word, task_text, reminder_id FROM user_state WHERE user_id=?`, userID).
		Scan(&s.Mode, &s.Word, &s.TaskText, &s.ReminderID)
	if err != nil {
		return State{}
	}
	return s
}

func upsertState(db *sql.DB, userID int, mode, word, task string, reminderID int) error {
	_, err := db.Exec(`
		INSERT INTO user_state (user_id, mode, word, task_text, reminder_id)
		VALUES (?,?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET
			mode=excluded.mode, word=excluded.word,
			task_text=excluded.task_text, reminder_id=excluded.reminder_id
	`, userID, mode, word, task, reminderID)
	return err
}

// SetCurrentWord records the last looked-up word and resets the interaction mode.
func SetCurrentWord(db *sql.DB, userID int, word string) error {
	return upsertState(db, userID, "", word, "", 0)
}

func SetState(db *sql.DB, userID int, mode, word, task string, reminderID int) error {
	return upsertState(db, userID, mode, word, task, reminderID)
}

// ClearMode ends any active practice/reminder but keeps the current word.
func ClearMode(db *sql.DB, userID int) error {
	_, err := db.Exec(`UPDATE user_state SET mode='', task_text='', reminder_id=0 WHERE user_id=?`, userID)
	return err
}

func GetLastReminderAt(db *sql.DB, userID int) (time.Time, bool) {
	var t sql.NullTime
	if err := db.QueryRow(`SELECT last_reminder_at FROM user_state WHERE user_id=?`, userID).Scan(&t); err != nil || !t.Valid {
		return time.Time{}, false
	}
	return t.Time, true
}

func SetLastReminderAt(db *sql.DB, userID int, at time.Time) error {
	_, err := db.Exec(`
		INSERT INTO user_state (user_id, last_reminder_at) VALUES (?, ?)
		ON CONFLICT(user_id) DO UPDATE SET last_reminder_at=excluded.last_reminder_at
	`, userID, at)
	return err
}

// --- Vocabulary ---

func SaveVocab(db *sql.DB, userID int, word string) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO vocab (user_id, word) VALUES (?, ?)`, userID, word)
	return err
}

func GetVocabList(db *sql.DB, userID int) ([]string, error) {
	rows, err := db.Query(`SELECT word FROM vocab WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var words []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		words = append(words, w)
	}
	return words, nil
}

// GetRandomVocabWord returns a random saved word, optionally excluding one.
func GetRandomVocabWord(db *sql.DB, userID int, exclude string) (string, error) {
	var w string
	err := db.QueryRow(
		`SELECT word FROM vocab WHERE user_id=? AND word<>? ORDER BY RANDOM() LIMIT 1`,
		userID, exclude,
	).Scan(&w)
	if err == sql.ErrNoRows && exclude != "" {
		// only one word in vocab — fall back to it
		err = db.QueryRow(`SELECT word FROM vocab WHERE user_id=? ORDER BY RANDOM() LIMIT 1`, userID).Scan(&w)
	}
	if err != nil {
		return "", err
	}
	return w, nil
}

// --- Reminders ---

type DueReminder struct {
	ID     int
	UserID int
	Word   string
	Step   int
}

func ScheduleReminder(db *sql.DB, userID int, word string, step int, sendAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO reminders (user_id, word, step, send_at, sent) VALUES (?,?,?,?,0)`,
		userID, word, step, sendAt,
	)
	return err
}

func HasPendingReminder(db *sql.DB, userID int, word string) bool {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM reminders WHERE user_id=? AND word=? AND sent=0`, userID, word).Scan(&n)
	return n > 0
}

func GetDueReminders(db *sql.DB) ([]DueReminder, error) {
	rows, err := db.Query(
		`SELECT id, user_id, word, step FROM reminders WHERE sent=0 AND send_at<=? ORDER BY send_at ASC`,
		time.Now(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueReminder
	for rows.Next() {
		var r DueReminder
		if err := rows.Scan(&r.ID, &r.UserID, &r.Word, &r.Step); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func MarkReminderSent(db *sql.DB, id int) error {
	_, err := db.Exec(`UPDATE reminders SET sent=1 WHERE id=?`, id)
	return err
}

func GetReminder(db *sql.DB, id int) (word string, step int, err error) {
	err = db.QueryRow(`SELECT word, step FROM reminders WHERE id=?`, id).Scan(&word, &step)
	return
}

// --- Conversation history ---

type ConversationTurn struct {
	UserMessage string
	BotResponse string
}

func SaveConversationTurn(db *sql.DB, userID int, userMessage, botResponse string) error {
	_, err := db.Exec(
		`INSERT INTO conversations (user_id, user_message, bot_response) VALUES (?,?,?)`,
		userID, userMessage, botResponse,
	)
	return err
}

func GetConversationHistory(db *sql.DB, userID int, n int) ([]ConversationTurn, error) {
	rows, err := db.Query(`
		SELECT user_message, bot_response FROM (
			SELECT user_message, bot_response, created_at
			FROM conversations WHERE user_id = ?
			ORDER BY created_at DESC LIMIT ?
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
