// local event db, written by several processes at once
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/store/sqlc"
	"github.com/google/uuid"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed schema.sql
var schema string

type Store struct {
	db *sql.DB
	q  *sqlc.Queries
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := useWAL(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := addChainColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, q: sqlc.New(db)}, nil
}

// databases from before the chain. two processes can race here, so a column that's already there is fine
func addChainColumns(db *sql.DB) error {
	for _, col := range []string{"prev_hash", "hash"} {
		_, err := db.Exec("ALTER TABLE events ADD COLUMN " + col + " TEXT")
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// busy_timeout doesn't cover switching to wal, so racing first opens retry
func useWAL(db *sql.DB) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var mode string
		err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode)
		var sqliteErr *sqlite.Error
		busy := errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
		if !busy || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Append(ctx context.Context, e event.Event) (event.Event, error) {
	err := s.Update(ctx, func(tx *Tx) error {
		var err error
		e, err = tx.Append(ctx, e)
		return err
	})
	return e, err
}

type Tx struct{ q *sqlc.Queries }

func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&Tx{q: s.q.WithTx(tx)}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (t *Tx) Append(ctx context.Context, e event.Event) (event.Event, error) {
	data := string(e.Data)
	if data == "" {
		data = "{}"
	}
	seq, err := t.q.InsertEvent(ctx, sqlc.InsertEventParams{
		Ts:        e.TS.UnixMilli(),
		Kind:      string(e.Kind),
		SessionID: e.SessionID,
		ProjectID: e.ProjectID,
		Harness:   e.Harness,
		Source:    string(e.Source),
		Data:      data,
	})
	if err != nil {
		return e, err
	}
	e.Seq = seq
	e.Data = []byte(data)

	// the insert holds the write lock, so the event before this one can't change
	if e.PrevHash, err = t.q.PrevHash(ctx, seq); err != nil {
		return e, err
	}
	if e.Hash, err = event.ChainHash(e.PrevHash, e); err != nil {
		return e, err
	}
	err = t.q.SetHash(ctx, sqlc.SetHashParams{
		PrevHash: sql.NullString{String: e.PrevHash, Valid: true},
		Hash:     sql.NullString{String: e.Hash, Valid: true},
		Seq:      seq,
	})
	return e, err
}

func (t *Tx) SessionState(ctx context.Context, sessionID string) ([]byte, error) {
	data, err := t.q.GetSessionState(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return []byte(data), err
}

func (t *Tx) PutSessionState(ctx context.Context, sessionID string, data []byte, now time.Time) error {
	return t.q.PutSessionState(ctx, sqlc.PutSessionStateParams{
		SessionID: sessionID,
		Data:      string(data),
		UpdatedAt: now.UnixMilli(),
	})
}

func (s *Store) Salt(ctx context.Context) ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	v, err := s.meta(ctx, "salt", hex.EncodeToString(b))
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(v)
}

// random uuid naming this machine on the server so that seq numbers are separated per machine
func (s *Store) InstallID(ctx context.Context) (string, error) {
	return s.meta(ctx, "install_id", uuid.NewString())
}

// when the server's chain for this machine no longer matches, like after restoring an old
// backup or copying the db to another machine: a new install id, with the unsent events
// chained again from scratch. does nothing if the id isn't old anymore, another sync did it
func (s *Store) NewInstall(ctx context.Context, old string) error {
	return s.Update(ctx, func(tx *Tx) error {
		cur, err := tx.q.GetMeta(ctx, "install_id")
		if err != nil || cur != old {
			return err
		}
		if err := tx.q.SetMeta(ctx, sqlc.SetMetaParams{Key: "install_id", Value: uuid.NewString()}); err != nil {
			return err
		}
		rows, err := tx.q.UnsentEvents(ctx)
		if err != nil {
			return err
		}
		prev := ""
		for _, e := range fromRows(rows) {
			hash, err := event.ChainHash(prev, e)
			if err != nil {
				return err
			}
			err = tx.q.SetHash(ctx, sqlc.SetHashParams{
				PrevHash: sql.NullString{String: prev, Valid: true},
				Hash:     sql.NullString{String: hash, Valid: true},
				Seq:      e.Seq,
			})
			if err != nil {
				return err
			}
			prev = hash
		}
		return nil
	})
}

// value for key, "" if never set
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	v, err := s.q.GetMeta(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.q.SetMeta(ctx, sqlc.SetMetaParams{Key: key, Value: value})
}

// value for key, set to initial on first use
func (s *Store) meta(ctx context.Context, key, initial string) (string, error) {
	if err := s.q.SetMetaIfMissing(ctx, sqlc.SetMetaIfMissingParams{Key: key, Value: initial}); err != nil {
		return "", err
	}
	return s.q.GetMeta(ctx, key)
}

func (s *Store) Between(ctx context.Context, from, to time.Time) ([]event.Event, error) {
	rows, err := s.q.EventsBetween(ctx, sqlc.EventsBetweenParams{
		FromTs: from.UnixMilli(),
		ToTs:   to.UnixMilli(),
	})
	if err != nil {
		return nil, err
	}
	return fromRows(rows), nil
}

func (s *Store) CountSession(ctx context.Context, session string, source event.Source) (int64, error) {
	return s.q.CountSessionEvents(ctx, sqlc.CountSessionEventsParams{SessionID: session, Source: string(source)})
}

// pending count and when the last upload happened (zero if never)
func (s *Store) SyncStatus(ctx context.Context) (int64, time.Time, error) {
	r, err := s.q.SyncStatus(ctx)
	return r.Pending, unixMilli(r.LastUpload), err
}

// zero if the harness never sent anything from that source
func (s *Store) LastEventFrom(ctx context.Context, harness string, source event.Source) (time.Time, error) {
	ms, err := s.q.LastEventFrom(ctx, sqlc.LastEventFromParams{Harness: harness, Source: string(source)})
	return unixMilli(ms), err
}

func unixMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// events not uploaded yet, by seq
func (s *Store) Pending(ctx context.Context, limit int64) ([]event.Event, error) {
	rows, err := s.q.PendingEvents(ctx, limit)
	if err != nil {
		return nil, err
	}
	return fromRows(rows), nil
}

// everything gets sent again, e.g. to a new server
func (s *Store) MarkAllPending(ctx context.Context) error {
	return s.q.MarkAllPending(ctx)
}

// marks every event up to seq as sent
func (s *Store) MarkUploaded(ctx context.Context, seq int64, at time.Time) error {
	return s.q.MarkUploaded(ctx, sqlc.MarkUploadedParams{
		UploadedAt: sql.NullInt64{Int64: at.UnixMilli(), Valid: true},
		UpToSeq:    seq,
	})
}

func fromRows(rows []sqlc.Event) []event.Event {
	out := make([]event.Event, len(rows))
	for i, r := range rows {
		out[i] = event.Event{
			Seq:       r.Seq,
			TS:        time.UnixMilli(r.Ts).UTC(),
			Kind:      event.Kind(r.Kind),
			SessionID: r.SessionID,
			ProjectID: r.ProjectID,
			Harness:   r.Harness,
			Source:    event.Source(r.Source),
			Data:      []byte(r.Data),
			PrevHash:  r.PrevHash.String,
			Hash:      r.Hash.String,
		}
	}
	return out
}
