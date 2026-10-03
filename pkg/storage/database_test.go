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
			alternatives TEXT NOT NULL DEFAULT '',
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
			alternatives TEXT NOT NULL DEFAULT '',
			verified INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	// the tables above are the pre-migration schema, as on a deployed server
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestGetWeakVocabWordPrefersLowestStep(t *testing.T) {
	db := testDB(t)
	const uid = 1
	for _, w := range []string{"крыша", "дерево", "игра"} {
		if err := SaveVocab(db, uid, w, "", ""); err != nil {
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
	if err := SaveVocab(db, uid, "крыша", "", ""); err != nil {
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
		if err := SaveVocab(db, uid, fmt.Sprintf("слово%02d", i), "", ""); err != nil {
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
		if err := SaveVocab(db, uid, w, "", ""); err != nil {
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
	if _, _, _, ok := GetCachedTranslation(db, "крыша"); ok {
		t.Fatal("empty cache must miss")
	}
	SetCachedTranslation(db, "крыша", "屋根(やね) (yane)", "", false)
	tr, _, verified, ok := GetCachedTranslation(db, "крыша")
	if !ok || tr != "屋根(やね) (yane)" || verified {
		t.Errorf("got (%q, verified=%v, ok=%v)", tr, verified, ok)
	}
	SetCachedTranslation(db, "крыша", "屋根(やね) (yane)", "", true) // later verified by Jisho
	if _, _, verified, _ := GetCachedTranslation(db, "крыша"); !verified {
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

	n, err := CountDueReminders(db, uid, now, now, -1)
	if err != nil || n != 2 {
		t.Fatalf("CountDueReminders = %d (err=%v), want 2 — future and other users excluded", n, err)
	}

	r, err := NextDueReminder(db, uid, now, now, -1)
	if err != nil || r.Word != "крыша" {
		t.Fatalf("NextDueReminder = %+v (err=%v), want the most overdue word", r, err)
	}

	// working through the batch: answered words drop out of the queue
	MarkReminderSent(db, r.ID)
	if n, _ := CountDueReminders(db, uid, now, now, -1); n != 1 {
		t.Errorf("after answering one, %d left, want 1", n)
	}
	r, err = NextDueReminder(db, uid, now, now, -1)
	if err != nil || r.Word != "дерево" {
		t.Fatalf("next word = %+v (err=%v), want дерево", r, err)
	}
	MarkReminderSent(db, r.ID)
	if _, err := NextDueReminder(db, uid, now, now, -1); err == nil {
		t.Error("an empty queue must return an error so the session can close")
	}
}

// TestBatchNeverSkipsTheShortStep — a batch may gather long-interval words a
// few hours early, but a freshly lapsed word on the 3-hour step must wait for
// its exact time: that short step is the whole point of getting it wrong.
func TestBatchNeverSkipsTheShortStep(t *testing.T) {
	db := testDB(t)
	const uid = 1
	now := time.Now()
	ScheduleReminder(db, uid, "холодно", 1, now.Add(3*time.Hour)) // the lapse — must NOT be pulled forward
	ScheduleReminder(db, uid, "дерево", 2, now.Add(2*time.Hour))  // a day-interval word ripening today
	ScheduleReminder(db, uid, "крыша", 4, now.Add(20*time.Hour))  // beyond the horizon
	ScheduleReminder(db, uid, "окно", 1, now.Add(-time.Hour))     // genuinely overdue
	horizon := now.Add(12 * time.Hour)

	n, err := CountDueReminders(db, uid, now, horizon, -1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("batch size = %d, want 2 (overdue «окно» + day-interval «дерево»); "+
			"the 3-hour lapse and the 20-hour word must stay out", n)
	}
	if r, _ := NextDueReminder(db, uid, now, horizon, -1); r.Word != "окно" {
		t.Errorf("next word = %q, want the overdue «окно»", r.Word)
	}
	// the lapsed word becomes available once its three hours are really up
	if n, _ := CountDueReminders(db, uid, now, now.Add(4*time.Hour), -1); n != 2 {
		t.Errorf("a step-1 word still must not be pulled early even with a wider horizon, got %d", n)
	}
}

// TestDueRemindersSkipJustAnswered — nothing the user answered minutes ago
// comes back in the same sitting, whatever its interval.
func TestDueRemindersSkipJustAnswered(t *testing.T) {
	db := testDB(t)
	const uid = 1
	now := time.Now()
	ScheduleReminder(db, uid, "холодно", 2, now.Add(-time.Hour)) // overdue
	ScheduleReminder(db, uid, "дерево", 2, now.Add(-time.Hour))
	horizon := now.Add(12 * time.Hour)

	if n, _ := CountDueReminders(db, uid, now, horizon, -1); n != 2 {
		t.Fatalf("both words start out due, got %d", n)
	}
	LogOutcome(db, uid, "reminder", "холодно", false, true, "word-choice", "x", "")
	if n, _ := CountDueReminders(db, uid, now, horizon, -1); n != 1 {
		t.Errorf("the just-answered word must drop out, got %d", n)
	}
	if r, _ := NextDueReminder(db, uid, now, horizon, -1); r.Word != "дерево" {
		t.Errorf("next word = %q, want дерево", r.Word)
	}
}

// TestRescheduleAbandoned — a reminder that was sent and never answered used to
// take its word out of the rotation for good, because only an answer ever
// creates the next one. Three real words («бутылка», «игра», «ребёнок») had
// vanished that way.
func TestRescheduleAbandoned(t *testing.T) {
	db := testDB(t)
	const uid = 1
	now := time.Now()
	for _, w := range []string{"бутылка", "окно", "игра", "новое"} {
		SaveVocab(db, uid, w, "", "")
	}
	// бутылка: asked at step 3 a day ago, never answered — abandoned
	ScheduleReminder(db, uid, "бутылка", 3, now.Add(-24*time.Hour))
	MarkReminderSent(db, 1)
	db.Exec(`UPDATE reminders SET sent_at=? WHERE id=1`, now.Add(-24*time.Hour).UTC())
	// окно: has a pending reminder, nothing to do
	ScheduleReminder(db, uid, "окно", 2, now.Add(24*time.Hour))
	// игра: asked minutes ago and still on screen — must be left alone
	ScheduleReminder(db, uid, "игра", 1, now.Add(-time.Minute))
	MarkReminderSent(db, 3)
	// новое: never had a reminder at all

	n, err := RescheduleAbandoned(db, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("put back %d words, want 2 (бутылка and новое)", n)
	}
	if !HasPendingReminder(db, uid, "бутылка") {
		t.Error("«бутылка» must be back in the rotation")
	}
	if !HasPendingReminder(db, uid, "новое") {
		t.Error("a word that never had a reminder must get one")
	}
	if HasPendingReminder(db, uid, "игра") {
		t.Error("a question asked minutes ago must not be duplicated")
	}
	// the restored word keeps the step it had reached
	var step int
	db.QueryRow(`SELECT step FROM reminders WHERE word='бутылка' AND sent=0`).Scan(&step)
	if step != 3 {
		t.Errorf("restored step = %d, want 3 — progress must not be lost", step)
	}
	// running again changes nothing
	if again, _ := RescheduleAbandoned(db, 6*time.Hour); again != 0 {
		t.Errorf("second run touched %d rows, want 0", again)
	}
}

// TestRescheduleAbandonedSparesTheOpenQuestion reproduces how five real words
// ended up with two pending reminders: a word that was a day overdue when the
// bot asked it looked "abandoned" by its old scheduled time the minute it went
// out, got a second reminder while the user was still answering, and the answer
// then scheduled a third — so the word was asked twice from then on.
func TestRescheduleAbandonedSparesTheOpenQuestion(t *testing.T) {
	db := testDB(t)
	const uid = 1
	now := time.Now()
	SaveVocab(db, uid, "стул", "", "")

	// стул: due a day ago, asked just now
	ScheduleReminder(db, uid, "стул", 3, now.Add(-24*time.Hour))
	MarkReminderSent(db, 1)
	if n, _ := RescheduleAbandoned(db, 6*time.Hour); n != 0 {
		t.Fatalf("a word asked a minute ago was re-queued (%d rows) — the duplicate bug", n)
	}

	// окно: the open question for days — the freeze keeps it open, it must
	// not be duplicated however long it waits
	SaveVocab(db, uid, "окно", "", "")
	ScheduleReminder(db, uid, "окно", 2, now.Add(-72*time.Hour))
	MarkReminderSent(db, 2)
	db.Exec(`UPDATE reminders SET sent_at=? WHERE id=2`, now.Add(-48*time.Hour).UTC())
	SetState(db, uid, "reminder", "окно", "", 2)
	RescheduleAbandoned(db, 6*time.Hour)
	if HasPendingReminder(db, uid, "окно") {
		t.Error("the question still waiting for an answer got a second reminder")
	}
}

func TestScheduleReminderKeepsOnePending(t *testing.T) {
	db := testDB(t)
	now := time.Now()
	ScheduleReminder(db, 1, "машина", 4, now.Add(30*24*time.Hour))
	ScheduleReminder(db, 1, "машина", 3, now.Add(7*24*time.Hour)) // the latest answer decides
	ScheduleReminder(db, 1, "стул", 2, now)

	var n, step int
	db.QueryRow(`SELECT COUNT(*), MAX(step) FROM reminders WHERE word='машина' AND sent=0`).Scan(&n, &step)
	if n != 1 || step != 3 {
		t.Errorf("«машина» has %d pending reminder(s) at step %d, want exactly 1 at step 3", n, step)
	}
	if !HasPendingReminder(db, 1, "стул") {
		t.Error("another word's reminder must be untouched")
	}
}

func TestDedupePendingReminders(t *testing.T) {
	db := testDB(t)
	// doubles as they were on the server: older row first, newer row from the
	// latest answer second
	for _, r := range []struct {
		word string
		step int
		sent int
	}{
		{"ждать", 4, 0}, {"ждать", 4, 0}, // id 1, 2
		{"машина", 3, 1}, {"машина", 4, 0}, {"машина", 3, 0}, // history + 2 pending: id 4, 5
		{"окно", 2, 0}, // single — untouched
	} {
		db.Exec(`INSERT INTO reminders (user_id, word, step, send_at, sent) VALUES (1,?,?,?,?)`,
			r.word, r.step, time.Now().UTC(), r.sent)
	}
	n, err := DedupePendingReminders(db)
	if err != nil || n != 2 {
		t.Fatalf("DedupePendingReminders removed %d (err=%v), want 2", n, err)
	}
	var ids []int
	rows, _ := db.Query(`SELECT id FROM reminders WHERE sent=0 ORDER BY id`)
	for rows.Next() {
		var id int
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if fmt.Sprint(ids) != "[2 5 6]" {
		t.Errorf("pending ids = %v, want [2 5 6] — the newest per word", ids)
	}
	var sent int
	db.QueryRow(`SELECT COUNT(*) FROM reminders WHERE sent=1`).Scan(&sent)
	if sent != 1 {
		t.Error("sent history must never be touched")
	}
	if again, _ := DedupePendingReminders(db); again != 0 {
		t.Errorf("second run removed %d, want 0", again)
	}
}

// TestDailyReviewCap — after a break the backlog is spread over several days:
// at most the cap of long-interval words a day, the most fragile first, while
// the 3-hour step is never held back.
func TestDailyReviewCap(t *testing.T) {
	db := testDB(t)
	const uid = 1
	now := time.Now()
	for i := 0; i < 20; i++ { // a week away: 20 long-interval words overdue
		ScheduleReminder(db, uid, fmt.Sprintf("слово%d", i), 2+i%4, now.Add(-time.Duration(i+1)*time.Hour))
	}
	ScheduleReminder(db, uid, "новое", 1, now.Add(-time.Minute)) // the 3-hour step
	horizon := now.Add(12 * time.Hour)

	if n, _ := CountDueReminders(db, uid, now, horizon, 15); n != 16 {
		t.Errorf("with a cap of 15, count = %d, want 16 (15 long + the 3-hour word)", n)
	}
	if n, _ := CountDueReminders(db, uid, now, horizon, -1); n != 21 {
		t.Errorf("uncapped count = %d, want 21", n)
	}

	// the 3-hour word first, then the shortest long interval
	r, _ := NextDueReminder(db, uid, now, horizon, 15)
	if r.Word != "новое" {
		t.Fatalf("first word = %q, want the 3-hour «новое»", r.Word)
	}
	MarkReminderSent(db, r.ID)
	r, _ = NextDueReminder(db, uid, now, horizon, 15)
	if r.Step != 2 {
		t.Errorf("next word is on step %d, want 2 — the most fragile long interval first", r.Step)
	}

	// with the budget used up only the 3-hour step may come through
	if _, err := NextDueReminder(db, uid, now, horizon, 0); err == nil {
		t.Error("budget 0 must hold back every long-interval word")
	}
	ScheduleReminder(db, uid, "забытое", 1, now.Add(-time.Minute))
	if r, _ := NextDueReminder(db, uid, now, horizon, 0); r.Word != "забытое" {
		t.Errorf("budget 0 returned %q, want the lapsed «забытое» — the 3-hour step is never capped", r.Word)
	}
	if n, _ := CountDueReminders(db, uid, now, horizon, 0); n != 1 {
		t.Errorf("budget 0 count = %d, want 1", n)
	}
	r, _ = NextDueReminder(db, uid, now, horizon, 0)
	MarkReminderSent(db, r.ID)

	// the budget is measured by long-interval words actually asked
	if n, _ := CountLongSent(db, uid, now.Add(-24*time.Hour)); n != 0 {
		t.Fatalf("CountLongSent = %d before any long word was asked, want 0 (the 3-hour word doesn't count)", n)
	}
	long, _ := NextDueReminder(db, uid, now, horizon, 15)
	MarkReminderSent(db, long.ID)
	if n, _ := CountLongSent(db, uid, now.Add(-24*time.Hour)); n != 1 {
		t.Errorf("CountLongSent = %d after asking one long word, want 1", n)
	}
	if n, _ := CountLongSent(db, uid, now.Add(time.Hour)); n != 0 {
		t.Errorf("CountLongSent must only look at the window, got %d", n)
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

	if n, _ := CountDueReminders(db, uid, at0600, at0600, -1); n != 0 {
		t.Fatalf("the bug: before normalization the +05:00 row must NOT compare as due, got %d", n)
	}
	fixed, err := NormalizeReminderTimes(db)
	if err != nil || fixed != 1 {
		t.Fatalf("NormalizeReminderTimes = %d, %v; want 1 row fixed", fixed, err)
	}
	if n, _ := CountDueReminders(db, uid, at0600, at0600, -1); n != 1 {
		t.Errorf("after normalization the 05:00 UTC row must be due at 06:00 UTC, got %d", n)
	}
	if again, _ := NormalizeReminderTimes(db); again != 0 {
		t.Errorf("normalization must be idempotent, touched %d rows on second run", again)
	}

	// horizon: a day-interval word ripening in 2h joins today's batch but is
	// not "due now"
	ScheduleReminder(db, uid, "скоро", 2, at0600.Add(2*time.Hour))
	if n, _ := CountDueReminders(db, uid, at0600, at0600, -1); n != 1 {
		t.Errorf("due-now count = %d, want 1", n)
	}
	if n, _ := CountDueReminders(db, uid, at0600, at0600.Add(12*time.Hour), -1); n != 2 {
		t.Errorf("12h-window count = %d, want 2", n)
	}
	if r, _ := NextDueReminder(db, uid, at0600, at0600.Add(12*time.Hour), -1); r.Word != "мак" {
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
	SaveVocab(db, uid, "крыша", "", "")
	SaveVocab(db, uid, "дерево", "", "")
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
	if err := SaveVocab(db, 1, "крыша", "屋根(やね) (yane)", ""); err != nil {
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
	if err := SaveVocab(db, 1, "крыша", "屋根(やね) (yane)", ""); err != nil {
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

func TestMigrateIsIdempotent(t *testing.T) {
	db := testDB(t) // already migrated once
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := SetState(db, 1, "reminder", "крыша", "", 7); err != nil {
		t.Fatalf("SetState after migration: %v", err)
	}
}

func TestIdleModes(t *testing.T) {
	db := testDB(t)
	SetState(db, 1, "reminder", "крыша", "", 7)
	SetCurrentWord(db, 2, "окно") // mode '' — not listed

	modes, err := ActiveModes(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(modes) != 1 || modes[0].UserID != 1 || modes[0].Mode != "reminder" || modes[0].ReminderID != 7 || modes[0].Nudged {
		t.Fatalf("ActiveModes = %+v", modes)
	}
	if time.Since(modes[0].ActiveAt) > time.Minute {
		t.Fatalf("ActiveAt not set by SetState: %v", modes[0].ActiveAt)
	}

	past := time.Now().Add(-time.Hour)
	if ok, _ := MarkNudged(db, 1, "reminder", past); ok {
		t.Fatal("nudged a user who was active just now")
	}
	if ok, _ := ClearIdleMode(db, 1, "reminder", past); ok {
		t.Fatal("cleared a mode the user was active in just now")
	}

	later := time.Now().Add(time.Minute)
	if ok, _ := ClearIdleMode(db, 1, "ask", later); ok {
		t.Fatal("cleared although the mode had changed")
	}
	if ok, _ := MarkNudged(db, 1, "reminder", later); !ok {
		t.Fatal("idle user was not nudged")
	}
	if ok, _ := MarkNudged(db, 1, "reminder", later); ok {
		t.Fatal("nudged twice")
	}
	if ok, _ := ClearIdleMode(db, 1, "reminder", later); !ok {
		t.Fatal("idle mode was not cleared")
	}
	if st := GetState(db, 1); st.Mode != "" || st.Word != "крыша" {
		t.Fatalf("after ClearIdleMode state = %+v", st)
	}

	// a new mode starts a fresh clock with a fresh nudge
	SetState(db, 1, "ask", "は", "", 0)
	if m, _ := ActiveModes(db); len(m) != 1 || m[0].Nudged {
		t.Fatalf("new mode inherited nudge: %+v", m)
	}
}
