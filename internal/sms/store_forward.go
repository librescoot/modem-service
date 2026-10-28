package sms

import (
	"database/sql"
	"time"
)

type ForwardJob struct {
	ID       string
	To       string
	Text     string
	Segments int
}

// RecoverForward marks a send interrupted by a crash as unknown, never queued.
func (s *Store) RecoverForward() error {
	_, err := s.db.Exec(`UPDATE forward_jobs SET state='unknown' WHERE state='sending'`)
	return err
}

func (s *Store) ConfigureForward(destination string, now time.Time) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var previous string
	var activatedAt int64
	if err = tx.QueryRow(`SELECT destination,activated_at FROM forward_config WHERE id=1`).Scan(&previous, &activatedAt); err != nil {
		return 0, err
	}
	if previous != destination {
		activatedAt = now.Truncate(time.Second).UnixMilli()
		if _, err = tx.Exec(`UPDATE forward_config SET destination=?,activated_at=? WHERE id=1`, destination, activatedAt); err != nil {
			return 0, err
		}
	}
	if _, err = tx.Exec(`UPDATE forward_jobs SET state='cancelled' WHERE state='queued' AND destination<>?`, destination); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return activatedAt, nil
}

// ReserveForward commits both the attempt and its daily segment budget before
// any modem call. An interrupted or failed send cannot be silently retried.
func (s *Store) ReserveForward(now time.Time, destination, own string) (*ForwardJob, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM forward_jobs WHERE created_at < ? AND state <> 'sending'`, now.Add(-inboxMaxAge).UnixMilli()); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE forward_jobs SET state='expired' WHERE state='queued' AND expires_at<=?`, now.UnixMilli()); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE forward_jobs SET state='cancelled' WHERE state='queued' AND destination<>?`, destination); err != nil {
		return nil, err
	}
	if destination == "off" || !ValidForwardDestination(destination) || (own != "" && own == destination) {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if own != "" {
		if _, err = tx.Exec(`UPDATE forward_jobs SET state='cancelled' WHERE state='queued' AND sender=?`, own); err != nil {
			return nil, err
		}
	}
	day := now.UTC().Format("2006-01-02")
	var spent int
	err = tx.QueryRow(`SELECT segments FROM forward_budget WHERE day=?`, day).Scan(&spent)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	var job ForwardJob
	err = tx.QueryRow(`SELECT id,destination,body,segments FROM forward_jobs WHERE state='queued' ORDER BY created_at,id LIMIT 1`).Scan(&job.ID, &job.To, &job.Text, &job.Segments)
	if err == sql.ErrNoRows {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if spent+job.Segments > 10 {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if _, err = tx.Exec(`UPDATE forward_jobs SET state='sending',budget_day=? WHERE id=? AND state='queued'`, day, job.ID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO forward_budget(day,segments) VALUES(?,?) ON CONFLICT(day) DO UPDATE SET segments=segments+excluded.segments`, day, job.Segments); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Store) FinishForward(id string, sent bool) error {
	state := "unknown"
	if sent {
		state = "sent"
	}
	_, err := s.db.Exec(`UPDATE forward_jobs SET state=? WHERE id=? AND state='sending'`, state, id)
	return err
}
