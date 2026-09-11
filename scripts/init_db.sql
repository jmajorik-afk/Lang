-- Schema for the Japanese learning bot (3 flows + /ask)

CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY,
    speech_speed REAL NOT NULL DEFAULT 1.0
);

-- Saved vocabulary (only words the user explicitly pressed "Запомнить" on)
CREATE TABLE IF NOT EXISTS vocab (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    word TEXT NOT NULL,
    translation TEXT NOT NULL DEFAULT '', -- Japanese: "kanji(чтение) (romaji)"
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, word)
);

-- Spaced-repetition reminders (one pending row per word at a time)
CREATE TABLE IF NOT EXISTS reminders (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    word TEXT NOT NULL,
    step INTEGER NOT NULL DEFAULT 1,
    send_at DATETIME NOT NULL,
    sent INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_reminders_due ON reminders (send_at, sent);

-- Per-user interaction state machine
CREATE TABLE IF NOT EXISTS user_state (
    user_id INTEGER PRIMARY KEY,
    mode TEXT NOT NULL DEFAULT '',          -- '' | practice_compose | practice_translate | paused_compose | paused_translate | reminder | srs_offer | confirm_save | await_word | ask
    -- during 'reminder' task_text holds 'session' while working through a batch
    word TEXT NOT NULL DEFAULT '',          -- current / active word
    task_text TEXT NOT NULL DEFAULT '',     -- the sentence shown for practice
    reminder_id INTEGER NOT NULL DEFAULT 0,
    last_reminder_at DATETIME,
    last_answer TEXT NOT NULL DEFAULT ''   -- most recent judged answer, for «Оспорить»
);

-- Short conversation history fed back to Claude for word lookups
CREATE TABLE IF NOT EXISTS conversations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    user_message TEXT NOT NULL,
    bot_response TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_conversations_user ON conversations (user_id, created_at);

-- Per-user daily count of user-initiated API interactions (cost control)
CREATE TABLE IF NOT EXISTS api_usage (
    user_id INTEGER NOT NULL,
    day TEXT NOT NULL,                      -- YYYY-MM-DD
    calls INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, day)
);

-- Global cache of word → Japanese translation, with Jisho verification flag
CREATE TABLE IF NOT EXISTS translation_cache (
    word TEXT PRIMARY KEY,
    translation TEXT NOT NULL,
    verified INTEGER NOT NULL DEFAULT 0,    -- 1 = reading confirmed by Jisho
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
