package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	LastAnswer string // the user's most recent judged answer, kept so it can be contested
}

func GetState(db *sql.DB, userID int) State {
	var s State
	err := db.QueryRow(`SELECT mode, word, task_text, reminder_id, last_answer FROM user_state WHERE user_id=?`, userID).
		Scan(&s.Mode, &s.Word, &s.TaskText, &s.ReminderID, &s.LastAnswer)
	if err != nil {
		return State{}
	}
	return s
}

// SetLastAnswer remembers the answer that was just judged (for «Оспорить»).
func SetLastAnswer(db *sql.DB, userID int, answer string) error {
	_, err := db.Exec(`
		INSERT INTO user_state (user_id, last_answer) VALUES (?, ?)
		ON CONFLICT(user_id) DO UPDATE SET last_answer=excluded.last_answer
	`, userID, answer)
	return err
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

// VocabEntry is one saved word together with its Japanese translation/reading.
type VocabEntry struct {
	ID          int
	Word        string
	Translation string // e.g. "植物(しょくぶつ) (shokubutsu)"; may be "" for legacy rows
}

// SaveVocab stores the word with its translation. If the word already exists,
// its translation is refreshed.
func SaveVocab(db *sql.DB, userID int, word, translation string) error {
	_, err := db.Exec(`
		INSERT INTO vocab (user_id, word, translation) VALUES (?, ?, ?)
		ON CONFLICT(user_id, word) DO UPDATE SET translation=excluded.translation
	`, userID, word, translation)
	return err
}

// SetVocabTranslation backfills the translation for an already-saved word.
func SetVocabTranslation(db *sql.DB, userID int, word, translation string) error {
	_, err := db.Exec(`UPDATE vocab SET translation=? WHERE user_id=? AND word=?`, translation, userID, word)
	return err
}

// GetVocabPage returns the total number of saved words plus one page of them,
// newest first (id breaks ties for words saved within the same second).
func GetVocabPage(db *sql.DB, userID, offset, limit int) (int, []VocabEntry, error) {
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM vocab WHERE user_id=?`, userID).Scan(&total); err != nil {
		return 0, nil, err
	}
	rows, err := db.Query(
		`SELECT id, word, translation FROM vocab WHERE user_id=?
		 ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		userID, limit, offset,
	)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	entries, err := scanVocab(rows)
	return total, entries, err
}

// SearchVocab returns saved words containing the substring q (words are stored
// lowercase, so pass q lowercased), newest first, capped at limit.
func SearchVocab(db *sql.DB, userID int, q string, limit int) ([]VocabEntry, error) {
	rows, err := db.Query(
		`SELECT id, word, translation FROM vocab WHERE user_id=? AND instr(word, ?) > 0
		 ORDER BY created_at DESC, id DESC LIMIT ?`,
		userID, q, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanVocab(rows)
}

func scanVocab(rows *sql.Rows) ([]VocabEntry, error) {
	var entries []VocabEntry
	for rows.Next() {
		var e VocabEntry
		if err := rows.Scan(&e.ID, &e.Word, &e.Translation); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// FindVocab returns the user's saved entry for a word.
func FindVocab(db *sql.DB, userID int, word string) (VocabEntry, error) {
	var e VocabEntry
	err := db.QueryRow(`SELECT id, word, translation FROM vocab WHERE user_id=? AND word=?`, userID, word).
		Scan(&e.ID, &e.Word, &e.Translation)
	return e, err
}

// GetVocabEntry returns one saved entry by id, scoped to the user.
func GetVocabEntry(db *sql.DB, userID, id int) (VocabEntry, error) {
	var e VocabEntry
	err := db.QueryRow(`SELECT id, word, translation FROM vocab WHERE user_id=? AND id=?`, userID, id).
		Scan(&e.ID, &e.Word, &e.Translation)
	return e, err
}

// GetWeakVocabWord picks the word the user knows least well: the one whose
// latest SRS step is lowest (words never practiced count as step 0, i.e.
// weakest). Random among ties, optionally excluding one word so consecutive
// rounds don't repeat.
func GetWeakVocabWord(db *sql.DB, userID int, exclude string) (string, error) {
	const q = `
		SELECT v.word
		FROM vocab v
		LEFT JOIN (
			SELECT word, step FROM reminders
			WHERE user_id = ?1
			  AND id IN (SELECT MAX(id) FROM reminders WHERE user_id = ?1 GROUP BY word)
		) s ON s.word = v.word
		WHERE v.user_id = ?1 AND v.word <> ?2
		ORDER BY COALESCE(s.step, 0) ASC, RANDOM()
		LIMIT 1`
	var w string
	err := db.QueryRow(q, userID, exclude).Scan(&w)
	if err == sql.ErrNoRows && exclude != "" {
		// only the excluded word exists — fall back to it
		err = db.QueryRow(q, userID, "").Scan(&w)
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

// DeletePendingReminders drops unsent reminders for a word (used when a
// contested verdict is overturned and the lapse it scheduled must be undone).
func DeletePendingReminders(db *sql.DB, userID int, word string) error {
	_, err := db.Exec(`DELETE FROM reminders WHERE user_id=? AND word=? AND sent=0`, userID, word)
	return err
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

// --- API budget ---

// BumpDailyUsage counts one user-initiated API interaction for the given day
// (YYYY-MM-DD) and returns the new total.
func BumpDailyUsage(db *sql.DB, userID int, day string) (int, error) {
	if _, err := db.Exec(`
		INSERT INTO api_usage (user_id, day, calls) VALUES (?, ?, 1)
		ON CONFLICT(user_id, day) DO UPDATE SET calls = calls + 1
	`, userID, day); err != nil {
		return 0, err
	}
	var n int
	err := db.QueryRow(`SELECT calls FROM api_usage WHERE user_id=? AND day=?`, userID, day).Scan(&n)
	return n, err
}

// --- Translation cache ---
// A word's Japanese translation doesn't depend on the user, so it is cached
// globally: saving or backfilling the same word twice never pays for Claude twice.

func GetCachedTranslation(db *sql.DB, word string) (translation string, verified, ok bool) {
	var v int
	if err := db.QueryRow(`SELECT translation, verified FROM translation_cache WHERE word=?`, word).
		Scan(&translation, &v); err != nil {
		return "", false, false
	}
	return translation, v == 1, true
}

func SetCachedTranslation(db *sql.DB, word, translation string, verified bool) error {
	v := 0
	if verified {
		v = 1
	}
	_, err := db.Exec(`
		INSERT INTO translation_cache (word, translation, verified) VALUES (?, ?, ?)
		ON CONFLICT(word) DO UPDATE SET translation=excluded.translation, verified=excluded.verified
	`, word, translation, v)
	return err
}

// --- Backups ---

// BackupDB writes a consistent snapshot of the live database into dir using
// VACUUM INTO (safe while the bot is running) and prunes old snapshots so at
// most keep newest files remain. Returns the path of the new snapshot.
func BackupDB(db *sql.DB, dir string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "languagebot-"+time.Now().Format("20060102-150405")+".db")
	// path is generated above — never user input — so inlining it is safe;
	// VACUUM INTO does not support bound parameters.
	if _, err := db.Exec("VACUUM INTO '" + path + "'"); err != nil {
		return "", err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return path, nil // backup itself succeeded; pruning is best-effort
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "languagebot-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // timestamped names sort chronologically
	for len(names) > keep {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
	return path, nil
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
