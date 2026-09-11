package storage

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, ddl := range []string{
		`CREATE TABLE vocab (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			word TEXT NOT NULL,
			translation TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(user_id, word)
		)`,
		`CREATE TABLE reminders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			word TEXT NOT NULL,
			step INTEGER NOT NULL DEFAULT 1,
			send_at DATETIME NOT NULL,
			sent INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE user_state (
			user_id INTEGER PRIMARY KEY,
			mode TEXT NOT NULL DEFAULT '',
			word TEXT NOT NULL DEFAULT '',
			task_text TEXT NOT NULL DEFAULT '',
			reminder_id INTEGER NOT NULL DEFAULT 0,
			last_reminder_at DATETIME,
			last_answer TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE api_usage (
			user_id INTEGER NOT NULL,
			day TEXT NOT NULL,
			calls INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (user_id, day)
		)`,
		`CREATE TABLE translation_cache (
			word TEXT PRIMARY KEY,
			translation TEXT NOT NULL,
			verified INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestGetWeakVocabWordPrefersLowestStep(t *testing.T) {
	db := testDB(t)
	const uid = 1
	for _, w := range []string{"крыша", "дерево", "игра"} {
		if err := SaveVocab(db, uid, w, ""); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	// крыша is well-known (step 4), дерево is shaky (step 2, after an earlier
	// step-3 row — the LATEST row must win), игра was never practiced (weakest).
	ScheduleReminder(db, uid, "крыша", 4, now)
	ScheduleReminder(db, uid, "дерево", 3, now)
	ScheduleReminder(db, uid, "дерево", 2, now)

	got, err := GetWeakVocabWord(db, uid, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "игра" {
		t.Errorf("weakest = %q, want «игра» (never practiced)", got)
	}

	got, err = GetWeakVocabWord(db, uid, "игра")
	if err != nil {
		t.Fatal(err)
	}
	if got != "дерево" {
		t.Errorf("weakest excluding «игра» = %q, want «дерево» (latest step 2)", got)
	}
}

func TestGetWeakVocabWordFallsBackToExcluded(t *testing.T) {
	db := testDB(t)
	const uid = 1
	if err := SaveVocab(db, uid, "крыша", ""); err != nil {
		t.Fatal(err)
	}
	got, err := GetWeakVocabWord(db, uid, "крыша")
	if err != nil {
		t.Fatal(err)
	}
	if got != "крыша" {
		t.Errorf("single-word vocab must fall back to the excluded word, got %q", got)
	}
}

func TestGetVocabPage(t *testing.T) {
	db := testDB(t)
	const uid = 1
	// 12 words; same created_at second, so id must break the tie (newest first)
	for i := 1; i <= 12; i++ {
		if err := SaveVocab(db, uid, fmt.Sprintf("слово%02d", i), ""); err != nil {
			t.Fatal(err)
		}
	}

	total, page, err := GetVocabPage(db, uid, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 12 || len(page) != 10 {
		t.Fatalf("page 1: total=%d len=%d, want 12/10", total, len(page))
	}
	if page[0].Word != "слово12" || page[9].Word != "слово03" {
		t.Errorf("page 1 order: got %s..%s, want слово12..слово03", page[0].Word, page[9].Word)
	}

	total, page, err = GetVocabPage(db, uid, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 12 || len(page) != 2 || page[0].Word != "слово02" || page[1].Word != "слово01" {
		t.Errorf("page 2: total=%d len=%d %v, want the two oldest words", total, len(page), page)
	}

	_, page, err = GetVocabPage(db, uid, 20, 10) // past the end
	if err != nil || len(page) != 0 {
		t.Errorf("past-the-end page: len=%d err=%v, want empty and no error", len(page), err)
	}
}

func TestSearchVocab(t *testing.T) {
	db := testDB(t)
	const uid = 1
	for _, w := range []string{"крыша", "дерево", "игра"} {
		if err := SaveVocab(db, uid, w, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := SearchVocab(db, uid, "рыш", 20)
	if err != nil || len(got) != 1 || got[0].Word != "крыша" {
		t.Errorf("search «рыш»: %v (err=%v), want just «крыша»", got, err)
	}
	got, _ = SearchVocab(db, uid, "е", 20)
	if len(got) != 1 || got[0].Word != "дерево" {
		t.Errorf("search «е»: %v, want just «дерево»", got)
	}
	if got, _ := SearchVocab(db, uid, "xyz", 20); len(got) != 0 {
		t.Errorf("search «xyz»: %v, want no matches", got)
	}
}

func TestSetLastAnswerRoundTrip(t *testing.T) {
	db := testDB(t)
	if err := SetLastAnswer(db, 1, "やね"); err != nil {
		t.Fatal(err)
	}
	if got := GetState(db, 1).LastAnswer; got != "やね" {
		t.Errorf("LastAnswer = %q, want やね", got)
	}
	SetLastAnswer(db, 1, "")
	if got := GetState(db, 1).LastAnswer; got != "" {
		t.Errorf("LastAnswer after clearing = %q, want empty", got)
	}
}

func TestBumpDailyUsage(t *testing.T) {
	db := testDB(t)
	for want := 1; want <= 3; want++ {
		n, err := BumpDailyUsage(db, 1, "2026-09-10")
		if err != nil || n != want {
			t.Fatalf("bump #%d: n=%d err=%v", want, n, err)
		}
	}
	if n, _ := BumpDailyUsage(db, 1, "2026-09-11"); n != 1 {
		t.Errorf("a new day must start from 1, got %d", n)
	}
	if n, _ := BumpDailyUsage(db, 2, "2026-09-10"); n != 1 {
		t.Errorf("another user must start from 1, got %d", n)
	}
}

func TestTranslationCache(t *testing.T) {
	db := testDB(t)
	if _, _, ok := GetCachedTranslation(db, "крыша"); ok {
		t.Fatal("empty cache must miss")
	}
	SetCachedTranslation(db, "крыша", "屋根(やね) (yane)", false)
	tr, verified, ok := GetCachedTranslation(db, "крыша")
	if !ok || tr != "屋根(やね) (yane)" || verified {
		t.Errorf("got (%q, verified=%v, ok=%v)", tr, verified, ok)
	}
	SetCachedTranslation(db, "крыша", "屋根(やね) (yane)", true) // later verified by Jisho
	if _, verified, _ := GetCachedTranslation(db, "крыша"); !verified {
		t.Error("cache must upgrade to verified")
	}
}

func TestDeletePendingRemindersKeepsSentHistory(t *testing.T) {
	db := testDB(t)
	now := time.Now()
	ScheduleReminder(db, 1, "крыша", 1, now)
	MarkReminderSent(db, 1)                  // answered → history
	ScheduleReminder(db, 1, "крыша", 1, now) // the lapse we want to undo
	ScheduleReminder(db, 1, "дерево", 1, now)

	if err := DeletePendingReminders(db, 1, "крыша"); err != nil {
		t.Fatal(err)
	}
	if HasPendingReminder(db, 1, "крыша") {
		t.Error("pending «крыша» reminder must be gone")
	}
	if !HasPendingReminder(db, 1, "дерево") {
		t.Error("other words' reminders must be untouched")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM reminders WHERE word='крыша'`).Scan(&n)
	if n != 1 {
		t.Errorf("sent history must survive, got %d «крыша» rows", n)
	}
}

func TestDueReminderQueue(t *testing.T) {
	db := testDB(t)
	const uid = 1
	now := time.Now()
	ScheduleReminder(db, uid, "крыша", 1, now.Add(-2*time.Hour)) // most overdue
	ScheduleReminder(db, uid, "дерево", 1, now.Add(-time.Hour))
	ScheduleReminder(db, uid, "игра", 1, now.Add(time.Hour)) // not due yet
	ScheduleReminder(db, 2, "чужое", 1, now.Add(-time.Hour)) // another user

	n, err := CountDueReminders(db, uid)
	if err != nil || n != 2 {
		t.Fatalf("CountDueReminders = %d (err=%v), want 2 — future and other users excluded", n, err)
	}

	r, err := NextDueReminder(db, uid)
	if err != nil || r.Word != "крыша" {
		t.Fatalf("NextDueReminder = %+v (err=%v), want the most overdue word", r, err)
	}

	// working through the batch: answered words drop out of the queue
	MarkReminderSent(db, r.ID)
	if n, _ := CountDueReminders(db, uid); n != 1 {
		t.Errorf("after answering one, %d left, want 1", n)
	}
	r, err = NextDueReminder(db, uid)
	if err != nil || r.Word != "дерево" {
		t.Fatalf("next word = %+v (err=%v), want дерево", r, err)
	}
	MarkReminderSent(db, r.ID)
	if _, err := NextDueReminder(db, uid); err == nil {
		t.Error("an empty queue must return an error so the session can close")
	}
}

func TestFindVocabAndGetVocabEntry(t *testing.T) {
	db := testDB(t)
	if err := SaveVocab(db, 1, "крыша", "屋根(やね) (yane)"); err != nil {
		t.Fatal(err)
	}
	e, err := FindVocab(db, 1, "крыша")
	if err != nil || e.ID == 0 || e.Translation != "屋根(やね) (yane)" {
		t.Fatalf("FindVocab = %+v, err=%v", e, err)
	}
	byID, err := GetVocabEntry(db, 1, e.ID)
	if err != nil || byID.Word != "крыша" {
		t.Errorf("GetVocabEntry = %+v, err=%v", byID, err)
	}
	if _, err := GetVocabEntry(db, 2, e.ID); err == nil {
		t.Error("an entry must only be readable by its owner")
	}
}

func TestBackupDBWritesReadableSnapshot(t *testing.T) {
	db := testDB(t)
	if err := SaveVocab(db, 1, "крыша", "屋根(やね) (yane)"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path, err := BackupDB(db, dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	var n int
	if err := snap.QueryRow(`SELECT COUNT(*) FROM vocab`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("snapshot has %d vocab rows, want 1", n)
	}
}
