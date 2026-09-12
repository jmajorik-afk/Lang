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
	Target     string // error bucket the current practice task was built to drill, "" if none
}

func GetState(db *sql.DB, userID int) State {
	var s State
	err := db.QueryRow(`SELECT mode, word, task_text, reminder_id, last_answer, target FROM user_state WHERE user_id=?`, userID).
		Scan(&s.Mode, &s.Word, &s.TaskText, &s.ReminderID, &s.LastAnswer, &s.Target)
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
	// attempt resets here on purpose: SetState marks the start of a new task
	// or stage, so the next judged answer is a first try again.
	_, err := db.Exec(`
		INSERT INTO user_state (user_id, mode, word, task_text, reminder_id, attempt, target)
		VALUES (?,?,?,?,?,0,'')
		ON CONFLICT(user_id) DO UPDATE SET
			mode=excluded.mode, word=excluded.word,
			task_text=excluded.task_text, reminder_id=excluded.reminder_id, attempt=0, target=''
	`, userID, mode, word, task, reminderID)
	return err
}

// SetTaskTarget records which error bucket the current practice task drills.
// Call right after SetState, which clears it.
func SetTaskTarget(db *sql.DB, userID int, target string) error {
	_, err := db.Exec(`
		INSERT INTO user_state (user_id, target) VALUES (?, ?)
		ON CONFLICT(user_id) DO UPDATE SET target=excluded.target
	`, userID, target)
	return err
}

// BumpAttempts counts one more judged attempt at the current task and returns
// the new count (1 = first try).
func BumpAttempts(db *sql.DB, userID int) (int, error) {
	if _, err := db.Exec(`
		INSERT INTO user_state (user_id, attempt) VALUES (?, 1)
		ON CONFLICT(user_id) DO UPDATE SET attempt = attempt + 1
	`, userID); err != nil {
		return 0, err
	}
	var n int
	err := db.QueryRow(`SELECT attempt FROM user_state WHERE user_id=?`, userID).Scan(&n)
	return n, err
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
	`, userID, at.UTC())
	return err
}

// --- Vocabulary ---

// VocabEntry is one saved word with its Japanese translation and, when the
// Russian word splits by context, the other senses.
type VocabEntry struct {
	ID          int
	Word        string
	Translation string // main sense, e.g. "寒(さむ)い (samui)"; may be "" for legacy rows
	// Alternatives holds the other senses, one per line, each
	// "japanese(reading) (romaji) — когда так говорят". Empty for most words.
	Alternatives string
}

// SenseLines is the translation plus every alternative sense, as separate lines.
func (e VocabEntry) SenseLines() []string {
	var out []string
	for _, l := range append([]string{e.Translation}, strings.Split(e.Alternatives, "\n")...) {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// SaveVocab stores the word with its senses. If the word already exists, they
// are refreshed.
func SaveVocab(db *sql.DB, userID int, word, translation, alternatives string) error {
	_, err := db.Exec(`
		INSERT INTO vocab (user_id, word, translation, alternatives) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, word) DO UPDATE SET
			translation=excluded.translation, alternatives=excluded.alternatives
	`, userID, word, translation, alternatives)
	return err
}

// SetVocabSenses backfills the senses of an already-saved word.
func SetVocabSenses(db *sql.DB, userID int, word, translation, alternatives string) error {
	_, err := db.Exec(`UPDATE vocab SET translation=?, alternatives=? WHERE user_id=? AND word=?`,
		translation, alternatives, userID, word)
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
		`SELECT id, word, translation, alternatives FROM vocab WHERE user_id=?
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
		`SELECT id, word, translation, alternatives FROM vocab WHERE user_id=? AND instr(word, ?) > 0
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
		if err := rows.Scan(&e.ID, &e.Word, &e.Translation, &e.Alternatives); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// FindVocab returns the user's saved entry for a word.
func FindVocab(db *sql.DB, userID int, word string) (VocabEntry, error) {
	var e VocabEntry
	err := db.QueryRow(`SELECT id, word, translation, alternatives FROM vocab WHERE user_id=? AND word=?`, userID, word).
		Scan(&e.ID, &e.Word, &e.Translation, &e.Alternatives)
	return e, err
}

// GetVocabEntry returns one saved entry by id, scoped to the user.
func GetVocabEntry(db *sql.DB, userID, id int) (VocabEntry, error) {
	var e VocabEntry
	err := db.QueryRow(`SELECT id, word, translation, alternatives FROM vocab WHERE user_id=? AND id=?`, userID, id).
		Scan(&e.ID, &e.Word, &e.Translation, &e.Alternatives)
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

// Timestamps are always written in UTC. go-sqlite3 stores time.Time as text
// carrying the process's local offset, and SQLite compares that text
// lexicographically — so a row written on a UTC+5 laptop and compared on a
// UTC+2 server fired three hours late. Every writer here calls .UTC() and every
// comparison passes a UTC value; NormalizeReminderTimes fixes rows from before.
func ScheduleReminder(db *sql.DB, userID int, word string, step int, sendAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO reminders (user_id, word, step, send_at, sent) VALUES (?,?,?,?,0)`,
		userID, word, step, sendAt.UTC(),
	)
	return err
}

// NormalizeReminderTimes rewrites timestamps stored with a non-UTC offset as
// UTC text so they compare correctly. Idempotent; run at startup. Returns the
// number of rows fixed.
func NormalizeReminderTimes(db *sql.DB) (int64, error) {
	res, err := db.Exec(`
		UPDATE reminders SET send_at = strftime('%Y-%m-%d %H:%M:%f', send_at) || '+00:00'
		WHERE send_at NOT LIKE '%+00:00' AND strftime('%Y-%m-%d %H:%M:%f', send_at) IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = db.Exec(`
		UPDATE user_state SET last_reminder_at = strftime('%Y-%m-%d %H:%M:%f', last_reminder_at) || '+00:00'
		WHERE last_reminder_at IS NOT NULL AND last_reminder_at NOT LIKE '%+00:00'
		  AND strftime('%Y-%m-%d %H:%M:%f', last_reminder_at) IS NOT NULL`)
	if err != nil {
		return n, err
	}
	m, _ := res.RowsAffected()
	return n + m, nil
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

// justAnswered excludes words answered in the last half hour. A word answered
// wrongly lapses to a 3-hour interval, which falls inside the batch horizon —
// without this it would be asked again in the same sitting, right after the
// user had just been shown the correct answer.
const justAnswered = ` AND NOT EXISTS (
		SELECT 1 FROM outcomes o WHERE o.user_id = r.user_id AND o.word = r.word
		  AND o.created_at >= datetime('now', '-30 minutes'))`

// dueWindow decides which words a batch may pull forward. A word is in scope if
// it is genuinely due now, or — only on a long interval (step 2+, a day or
// more) — if it ripens before the horizon, so a day's worth can be gathered into
// one sitting. A freshly lapsed word sits on the short 3-hour step precisely so
// the user meets it again soon but not immediately; pulling that forward would
// erase the step, so short intervals always wait for their exact time.
const dueWindow = ` AND (r.send_at <= ? OR (r.step >= 2 AND r.send_at <= ?))`

// CountDueReminders is how many words are waiting for this user as of now: those
// already due, plus long-interval words ripening before horizon that a batch may
// gather early. Pass horizon == now for "strictly due now".
func CountDueReminders(db *sql.DB, userID int, now, horizon time.Time) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM reminders r WHERE r.user_id=? AND r.sent=0`+dueWindow+justAnswered,
		userID, now.UTC(), horizon.UTC()).Scan(&n)
	return n, err
}

// NextDueReminder returns the longest-overdue word in that same scope.
func NextDueReminder(db *sql.DB, userID int, now, horizon time.Time) (DueReminder, error) {
	var r DueReminder
	err := db.QueryRow(
		`SELECT r.id, r.user_id, r.word, r.step FROM reminders r WHERE r.user_id=? AND r.sent=0`+
			dueWindow+justAnswered+` ORDER BY r.send_at ASC, r.id ASC LIMIT 1`,
		userID, now.UTC(), horizon.UTC()).
		Scan(&r.ID, &r.UserID, &r.Word, &r.Step)
	return r, err
}

func GetDueReminders(db *sql.DB) ([]DueReminder, error) {
	rows, err := db.Query(
		`SELECT id, user_id, word, step FROM reminders WHERE sent=0 AND send_at<=? ORDER BY send_at ASC`,
		time.Now().UTC(),
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

func GetCachedTranslation(db *sql.DB, word string) (translation, alternatives string, verified, ok bool) {
	var v int
	if err := db.QueryRow(`SELECT translation, alternatives, verified FROM translation_cache WHERE word=?`, word).
		Scan(&translation, &alternatives, &v); err != nil {
		return "", "", false, false
	}
	return translation, alternatives, v == 1, true
}

func SetCachedTranslation(db *sql.DB, word, translation, alternatives string, verified bool) error {
	v := 0
	if verified {
		v = 1
	}
	_, err := db.Exec(`
		INSERT INTO translation_cache (word, translation, alternatives, verified) VALUES (?, ?, ?, ?)
		ON CONFLICT(word) DO UPDATE SET translation=excluded.translation,
			alternatives=excluded.alternatives, verified=excluded.verified
	`, word, translation, alternatives, v)
	return err
}

// --- Outcomes: the practice/reminder journal ---
// Every judged answer is recorded. This is the raw material for /stats and,
// later, for aiming practice at weak grammar points and calibrating difficulty.

func LogOutcome(db *sql.DB, userID int, kind, word string, ok, firstTry bool, tag, detail, target string) error {
	_, err := db.Exec(`
		INSERT INTO outcomes (user_id, kind, word, ok, first_try, tag, detail, target)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, kind, word, b2i(ok), b2i(firstTry), tag, detail, target)
	return err
}

// FirstTryRate looks at the user's last n first attempts of a kind and returns
// how many were solved (an overturned mistake counts as solved) out of how many.
func FirstTryRate(db *sql.DB, userID int, kind string, n int) (ok, total int, err error) {
	rows, err := db.Query(`
		SELECT ok, overturned FROM outcomes WHERE user_id=? AND kind=? AND first_try=1
		ORDER BY id DESC LIMIT ?`, userID, kind, n)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var o, ov int
		if err := rows.Scan(&o, &ov); err != nil {
			return 0, 0, err
		}
		total++
		if o == 1 || ov == 1 {
			ok++
		}
	}
	return ok, total, rows.Err()
}

// WeakestBucket picks the error bucket practice should aim at. A bucket
// qualifies when it has at least min unreversed mistakes in the last 30 days
// that have not yet been paid down: every targeted task on that bucket solved
// on the first try pays one mistake off, so a weakness that has been drilled
// away stops being drilled instead of haunting the user for the whole window.
// Among qualifying buckets the largest outstanding balance wins (ties by
// name). Returns the bucket and the user's last 3 mistake descriptions in it.
func WeakestBucket(db *sql.DB, userID, min int) (string, []string, error) {
	const win = " AND created_at >= datetime('now', '-30 days')"
	mistakes, paid := map[string]int{}, map[string]int{}

	collect := func(dst map[string]int, query string) error {
		rows, err := db.Query(query, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			var n int
			if err := rows.Scan(&t, &n); err != nil {
				return err
			}
			dst[t] = n
		}
		return rows.Err()
	}
	if err := collect(mistakes, `SELECT tag, COUNT(*) FROM outcomes WHERE user_id=? AND ok=0 AND overturned=0
		AND tag<>'' AND tag<>'other'`+win+` GROUP BY tag`); err != nil {
		return "", nil, err
	}
	if err := collect(paid, `SELECT target, COUNT(*) FROM outcomes WHERE user_id=? AND target<>'' AND first_try=1
		AND (ok=1 OR overturned=1)`+win+` GROUP BY target`); err != nil {
		return "", nil, err
	}

	best, bestLeft := "", 0
	for tag, m := range mistakes {
		left := m - paid[tag]
		if m < min || left <= 0 {
			continue
		}
		if left > bestLeft || (left == bestLeft && tag < best) {
			best, bestLeft = tag, left
		}
	}
	if best == "" {
		return "", nil, nil
	}

	rows, err := db.Query(`SELECT detail FROM outcomes WHERE user_id=? AND tag=? AND ok=0 AND overturned=0
		AND detail<>''`+win+` ORDER BY id DESC LIMIT 3`, userID, best)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return "", nil, err
		}
		details = append(details, d)
	}
	return best, details, rows.Err()
}

// OverturnLastMistake marks the user's most recent unreversed mistake on a word
// as overturned — «Оспорить» won — so it no longer counts against them.
func OverturnLastMistake(db *sql.DB, userID int, word string) error {
	_, err := db.Exec(`
		UPDATE outcomes SET overturned=1 WHERE id = (
			SELECT id FROM outcomes WHERE user_id=? AND word=? AND ok=0 AND overturned=0
			ORDER BY id DESC LIMIT 1)`, userID, word)
	return err
}

type TagCount struct {
	Tag   string
	Count int
}

type WordCount struct {
	Word  string
	Count int
}

// Stats backs /stats. Windowed figures cover the last 30 days; an overturned
// mistake counts as correct everywhere.
type Stats struct {
	VocabTotal   int
	Learned      int // words whose latest SRS step is 4+ (30-day interval or longer)
	Streak       int // consecutive UTC days with a judged answer, still alive today or as of yesterday
	RemTotal     int // reminder answers in the window
	RemOK        int
	PracTasks    int         // practice tasks attempted in the window (first attempts)
	PracFirstTry int         // ...solved on the first try
	WeakTags     []TagCount  // mistakes per bucket, most frequent first (top 3)
	MissedWords  []WordCount // words with most mistakes (top 3)
}

func GetStats(db *sql.DB, userID int) (Stats, error) {
	var s Stats
	count := func(dst *int, query string) error {
		return db.QueryRow(query, userID).Scan(dst)
	}
	const win = " AND created_at >= datetime('now', '-30 days')"
	steps := []struct {
		dst   *int
		query string
	}{
		{&s.VocabTotal, `SELECT COUNT(*) FROM vocab WHERE user_id=?`},
		{&s.Learned, `SELECT COUNT(*) FROM reminders WHERE user_id=?1 AND step>=4
			AND id IN (SELECT MAX(id) FROM reminders WHERE user_id=?1 GROUP BY word)`},
		{&s.RemTotal, `SELECT COUNT(*) FROM outcomes WHERE user_id=? AND kind='reminder'` + win},
		{&s.RemOK, `SELECT COUNT(*) FROM outcomes WHERE user_id=? AND kind='reminder' AND (ok=1 OR overturned=1)` + win},
		{&s.PracTasks, `SELECT COUNT(*) FROM outcomes WHERE user_id=? AND kind<>'reminder' AND first_try=1` + win},
		{&s.PracFirstTry, `SELECT COUNT(*) FROM outcomes WHERE user_id=? AND kind<>'reminder' AND first_try=1 AND (ok=1 OR overturned=1)` + win},
	}
	for _, st := range steps {
		if err := count(st.dst, st.query); err != nil {
			return s, err
		}
	}

	rows, err := db.Query(`SELECT tag, COUNT(*) FROM outcomes
		WHERE user_id=? AND ok=0 AND overturned=0 AND tag<>'' AND tag<>'other'`+win+`
		GROUP BY tag ORDER BY 2 DESC, tag LIMIT 3`, userID)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			rows.Close()
			return s, err
		}
		s.WeakTags = append(s.WeakTags, tc)
	}
	rows.Close()

	rows, err = db.Query(`SELECT word, COUNT(*) FROM outcomes
		WHERE user_id=? AND ok=0 AND overturned=0`+win+`
		GROUP BY word ORDER BY 2 DESC, word LIMIT 3`, userID)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var wc WordCount
		if err := rows.Scan(&wc.Word, &wc.Count); err != nil {
			rows.Close()
			return s, err
		}
		s.MissedWords = append(s.MissedWords, wc)
	}
	rows.Close()

	s.Streak, err = streakDays(db, userID)
	return s, err
}

// streakDays counts consecutive UTC days with at least one judged answer,
// counting back from today. Not having practised yet today does not break a
// streak that was alive yesterday.
func streakDays(db *sql.DB, userID int) (int, error) {
	rows, err := db.Query(`SELECT DISTINCT date(created_at) FROM outcomes WHERE user_id=? ORDER BY 1 DESC`, userID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var days []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return 0, err
		}
		days = append(days, d)
	}
	if len(days) == 0 {
		return 0, nil
	}
	const layout = "2006-01-02"
	expect := time.Now().UTC()
	if days[0] != expect.Format(layout) {
		expect = expect.AddDate(0, 0, -1)
		if days[0] != expect.Format(layout) {
			return 0, nil
		}
	}
	streak := 0
	for _, d := range days {
		if d != expect.Format(layout) {
			break
		}
		streak++
		expect = expect.AddDate(0, 0, -1)
	}
	return streak, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
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
