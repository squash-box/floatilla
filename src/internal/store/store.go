// Package store persists tasks in Postgres. Every task change is written in
// the same transaction as the outbox messages it causes, so the database and
// the bus can't drift: a relay publishes the outbox afterwards.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
	"github.com/squash-box/floatilla/src/internal/fsm"
)

//go:embed schema.sql
var schema string

var ErrNotFound = errors.New("task not found")

// OutboxMsg is a message to publish after the transaction commits.
type OutboxMsg struct {
	Subject string
	Payload []byte
	// MsgID de-duplicates JetStream publishes; empty for none.
	MsgID string
	// Core publishes over core NATS instead of JetStream (fleet snapshots).
	Core bool
}

type Store struct {
	pool *pgxpool.Pool
	// kick wakes the outbox relay after a commit.
	kick chan struct{}
}

// Open connects (retrying until ctx ends) and applies the schema.
func Open(ctx context.Context, url string) (*Store, error) {
	var pool *pgxpool.Pool
	var err error
	for {
		pool, err = pgxpool.New(ctx, url)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect postgres: %w", err)
		case <-time.After(time.Second):
		}
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{pool: pool, kick: make(chan struct{}, 1)}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Kicked fires after a commit that wrote outbox messages.
func (s *Store) Kicked() <-chan struct{} { return s.kick }

var jsonOpts = protojson.MarshalOptions{UseProtoNames: true}

func encode(t *v1.Task) ([]byte, error) { return jsonOpts.Marshal(t) }

func decode(data []byte) (*v1.Task, error) {
	t := &v1.Task{}
	return t, protojson.Unmarshal(data, t)
}

// Create inserts a new task with its outbox messages.
func (s *Store) Create(ctx context.Context, t *v1.Task, out []OutboxMsg) error {
	return s.tx(ctx, out, func(tx pgx.Tx) error {
		data, err := encode(t)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`insert into tasks (id, status, process_type, finished, lease_expires_at, data, created_at, updated_at)
			 values ($1, $2, $3, $4, $5, $6, $7, $7)`,
			t.Id, t.Status.String(), t.ProcessType, fsm.IsTerminal(t.Status), leaseOf(t), data, t.CreatedAt.AsTime())
		return err
	})
}

// UpdateFunc computes the next task and the messages it causes. Returning
// the same pointer it was given (or nil messages and an unchanged task) is a
// no-op.
type UpdateFunc func(cur *v1.Task) (next *v1.Task, out []OutboxMsg, err error)

// Update applies fn to the task under a row lock.
func (s *Store) Update(ctx context.Context, id string, fn UpdateFunc) (*v1.Task, error) {
	var result *v1.Task
	var out []OutboxMsg
	err := s.tx(ctx, nil, func(tx pgx.Tx) error {
		var data []byte
		err := tx.QueryRow(ctx, `select data from tasks where id = $1 for update`, id).Scan(&data)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		cur, err := decode(data)
		if err != nil {
			return err
		}
		next, msgs, err := fn(cur)
		if err != nil {
			return err
		}
		result = next
		if next == cur {
			return nil
		}
		if data, err = encode(next); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx,
			`update tasks set status = $2, finished = $3, lease_expires_at = $4, data = $5, updated_at = now() where id = $1`,
			id, next.Status.String(), fsm.IsTerminal(next.Status), leaseOf(next), data); err != nil {
			return err
		}
		out = msgs
		return insertOutbox(ctx, tx, msgs)
	})
	if err == nil && len(out) > 0 {
		s.wake()
	}
	return result, err
}

func (s *Store) Get(ctx context.Context, id string) (*v1.Task, error) {
	var data []byte
	err := s.pool.QueryRow(ctx, `select data from tasks where id = $1`, id).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decode(data)
}

// List returns tasks in creation order. Finished tasks are included when
// includeFinished is set.
func (s *Store) List(ctx context.Context, includeFinished bool) ([]*v1.Task, error) {
	rows, err := s.pool.Query(ctx,
		`select data from tasks where $1 or not finished order by created_at, id`, includeFinished)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*v1.Task
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		t, err := decode(data)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ExpiredLeases returns ids of tasks whose run lease lapsed before now.
func (s *Store) ExpiredLeases(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`select id from tasks where not finished and lease_expires_at < $1`, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// PublishFunc delivers one outbox message.
type PublishFunc func(ctx context.Context, m OutboxMsg) error

// RelayOutbox publishes up to limit pending messages in order and deletes
// them. It stops at the first failure so ordering is preserved; the failed
// message is retried on the next call.
func (s *Store) RelayOutbox(ctx context.Context, limit int, publish PublishFunc) (int, error) {
	sent := 0
	err := s.tx(ctx, nil, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`select id, subject, payload, msg_id, core from outbox order by id limit $1 for update skip locked`, limit)
		if err != nil {
			return err
		}
		type row struct {
			id int64
			m  OutboxMsg
		}
		var pending []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.m.Subject, &r.m.Payload, &r.m.MsgID, &r.m.Core); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var lastErr error
		var done []int64
		for _, r := range pending {
			if lastErr = publish(ctx, r.m); lastErr != nil {
				break
			}
			done = append(done, r.id)
		}
		if len(done) > 0 {
			if _, err := tx.Exec(ctx, `delete from outbox where id = any($1)`, done); err != nil {
				return err
			}
		}
		sent = len(done)
		if lastErr != nil && sent == 0 {
			return lastErr
		}
		return nil
	})
	return sent, err
}

func (s *Store) tx(ctx context.Context, out []OutboxMsg, fn func(pgx.Tx) error) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, out)
	})
	if err == nil && len(out) > 0 {
		s.wake()
	}
	return err
}

func insertOutbox(ctx context.Context, tx pgx.Tx, msgs []OutboxMsg) error {
	for _, m := range msgs {
		if _, err := tx.Exec(ctx,
			`insert into outbox (subject, payload, msg_id, core) values ($1, $2, $3, $4)`,
			m.Subject, m.Payload, m.MsgID, m.Core); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func leaseOf(t *v1.Task) *time.Time {
	if t.LeaseExpiresAt == nil {
		return nil
	}
	ts := t.LeaseExpiresAt.AsTime()
	return &ts
}
