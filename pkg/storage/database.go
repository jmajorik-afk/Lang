package storage

import (
	"database/sql"
	"time"
)

type Reminder struct {
	ID       int
	UserID   int
	Word     string
	Language string
	HelpType string
	SendAt   time.Time
}

// ScheduleReminders creates spaced-repetition reminders for a word if not already scheduled.
func ScheduleReminders(db *sql.DB, userID int, word, language, helpType string) error {
	// Only schedule if this is the first time this word is queried by this user
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM reminders WHERE user_id=? AND word=? AND language=?`,
		userID, word, language).Scan(&count)
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
	for _, d := range intervals {
		_, err := db.Exec(
			`INSERT INTO reminders (user_id, word, language, help_type, send_at) VALUES (?,?,?,?,?)`,
			userID, word, language, helpType, now.Add(d),
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
		`SELECT id, user_id, word, language, help_type, send_at FROM reminders WHERE sent=0 AND send_at <= ?`,
		time.Now(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reminders []Reminder
	for rows.Next() {
		var r Reminder
		if err := rows.Scan(&r.ID, &r.UserID, &r.Word, &r.Language, &r.HelpType, &r.SendAt); err != nil {
			return nil, err
		}
		reminders = append(reminders, r)
	}
	return reminders, nil
}

// MarkReminderSent marks a reminder as sent.
func MarkReminderSent(db *sql.DB, id int) error {
	_, err := db.Exec(`UPDATE reminders SET sent=1 WHERE id=?`, id)
	return err
}

type LastUserQuery struct {
	Word     string
	Type     string
	Language string
}

func UpdateUserLanguage(db *sql.DB, userID int, language string) error {
	// SQL query for upsert operation
	query := `
	INSERT INTO users (id, language, help_type, speech_speed)
	VALUES (?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		language = EXCLUDED.language,
		speech_speed = CASE WHEN EXCLUDED.speech_speed > 0 THEN EXCLUDED.speech_speed ELSE users.speech_speed END
	`
	_, err := db.Exec(query, userID, language, "", 0.0)
	if err != nil {
		return err
	}
	return nil
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

func GetUserLanguage(db *sql.DB, userID int) (string, error) {
	query := `
	SELECT language FROM users WHERE id = ?;
	`
	var language string
	err := db.QueryRow(query, userID).Scan(&language)
	if err != nil {
		return "", err
	}
	return language, nil
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

func UpdateUserHelpType(db *sql.DB, userID int, helpType string) error {
	query := `
    UPDATE users SET help_type = ?
    WHERE id = ?;
    `
	_, err := db.Exec(query, helpType, userID)
	if err != nil {
		return err
	}
	return nil
}

func GetUserHelpType(db *sql.DB, userID int) (string, error) {
	query := `
	SELECT help_type FROM users WHERE id = ?;
	`
	var helpType string
	err := db.QueryRow(query, userID).Scan(&helpType)
	if err != nil {
		return "", err
	}
	return helpType, nil
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
	// select last query from user, join with cached_responses to get type
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

func CacheResponse(db *sql.DB, query_id int, response string) error {
	query := `
  INSERT INTO cached_responses (query_id, response)
  VALUES (?, ?);
  `
	_, err := db.Exec(query, query_id, response)
	if err != nil {
		return err
	}
	return nil
}

func GetCachedResponseByWordLangAndType(db *sql.DB, language, helpType, word string) (string, error) {
	query := `
  SELECT cr.response
  FROM cached_responses cr
  JOIN queries q ON q.id = cr.query_id
  WHERE q.language = ? AND q.help_type = ? AND q.word = ? ;
  `
	var response string
	qr := db.QueryRow(query, language, helpType, word)
	err := qr.Err()
	if err != nil {
		return "", err
	}
	qr.Scan(&response)
	return response, nil
}

func CleanOldCachedResponses(db *sql.DB) error {
	query := `
        DELETE FROM cached_responses
        WHERE datetime(created_at) < datetime('now', '-24 hours');
    `
	_, err := db.Exec(query)
	if err != nil {
		return err
	}
	return nil
}
