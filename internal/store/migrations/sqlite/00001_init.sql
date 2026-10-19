-- +goose Up
CREATE TABLE relay_config (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL, tls TEXT NOT NULL, username TEXT NOT NULL, password BLOB NOT NULL, from_address TEXT NOT NULL, from_name TEXT NOT NULL);
INSERT INTO relay_config VALUES (1, 0, '', 0, '', '', X'', '', '');
CREATE TABLE queue_capacity (id INTEGER PRIMARY KEY CHECK (id = 1), messages INTEGER NOT NULL, bytes INTEGER NOT NULL);
INSERT INTO queue_capacity VALUES (1, 0, 0);
CREATE TABLE messages (id TEXT PRIMARY KEY, raw BLOB NOT NULL, sender TEXT NOT NULL, size INTEGER NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, claim_token TEXT NOT NULL DEFAULT '', lease_until INTEGER NOT NULL DEFAULT 0);
CREATE TABLE recipients (id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE, address TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER NOT NULL, next_attempt_at INTEGER NOT NULL, last_error TEXT NOT NULL DEFAULT '', completed_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX recipients_due ON recipients(status, next_attempt_at, message_id);
CREATE INDEX recipients_message ON recipients(message_id);
-- +goose Down
DROP TABLE recipients;
DROP TABLE messages;
DROP TABLE queue_capacity;
DROP TABLE relay_config;
