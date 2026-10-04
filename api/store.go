package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed seed/dictionary.tsv
var seedFiles embed.FS

type Store struct{ db *sql.DB }

func openStore(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err = s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.seedDictionary(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS cards (
  id TEXT PRIMARY KEY,
  word TEXT NOT NULL,
  ipa TEXT NOT NULL DEFAULT '',
  definition TEXT NOT NULL DEFAULT '',
  context_text TEXT NOT NULL DEFAULT '',
  context_meaning TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  due_at TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'new',
  difficulty REAL NOT NULL DEFAULT 0,
  stability REAL NOT NULL DEFAULT 0,
  last_review TEXT NOT NULL DEFAULT '',
  reps INTEGER NOT NULL DEFAULT 0,
  lapses INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS cards_due_idx ON cards(due_at);
CREATE INDEX IF NOT EXISTS cards_word_idx ON cards(word COLLATE NOCASE);
CREATE TABLE IF NOT EXISTS review_events (
  id TEXT PRIMARY KEY,
  card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  rating INTEGER NOT NULL CHECK (rating BETWEEN 1 AND 4),
  reviewed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS reviews_card_idx ON review_events(card_id, reviewed_at);
CREATE TABLE IF NOT EXISTS dictionary_entries (
  word TEXT PRIMARY KEY COLLATE NOCASE,
  ipa TEXT NOT NULL DEFAULT '',
  definition TEXT NOT NULL DEFAULT '',
  locale TEXT NOT NULL DEFAULT 'en-US',
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS batch_requests (
  request_id TEXT PRIMARY KEY,
  response TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`)
	return err
}

// allSettings returns the saved key/value settings; an empty map means the
// deployment has never saved anything from the web UI and env vars still rule.
func (s *Store) allSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, rows.Err()
}

func (s *Store) putSettings(ctx context.Context, values map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) seedDictionary(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM dictionary_entries`).Scan(&count); err != nil || count > 0 {
		return err
	}
	data, err := fs.ReadFile(seedFiles, "seed/dictionary.tsv")
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO dictionary_entries(word, ipa, definition, locale, updated_at) VALUES(?, ?, ?, 'en-US', ?)`, fields[0], fields[1], fields[2], now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) listCards(ctx context.Context, scope, query string) ([]Card, error) {
	where := []string{}
	args := []any{}
	if scope == "due" {
		where = append(where, "due_at <= ?")
		args = append(args, time.Now().UTC().Format(time.RFC3339Nano))
	}
	if strings.TrimSpace(query) != "" {
		where = append(where, "(word LIKE ? OR definition LIKE ? OR context_text LIKE ?)")
		needle := "%" + strings.TrimSpace(query) + "%"
		args = append(args, needle, needle, needle)
	}
	sqlQuery := `SELECT id, word, ipa, definition, context_text, context_meaning, source, created_at, updated_at, due_at, state, difficulty, stability, last_review, reps, lapses FROM cards`
	if len(where) > 0 {
		sqlQuery += " WHERE " + strings.Join(where, " AND ")
	}
	sqlQuery += ` ORDER BY CASE WHEN due_at <= datetime('now') THEN 0 ELSE 1 END, due_at, word COLLATE NOCASE LIMIT 2000`
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cards := make([]Card, 0)
	for rows.Next() {
		card, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}
	return cards, rows.Err()
}

func (s *Store) allCards(ctx context.Context) ([]Card, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cardColumns+` FROM cards ORDER BY word COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cards := make([]Card, 0)
	for rows.Next() {
		card, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}
	return cards, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanCard(row scanner) (Card, error) {
	var c Card
	var created, updated, due, last string
	err := row.Scan(&c.ID, &c.Word, &c.IPA, &c.Definition, &c.ContextText, &c.ContextMeaning, &c.Source, &created, &updated, &due, &c.State, &c.Difficulty, &c.Stability, &last, &c.Reps, &c.Lapses)
	if err != nil {
		return Card{}, err
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	c.DueAt, _ = time.Parse(time.RFC3339Nano, due)
	if last != "" {
		c.LastReview, _ = time.Parse(time.RFC3339Nano, last)
	}
	return c, nil
}

const cardColumns = `id, word, ipa, definition, context_text, context_meaning, source, created_at, updated_at, due_at, state, difficulty, stability, last_review, reps, lapses`

func (s *Store) getCard(ctx context.Context, id string) (Card, error) {
	return scanCard(s.db.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM cards WHERE id = ?`, id))
}

func (s *Store) createCard(ctx context.Context, input CardInput, id string) (Card, error) {
	now := time.Now().UTC()
	c := Card{ID: id, Word: strings.TrimSpace(input.Word), IPA: strings.TrimSpace(input.IPA), Definition: strings.TrimSpace(input.Definition), ContextText: strings.TrimSpace(input.ContextText), ContextMeaning: strings.TrimSpace(input.ContextMeaning), Source: strings.TrimSpace(input.Source), CreatedAt: now, UpdatedAt: now, DueAt: now, State: "new"}
	_, err := s.db.ExecContext(ctx, `INSERT INTO cards (`+cardColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET word=excluded.word, ipa=excluded.ipa, definition=excluded.definition, context_text=excluded.context_text, context_meaning=excluded.context_meaning, source=excluded.source, updated_at=excluded.updated_at`,
		c.ID, c.Word, c.IPA, c.Definition, c.ContextText, c.ContextMeaning, c.Source, c.CreatedAt.Format(time.RFC3339Nano), c.UpdatedAt.Format(time.RFC3339Nano), c.DueAt.Format(time.RFC3339Nano), c.State, c.Difficulty, c.Stability, "", c.Reps, c.Lapses)
	if err != nil {
		return Card{}, err
	}
	return s.getCard(ctx, id)
}

func (s *Store) updateCard(ctx context.Context, id string, input CardInput) (Card, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE cards SET word=?, ipa=?, definition=?, context_text=?, context_meaning=?, source=?, updated_at=? WHERE id=?`,
		strings.TrimSpace(input.Word), strings.TrimSpace(input.IPA), strings.TrimSpace(input.Definition), strings.TrimSpace(input.ContextText), strings.TrimSpace(input.ContextMeaning), strings.TrimSpace(input.Source), time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return Card{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Card{}, sql.ErrNoRows
	}
	return s.getCard(ctx, id)
}

func (s *Store) deleteCard(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cards WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// findCardByWord returns an arbitrary card with the given word (case-insensitive).
// Cards are word-in-context units, so several cards may share one word.
func (s *Store) findCardByWord(ctx context.Context, word string) (Card, bool, error) {
	card, err := scanCard(s.db.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM cards WHERE word = ? COLLATE NOCASE ORDER BY created_at LIMIT 1`, word))
	if errors.Is(err, sql.ErrNoRows) {
		return Card{}, false, nil
	}
	if err != nil {
		return Card{}, false, err
	}
	return card, true, nil
}

func (s *Store) dictionaryExact(ctx context.Context, word string) (DictionaryEntry, bool, error) {
	var entry DictionaryEntry
	err := s.db.QueryRowContext(ctx, `SELECT word, ipa, definition, locale FROM dictionary_entries WHERE word = ? COLLATE NOCASE`, word).Scan(&entry.Word, &entry.IPA, &entry.Definition, &entry.Locale)
	if errors.Is(err, sql.ErrNoRows) {
		return DictionaryEntry{}, false, nil
	}
	if err != nil {
		return DictionaryEntry{}, false, err
	}
	return entry, true, nil
}

func (s *Store) loadBatchResponse(ctx context.Context, requestID string) (BatchResponse, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT response FROM batch_requests WHERE request_id = ?`, requestID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return BatchResponse{}, false, nil
	}
	if err != nil {
		return BatchResponse{}, false, err
	}
	var response BatchResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return BatchResponse{}, false, fmt.Errorf("stored batch response is corrupt: %w", err)
	}
	return response, true, nil
}

func (s *Store) saveBatchResponse(ctx context.Context, requestID string, response BatchResponse) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO batch_requests(request_id, response, created_at) VALUES(?, ?, ?)
ON CONFLICT(request_id) DO NOTHING`, requestID, string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) reviewCard(ctx context.Context, cardID string, request ReviewRequest) (Card, bool, error) {
	if request.ID == "" || request.Rating < 1 || request.Rating > 4 {
		return Card{}, false, errors.New("review id and rating 1-4 are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Card{}, false, err
	}
	defer tx.Rollback()
	var existingCardID string
	err = tx.QueryRowContext(ctx, `SELECT card_id FROM review_events WHERE id=?`, request.ID).Scan(&existingCardID)
	if err == nil {
		if existingCardID != cardID {
			return Card{}, false, errors.New("review id has already been used for a different card")
		}
		c, getErr := scanCard(tx.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM cards WHERE id=?`, cardID))
		return c, true, getErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Card{}, false, err
	}
	c, err := scanCard(tx.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM cards WHERE id=?`, cardID))
	if err != nil {
		return Card{}, false, err
	}
	now := time.Now().UTC()
	if !request.ReviewedAt.IsZero() && !request.ReviewedAt.After(now.Add(5*time.Minute)) {
		now = request.ReviewedAt.UTC()
	}
	c = applyFSRS(c, request.Rating, now)
	_, err = tx.ExecContext(ctx, `UPDATE cards SET updated_at=?, due_at=?, state=?, difficulty=?, stability=?, last_review=?, reps=?, lapses=? WHERE id=?`, c.UpdatedAt.Format(time.RFC3339Nano), c.DueAt.Format(time.RFC3339Nano), c.State, c.Difficulty, c.Stability, c.LastReview.Format(time.RFC3339Nano), c.Reps, c.Lapses, c.ID)
	if err != nil {
		return Card{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_events(id, card_id, rating, reviewed_at) VALUES(?, ?, ?, ?)`, request.ID, cardID, request.Rating, now.Format(time.RFC3339Nano))
	if err != nil {
		return Card{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Card{}, false, err
	}
	return c, false, nil
}

func (s *Store) searchDictionary(ctx context.Context, q string) ([]DictionaryEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT word, ipa, definition, locale FROM dictionary_entries WHERE word LIKE ? ORDER BY CASE WHEN word=? COLLATE NOCASE THEN 0 ELSE 1 END, word COLLATE NOCASE LIMIT 30`, q+"%", q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]DictionaryEntry, 0)
	for rows.Next() {
		var d DictionaryEntry
		if err := rows.Scan(&d.Word, &d.IPA, &d.Definition, &d.Locale); err != nil {
			return nil, err
		}
		entries = append(entries, d)
	}
	return entries, rows.Err()
}

func (s *Store) importDictionary(ctx context.Context, entries []DictionaryEntry) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	count := 0
	for _, entry := range entries {
		entry.Word = strings.TrimSpace(entry.Word)
		if entry.Word == "" {
			continue
		}
		if entry.Locale == "" {
			entry.Locale = "en-US"
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dictionary_entries(word, ipa, definition, locale, updated_at) VALUES(?, ?, ?, ?, ?) ON CONFLICT(word) DO UPDATE SET ipa=excluded.ipa, definition=excluded.definition, locale=excluded.locale, updated_at=excluded.updated_at`, entry.Word, strings.TrimSpace(entry.IPA), strings.TrimSpace(entry.Definition), entry.Locale, now)
		if err != nil {
			return count, err
		}
		count++
	}
	return count, tx.Commit()
}

func (s *Store) export(ctx context.Context) (BackupBundle, error) {
	cards, err := s.allCards(ctx)
	if err != nil {
		return BackupBundle{}, err
	}
	bundle := BackupBundle{Version: 1, ExportedAt: time.Now().UTC(), Cards: cards, Reviews: []ReviewEvent{}, Dictionary: []DictionaryEntry{}}
	rows, err := s.db.QueryContext(ctx, `SELECT id, card_id, rating, reviewed_at FROM review_events ORDER BY reviewed_at`)
	if err != nil {
		return bundle, err
	}
	for rows.Next() {
		var ev ReviewEvent
		var reviewed string
		if err := rows.Scan(&ev.ID, &ev.CardID, &ev.Rating, &reviewed); err != nil {
			rows.Close()
			return bundle, err
		}
		ev.ReviewedAt, _ = time.Parse(time.RFC3339Nano, reviewed)
		bundle.Reviews = append(bundle.Reviews, ev)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return bundle, err
	}
	rows.Close()
	dRows, err := s.db.QueryContext(ctx, `SELECT word, ipa, definition, locale FROM dictionary_entries ORDER BY word COLLATE NOCASE`)
	if err != nil {
		return bundle, err
	}
	defer dRows.Close()
	for dRows.Next() {
		var d DictionaryEntry
		if err := dRows.Scan(&d.Word, &d.IPA, &d.Definition, &d.Locale); err != nil {
			return bundle, err
		}
		bundle.Dictionary = append(bundle.Dictionary, d)
	}
	return bundle, dRows.Err()
}

func (s *Store) importBundle(ctx context.Context, bundle BackupBundle) error {
	if bundle.Version != 1 {
		return fmt.Errorf("unsupported backup version %d", bundle.Version)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range bundle.Cards {
		if c.ID == "" || strings.TrimSpace(c.Word) == "" {
			continue
		}
		if c.CreatedAt.IsZero() {
			c.CreatedAt = time.Now().UTC()
		}
		if c.UpdatedAt.IsZero() {
			c.UpdatedAt = c.CreatedAt
		}
		if c.DueAt.IsZero() {
			c.DueAt = time.Now().UTC()
		}
		last := ""
		if !c.LastReview.IsZero() {
			last = c.LastReview.UTC().Format(time.RFC3339Nano)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO cards (`+cardColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET word=excluded.word, ipa=excluded.ipa, definition=excluded.definition, context_text=excluded.context_text, context_meaning=excluded.context_meaning, source=excluded.source, created_at=excluded.created_at, updated_at=excluded.updated_at, due_at=excluded.due_at, state=excluded.state, difficulty=excluded.difficulty, stability=excluded.stability, last_review=excluded.last_review, reps=excluded.reps, lapses=excluded.lapses`, c.ID, strings.TrimSpace(c.Word), c.IPA, c.Definition, c.ContextText, c.ContextMeaning, c.Source, c.CreatedAt.UTC().Format(time.RFC3339Nano), c.UpdatedAt.UTC().Format(time.RFC3339Nano), c.DueAt.UTC().Format(time.RFC3339Nano), c.State, c.Difficulty, c.Stability, last, c.Reps, c.Lapses)
		if err != nil {
			return err
		}
	}
	for _, ev := range bundle.Reviews {
		if ev.ID == "" || ev.CardID == "" || ev.Rating < 1 || ev.Rating > 4 {
			continue
		}
		if ev.ReviewedAt.IsZero() {
			ev.ReviewedAt = time.Now().UTC()
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO review_events(id, card_id, rating, reviewed_at) VALUES(?, ?, ?, ?)`, ev.ID, ev.CardID, ev.Rating, ev.ReviewedAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	for _, d := range bundle.Dictionary {
		if strings.TrimSpace(d.Word) == "" {
			continue
		}
		if d.Locale == "" {
			d.Locale = "en-US"
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO dictionary_entries(word, ipa, definition, locale, updated_at) VALUES(?, ?, ?, ?, ?) ON CONFLICT(word) DO UPDATE SET ipa=excluded.ipa, definition=excluded.definition, locale=excluded.locale, updated_at=excluded.updated_at`, strings.TrimSpace(d.Word), d.IPA, d.Definition, d.Locale, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
