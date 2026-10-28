package sms

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const (
	inboxLimit  = 100
	inboxMaxAge = 30 * 24 * time.Hour
)

// StoredMessage is an inbox entry. Times are Unix milliseconds.
type StoredMessage struct {
	ID            string `json:"id"`
	From          string `json:"from"`
	Text          string `json:"text"`
	Timestamp     string `json:"timestamp"`
	ReceivedAt    int64  `json:"received-at"`
	Read          bool   `json:"read"`
	ForwardStatus string `json:"forward-status,omitempty"`
}

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("inbox path is not a regular file")
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA secure_delete=ON", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON",
		`CREATE TABLE IF NOT EXISTS inbox (id TEXT PRIMARY KEY, sender TEXT NOT NULL, body TEXT NOT NULL, timestamp TEXT NOT NULL, received_at INTEGER NOT NULL, is_read INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS pending_modem (path TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, entry_id TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS forward_jobs (id TEXT PRIMARY KEY, sender TEXT NOT NULL, destination TEXT NOT NULL, body TEXT NOT NULL, segments INTEGER NOT NULL, state TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, budget_day TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS forward_budget (day TEXT PRIMARY KEY, segments INTEGER NOT NULL)`,
	} {
		if _, err = db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize inbox: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Add(msg Message, now time.Time) (StoredMessage, error) {
	entry, _, err := s.Record(msg, now)
	return entry, err
}

// Record commits a message and its modem identity atomically. A pending modem
// copy is recognized after process restart, including after a failed delete.
func (s *Store) Record(msg Message, now time.Time) (StoredMessage, bool, error) {
	return s.RecordWithForward(msg, now, "off")
}

func (s *Store) RecordWithForward(msg Message, now time.Time, destination string) (StoredMessage, bool, error) {
	identity, err := json.Marshal([]string{msg.Number, msg.Text, msg.SourceTimestamp})
	if err != nil {
		return StoredMessage{}, false, err
	}
	digest := sha256.Sum256(identity)
	fingerprint := hex.EncodeToString(digest[:])
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return StoredMessage{}, false, err
	}
	entry := StoredMessage{ID: hex.EncodeToString(random[:]), From: msg.Number, Text: msg.Text, Timestamp: msg.Timestamp.Format(time.RFC3339), ReceivedAt: now.UnixMilli()}
	tx, err := s.db.Begin()
	if err != nil {
		return StoredMessage{}, false, err
	}
	defer tx.Rollback()
	if msg.SourcePath != "" {
		var id, previous string
		err = tx.QueryRow(`SELECT entry_id,fingerprint FROM pending_modem WHERE path=?`, msg.SourcePath).Scan(&id, &previous)
		if err != nil && err != sql.ErrNoRows {
			return StoredMessage{}, false, err
		}
		if err == nil && previous == fingerprint {
			entry.ID = id
			return entry, false, nil
		}
	}
	if _, err = tx.Exec(`INSERT INTO inbox(id,sender,body,timestamp,received_at) VALUES(?,?,?,?,?)`, entry.ID, entry.From, entry.Text, entry.Timestamp, entry.ReceivedAt); err != nil {
		return StoredMessage{}, false, err
	}
	if msg.SourcePath != "" {
		if _, err = tx.Exec(`INSERT INTO pending_modem(path,fingerprint,entry_id) VALUES(?,?,?) ON CONFLICT(path) DO UPDATE SET fingerprint=excluded.fingerprint,entry_id=excluded.entry_id`, msg.SourcePath, fingerprint, entry.ID); err != nil {
			return StoredMessage{}, false, err
		}
	}
	if body, segments, ok := ForwardText(msg, destination); ok {
		if _, err = tx.Exec(`INSERT INTO forward_jobs(id,sender,destination,body,segments,state,created_at,expires_at) VALUES(?,?,?,?,?,'queued',?,?)`, entry.ID, msg.Number, destination, body, segments, now.UnixMilli(), now.Add(24*time.Hour).UnixMilli()); err != nil {
			return StoredMessage{}, false, err
		}
	}
	if _, err = tx.Exec(`DELETE FROM inbox WHERE received_at < ? OR id IN (SELECT id FROM inbox ORDER BY received_at DESC, rowid DESC LIMIT -1 OFFSET ?)`, now.Add(-inboxMaxAge).UnixMilli(), inboxLimit); err != nil {
		return StoredMessage{}, false, err
	}
	if _, err = tx.Exec(`DELETE FROM forward_jobs WHERE id NOT IN (SELECT id FROM inbox) AND state <> 'sending'`); err != nil {
		return StoredMessage{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return StoredMessage{}, false, err
	}
	return entry, true, nil
}

func (s *Store) ForgetModem(path string) error {
	_, err := s.db.Exec(`DELETE FROM pending_modem WHERE path=?`, path)
	return err
}

func (s *Store) List(now time.Time) ([]StoredMessage, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM inbox WHERE received_at < ?`, now.Add(-inboxMaxAge).UnixMilli()); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`DELETE FROM forward_jobs WHERE id NOT IN (SELECT id FROM inbox) AND state <> 'sending'`); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT inbox.id,inbox.sender,inbox.body,inbox.timestamp,inbox.received_at,inbox.is_read,COALESCE(forward_jobs.state,'') FROM inbox LEFT JOIN forward_jobs ON forward_jobs.id=inbox.id ORDER BY inbox.received_at DESC, inbox.rowid DESC LIMIT ?`, inboxLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]StoredMessage, 0)
	for rows.Next() {
		var entry StoredMessage
		if err := rows.Scan(&entry.ID, &entry.From, &entry.Text, &entry.Timestamp, &entry.ReceivedAt, &entry.Read, &entry.ForwardStatus); err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (s *Store) MarkRead(id string) (bool, error) {
	return s.change(`UPDATE inbox SET is_read=1 WHERE id=?`, id)
}
func (s *Store) Delete(id string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM inbox WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(`DELETE FROM forward_jobs WHERE id=?`, id); err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return n != 0, nil
}
func (s *Store) change(query, id string) (bool, error) {
	result, err := s.db.Exec(query, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n != 0, err
}
