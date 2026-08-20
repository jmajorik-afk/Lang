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
