package storage

import (
	"database/sql"
	"fmt"
	"strings"
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
			last_answer TEXT NOT NULL DEFAULT '',
			attempt INTEGER NOT NULL DEFAULT 0,
			target TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE outcomes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			word TEXT NOT NULL,
			ok INTEGER NOT NULL,
			first_try INTEGER NOT NULL DEFAULT 1,
			tag TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '',
			overturned INTEGER NOT NULL DEFAULT 0,
			target TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
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

	n, err := CountDueReminders(db, uid, now)
	if err != nil || n != 2 {
		t.Fatalf("CountDueReminders = %d (err=%v), want 2 — future and other users excluded", n, err)
	}

	r, err := NextDueReminder(db, uid, now)
	if err != nil || r.Word != "крыша" {
		t.Fatalf("NextDueReminder = %+v (err=%v), want the most overdue word", r, err)
	}

	// working through the batch: answered words drop out of the queue
	MarkReminderSent(db, r.ID)
	if n, _ := CountDueReminders(db, uid, now); n != 1 {
		t.Errorf("after answering one, %d left, want 1", n)
	}
	r, err = NextDueReminder(db, uid, now)
	if err != nil || r.Word != "дерево" {
		t.Fatalf("next word = %+v (err=%v), want дерево", r, err)
	}
	MarkReminderSent(db, r.ID)
	if _, err := NextDueReminder(db, uid, now); err == nil {
		t.Error("an empty queue must return an error so the session can close")
	}
}

func TestScheduleReminderStoresUTC(t *testing.T) {
	db := testDB(t)
	plus5 := time.FixedZone("UTC+5", 5*3600)
	ScheduleReminder(db, 1, "крыша", 1, time.Date(2026, 9, 11, 10, 0, 0, 0, plus5)) // = 05:00 UTC
	// `|| ''` yields a plain TEXT expression, so the driver hands back the raw
	// stored string instead of parsing the DATETIME column into a time.Time.
	var raw string
	db.QueryRow(`SELECT send_at || '' FROM reminders`).Scan(&raw)
	if !strings.HasPrefix(raw, "2026-09-11 05:00:00") || !strings.HasSuffix(raw, "+00:00") {
		t.Errorf("send_at stored as %q, want UTC text starting 2026-09-11 05:00:00 and ending +00:00", raw)
	}
}

// TestMixedOffsetsAndHorizon reproduces the production bug — rows written on a
// UTC+5 laptop compared as text against a UTC+2/UTC "now" fired hours late —
// and checks the startup normalization plus the "due within a window" horizon.
func TestMixedOffsetsAndHorizon(t *testing.T) {
	db := testDB(t)
	const uid = 1
	// legacy row: 10:00+05:00 is 05:00 UTC
	db.Exec(`INSERT INTO reminders (user_id, word, step, send_at, sent)
	         VALUES (?, 'мак', 1, '2026-09-11 10:00:00.000+05:00', 0)`, uid)
	at0600 := time.Date(2026, 9, 11, 6, 0, 0, 0, time.UTC)

	if n, _ := CountDueReminders(db, uid, at0600); n != 0 {
		t.Fatalf("the bug: before normalization the +05:00 row must NOT compare as due, got %d", n)
	}
	fixed, err := NormalizeReminderTimes(db)
	if err != nil || fixed != 1 {
		t.Fatalf("NormalizeReminderTimes = %d, %v; want 1 row fixed", fixed, err)
	}
	if n, _ := CountDueReminders(db, uid, at0600); n != 1 {
		t.Errorf("after normalization the 05:00 UTC row must be due at 06:00 UTC, got %d", n)
	}
	if again, _ := NormalizeReminderTimes(db); again != 0 {
		t.Errorf("normalization must be idempotent, touched %d rows on second run", again)
	}

	// horizon: a word due in 2h is part of today's batch but not "due now"
	ScheduleReminder(db, uid, "скоро", 1, at0600.Add(2*time.Hour))
	if n, _ := CountDueReminders(db, uid, at0600); n != 1 {
		t.Errorf("due-now count = %d, want 1", n)
	}
	if n, _ := CountDueReminders(db, uid, at0600.Add(12*time.Hour)); n != 2 {
		t.Errorf("12h-window count = %d, want 2", n)
	}
	if r, _ := NextDueReminder(db, uid, at0600.Add(12*time.Hour)); r.Word != "мак" {
		t.Errorf("batch must start with the earliest word, got %q", r.Word)
	}
}

func TestAttemptsResetPerTask(t *testing.T) {
	db := testDB(t)
	SetState(db, 1, "practice_compose", "крыша", "task", 0)
	n1, _ := BumpAttempts(db, 1)
	n2, _ := BumpAttempts(db, 1)
	if n1 != 1 || n2 != 2 {
		t.Errorf("attempts = %d, %d; want 1, 2", n1, n2)
	}
	SetState(db, 1, "practice_translate", "крыша", "task2", 0) // next stage = new task
	if n, _ := BumpAttempts(db, 1); n != 1 {
		t.Errorf("a new task must restart the attempt count, got %d", n)
	}
}

func TestOutcomesAndStats(t *testing.T) {
	db := testDB(t)
	const uid = 1
	SaveVocab(db, uid, "крыша", "")
	SaveVocab(db, uid, "дерево", "")
	now := time.Now()
	ScheduleReminder(db, uid, "крыша", 4, now.Add(30*24*time.Hour)) // long interval → "learned"
	ScheduleReminder(db, uid, "дерево", 2, now.Add(24*time.Hour))

	// practice: крыша failed twice on particles then passed; дерево right first try
	LogOutcome(db, uid, "compose", "крыша", false, true, "particle", "を вместо が", "")
	LogOutcome(db, uid, "compose", "крыша", false, false, "particle", "снова が", "")
	LogOutcome(db, uid, "compose", "крыша", true, false, "", "", "")
	LogOutcome(db, uid, "compose", "дерево", true, true, "", "", "")
	// reminders: one right, one wrong but overturned by «Оспорить»
	LogOutcome(db, uid, "reminder", "дерево", true, true, "", "", "")
	LogOutcome(db, uid, "reminder", "крыша", false, true, "word-choice", "x", "")
	OverturnLastMistake(db, uid, "крыша")

	s, err := GetStats(db, uid)
	if err != nil {
		t.Fatal(err)
	}
	if s.VocabTotal != 2 || s.Learned != 1 {
		t.Errorf("vocab=%d learned=%d, want 2/1", s.VocabTotal, s.Learned)
	}
	if s.Streak != 1 {
		t.Errorf("streak = %d, want 1 (everything logged today)", s.Streak)
	}
	if s.RemTotal != 2 || s.RemOK != 2 {
		t.Errorf("reminders %d/%d, want 2/2 — an overturned mistake counts as correct", s.RemOK, s.RemTotal)
	}
	if s.PracTasks != 2 || s.PracFirstTry != 1 {
		t.Errorf("practice first-try %d/%d, want 1/2", s.PracFirstTry, s.PracTasks)
	}
	if len(s.WeakTags) != 1 || s.WeakTags[0].Tag != "particle" || s.WeakTags[0].Count != 2 {
		t.Errorf("weak tags = %+v, want just particle×2 (the overturned word-choice must not count)", s.WeakTags)
	}
	if len(s.MissedWords) != 1 || s.MissedWords[0].Word != "крыша" || s.MissedWords[0].Count != 2 {
		t.Errorf("missed words = %+v, want just крыша×2", s.MissedWords)
	}
}

func TestTaskTargetLifecycle(t *testing.T) {
	db := testDB(t)
	SetState(db, 1, "practice_compose", "крыша", "task", 0)
	SetTaskTarget(db, 1, "particle")
	if got := GetState(db, 1).Target; got != "particle" {
		t.Errorf("Target = %q, want particle", got)
	}
	SetState(db, 1, "practice_translate", "крыша", "jp", 0) // new stage clears it
	if got := GetState(db, 1).Target; got != "" {
		t.Errorf("a new task must clear the target, got %q", got)
	}
}

func TestFirstTryRateWindow(t *testing.T) {
	db := testDB(t)
	const uid = 1
	seq := []struct {
		kind       string
		ok, first  bool
		overturned bool
	}{
		{"compose", false, true, false}, // oldest — outside a window of 3
		{"compose", true, true, false},
		{"compose", true, false, false}, // a retry: not a first attempt, ignored
		{"reminder", true, true, false}, // other kind, ignored
		{"compose", false, true, true},  // mistake later overturned → counts as solved
		{"compose", false, true, false},
	}
	for _, s := range seq {
		LogOutcome(db, uid, s.kind, "w", s.ok, s.first, "", "", "")
		if s.overturned {
			OverturnLastMistake(db, uid, "w")
		}
	}
	ok, total, err := FirstTryRate(db, uid, "compose", 3)
	if err != nil {
		t.Fatal(err)
	}
	// window of 3 first tries, newest first: fail, overturned(=ok), ok → 2 of 3
	if ok != 2 || total != 3 {
		t.Errorf("FirstTryRate = %d/%d, want 2/3", ok, total)
	}
}

func TestWeakestBucketThresholdAndPayDown(t *testing.T) {
	db := testDB(t)
	const uid = 1
	mistake := func(tag, detail string) { LogOutcome(db, uid, "compose", "w", false, true, tag, detail, "") }
	drilled := func(target string, ok bool) { LogOutcome(db, uid, "compose", "w", ok, true, "", "", target) }

	mistake("particle", "を вместо が")
	mistake("particle", "снова が")
	if tag, _, _ := WeakestBucket(db, uid, 3); tag != "" {
		t.Errorf("2 mistakes must not qualify, got %q", tag)
	}
	mistake("particle", "は вместо が")
	tag, details, err := WeakestBucket(db, uid, 3)
	if err != nil || tag != "particle" {
		t.Fatalf("WeakestBucket = %q (err=%v), want particle after 3 mistakes", tag, err)
	}
	if len(details) != 3 || details[0] != "は вместо が" {
		t.Errorf("details = %v, want the last 3 slips newest first", details)
	}

	// two targeted first-try successes pay two mistakes down: still one outstanding
	drilled("particle", true)
	drilled("particle", true)
	if tag, _, _ := WeakestBucket(db, uid, 3); tag != "particle" {
		t.Errorf("one mistake still outstanding, got %q", tag)
	}
	drilled("particle", false) // a failed drill pays nothing
	if tag, _, _ := WeakestBucket(db, uid, 3); tag != "particle" {
		t.Errorf("a failed drill must not pay down, got %q", tag)
	}
	drilled("particle", true)
	if tag, _, _ := WeakestBucket(db, uid, 3); tag != "" {
		t.Errorf("all mistakes paid down, drilling must stop, got %q", tag)
	}

	// a bigger outstanding balance wins; 'other' and overturned slips never count
	for i := 0; i < 4; i++ {
		mistake("verb-form", "past tense")
	}
	for i := 0; i < 5; i++ {
		mistake("other", "?")
	}
	mistake("counter", "x")
	OverturnLastMistake(db, uid, "w")
	if tag, _, _ := WeakestBucket(db, uid, 3); tag != "verb-form" {
		t.Errorf("want verb-form (4 outstanding), got %q", tag)
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
