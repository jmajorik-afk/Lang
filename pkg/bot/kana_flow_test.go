package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"language-learning-bot/pkg/storage"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/mattn/go-sqlite3"
)

// fakeTelegram answers the Bot API calls the trainer makes and keeps the texts
// it was asked to send.
type fakeTelegram struct {
	mu    sync.Mutex
	texts []string
	next  int
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot","username":"bot"}}`)
		return
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		f.texts = append(f.texts, r.Form.Get("text"))
	}
	fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"date":0,"chat":{"id":7}}}`, f.next)
}

func (f *fakeTelegram) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.texts) == 0 {
		return ""
	}
	return f.texts[len(f.texts)-1]
}

func kanaTestEnv(t *testing.T) (*tgbotapi.BotAPI, *sql.DB, *fakeTelegram) {
	t.Helper()
	fake := &fakeTelegram{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", srv.URL+"/bot%s/%s", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // every connection to :memory: would be a new, empty database
	t.Cleanup(func() { db.Close() })
	schema, err := os.ReadFile("../../scripts/init_db.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if err := storage.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return bot, db, fake
}

const kanaTestUser = 7

func kanaOpen(t *testing.T, db *sql.DB) (kanaState, bool) {
	t.Helper()
	st := storage.GetState(db, kanaTestUser)
	if st.Mode != modeKana {
		return kanaState{}, false
	}
	var ks kanaState
	if err := json.Unmarshal([]byte(st.TaskText), &ks); err != nil {
		t.Fatalf("bad kana state %q: %v", st.TaskText, err)
	}
	return ks, true
}

// answerKana answers the open question the way a user would: typing the reading
// or tapping a button. right picks the right answer or a wrong one.
func answerKana(bot *tgbotapi.BotAPI, db *sql.DB, ks kanaState, right bool) {
	if ks.Dir == "read" {
		answer := kanaByKana[ks.Item].Romaji[0]
		if !right {
			answer = "xx"
		}
		msg := &tgbotapi.Message{Text: answer, Chat: &tgbotapi.Chat{ID: kanaTestUser},
			From: &tgbotapi.User{ID: kanaTestUser}}
		handleKanaText(bot, db, msg, storage.GetState(db, kanaTestUser))
		return
	}
	choice := ks.Right
	if !right {
		choice = (ks.Right + 1) % len(ks.Opts)
	}
	cq := &tgbotapi.CallbackQuery{From: &tgbotapi.User{ID: kanaTestUser},
		Message: &tgbotapi.Message{MessageID: ks.Msg, Chat: &tgbotapi.Chat{ID: kanaTestUser}}}
	handleKanaButton(bot, db, cq, fmt.Sprintf("kana_pick#%d#%d", ks.Asked, choice))
}

// TestKanaRoundsEndToEnd plays whole rounds in every mode through the real
// handlers, the real schema and a fake Telegram.
func TestKanaRoundsEndToEnd(t *testing.T) {
	for _, mode := range []string{"hiragana", "katakana", "mixed", "pairs"} {
		t.Run(mode, func(t *testing.T) {
			bot, db, fake := kanaTestEnv(t)
			startKanaRound(bot, db, kanaTestUser, kanaTestUser, mode)
			if !strings.Contains(fake.last(), "Уровень 1") {
				t.Fatalf("round opened with %q", fake.last())
			}
			for round := 0; round < 3; round++ {
				for q := 0; q < kanaRoundSize; q++ {
					ks, open := kanaOpen(t, db)
					if !open {
						t.Fatalf("round %d: no open question after %d answers; last message %q", round, q, fake.last())
					}
					if ks.Asked != q {
						t.Fatalf("question counter %d, want %d", ks.Asked, q)
					}
					if ks.Dir != "read" && (len(ks.Opts) != 4 || ks.Msg == 0) {
						t.Fatalf("button question without four options or message id: %+v", ks)
					}
					answerKana(bot, db, ks, q != 3) // one miss per round
				}
				if _, open := kanaOpen(t, db); open {
					t.Fatal("the round did not close after 10 answers")
				}
				if last := fake.last(); !strings.Contains(last, "Раунд окончен: 9 из 10") {
					t.Fatalf("round summary %q", last)
				}
				startKanaRound(bot, db, kanaTestUser, kanaTestUser, mode) // «Ещё 10»
			}
			prog, _ := storage.KanaProgress(db, kanaTestUser)
			seen := 0
			for _, s := range prog {
				seen += s.Seen
			}
			if seen != 30 {
				t.Errorf("recorded %d answers, want 30", seen)
			}
		})
	}
}

// TestKanaStageAdvancesInPlay — answering everything right moves the user from
// reading to writing within a few rounds, and says so.
func TestKanaStageAdvancesInPlay(t *testing.T) {
	bot, db, fake := kanaTestEnv(t)
	passed := false
	for round := 0; round < 12 && !passed; round++ {
		startKanaRound(bot, db, kanaTestUser, kanaTestUser, "hiragana")
		for q := 0; q < kanaRoundSize; q++ {
			ks, open := kanaOpen(t, db)
			if !open {
				break
			}
			answerKana(bot, db, ks, true)
			if strings.Contains(fake.last(), "Уровень пройден") {
				passed = true
			}
		}
	}
	if !passed {
		t.Fatal("46 kana answered right never passed level 1 reading")
	}
	if got := storage.GetKanaStage(db, kanaTestUser, "hiragana"); got != 1 {
		t.Errorf("stage %d, want 1", got)
	}
}

// TestKanaStaleAndStrayInput — an old button, Cyrillic text and «Не знаю».
func TestKanaStaleAndStrayInput(t *testing.T) {
	bot, db, fake := kanaTestEnv(t)
	startKanaRound(bot, db, kanaTestUser, kanaTestUser, "hiragana")
	ks, _ := kanaOpen(t, db)

	stale := &tgbotapi.CallbackQuery{From: &tgbotapi.User{ID: kanaTestUser},
		Message: &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: kanaTestUser}}}
	handleKanaButton(bot, db, stale, fmt.Sprintf("kana_pick#%d#0", ks.Asked+5))
	if now, _ := kanaOpen(t, db); now.Asked != ks.Asked {
		t.Fatal("a tap on an old question counted as an answer")
	}

	msg := &tgbotapi.Message{Text: "окно", Chat: &tgbotapi.Chat{ID: kanaTestUser}, From: &tgbotapi.User{ID: kanaTestUser}}
	handleKanaText(bot, db, msg, storage.GetState(db, kanaTestUser))
	if now, _ := kanaOpen(t, db); now.Asked != ks.Asked {
		t.Fatal("a Russian word counted as an answer")
	}

	handleKanaButton(bot, db, stale, fmt.Sprintf("kana_skip#%d", ks.Asked))
	if now, _ := kanaOpen(t, db); now.Asked != ks.Asked+1 {
		t.Fatal("«Не знаю» did not move on")
	}
	if !strings.Contains(fake.last(), kanaByKana[ks.Item].Romaji[0]) {
		t.Errorf("«Не знаю» must show the answer, got %q", fake.last())
	}
}
