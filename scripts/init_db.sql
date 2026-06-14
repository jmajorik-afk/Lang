-- Initialize SQL Schema (init_db.sql)

-- Users Table
CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY,
    language TEXT NOT NULL,
    help_type TEXT NOT NULL,
    speech_speed REAL NOT NULL DEFAULT 0.0 -- Set a default value for the speech_speed column
);

-- Queries Table
CREATE TABLE IF NOT EXISTS queries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    word TEXT NOT NULL,
    language TEXT NOT NULL,
    help_type TEXT NOT NULL,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id)
);

-- Add indexes to queries table
CREATE INDEX IF NOT EXISTS idx_queries_language ON queries (language, help_type, word);

-- Reminders Table (spaced repetition)
CREATE TABLE IF NOT EXISTS reminders (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    word TEXT NOT NULL,
    language TEXT NOT NULL,
    help_type TEXT NOT NULL,
    send_at DATETIME NOT NULL,
    sent INTEGER NOT NULL DEFAULT 0,
    step INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id)
);

CREATE INDEX IF NOT EXISTS idx_reminders_send_at ON reminders (send_at, sent);

-- Add learned_at column to reminders if not exists (migration-safe via temp table approach is handled in Go)


-- Conversation history for passing context to Claude
CREATE TABLE IF NOT EXISTS conversations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    user_message TEXT NOT NULL,
    bot_response TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_conversations_user ON conversations (user_id, created_at);

