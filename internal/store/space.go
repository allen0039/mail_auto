package store

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Existing NONE databases need one VACUUM to create SQLite's pointer map.
// Subsequent deletes use bounded incremental reclamation, preserving row data
// and stable message refs. This does not change the application schema version.
func (s *Store) enableReclaim() error {
	var mode int
	if e := s.DB.QueryRow("PRAGMA auto_vacuum").Scan(&mode); e != nil {
		return e
	}
	if mode == 2 {
		return nil
	}
	if _, e := s.DB.Exec("PRAGMA auto_vacuum=INCREMENTAL"); e != nil {
		return e
	}
	if mode == 0 {
		if _, e := s.DB.Exec("VACUUM"); e != nil {
			return e
		}
	}
	if e := s.DB.QueryRow("PRAGMA auto_vacuum").Scan(&mode); e != nil {
		return e
	}
	if mode != 2 {
		return errors.New("database incremental reclamation unavailable")
	}
	return nil
}

func (s *Store) RequestReclaim() {
	select {
	case s.reclaim <- struct{}{}:
	default:
	}
}

type SpaceResult struct {
	PagesBefore int64
	PagesAfter  int64
	FreePages   int64
}

// Each pass moves at most 256 pages (normally 1 MiB). Drain the PRAGMA's rows:
// stepping just once can leave most of incremental_vacuum's work unexecuted.
func (s *Store) ReclaimSpace(ctx context.Context) (SpaceResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result := SpaceResult{}
	conn, e := s.DB.Conn(ctx)
	if e != nil {
		return result, e
	}
	defer conn.Close()
	if e = conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&result.PagesBefore); e != nil {
		return result, e
	}
	var free int64
	if e = conn.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); e != nil {
		return result, e
	}
	if free > 0 {
		rows, e := conn.QueryContext(ctx, "PRAGMA incremental_vacuum(256)")
		if e != nil {
			return result, e
		}
		for rows.Next() {
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return result, e
		}
	}
	if e = conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&result.PagesAfter); e != nil {
		return result, e
	}
	if e = conn.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&result.FreePages); e != nil {
		return result, e
	}
	var busy, logPages, checkpointed int
	if e = conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logPages, &checkpointed); e != nil {
		return result, e
	}
	// External readers can defer truncation; periodic maintenance retries it.
	if busy == 0 && result.FreePages > 0 && result.PagesAfter < result.PagesBefore {
		s.RequestReclaim()
	}
	return result, nil
}

// The serving process owns maintenance; backup/CLI store opens run no workers.
func (s *Store) Maintain(ctx context.Context) {
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	space := time.NewTicker(time.Minute)
	defer space.Stop()
	s.Cleanup(ctx)
	reclaim := func() {
		if _, e := s.ReclaimSpace(ctx); e != nil && ctx.Err() == nil {
			slog.Warn("数据库空间回收暂缓，将自动重试")
		}
	}
	reclaim()
	for {
		select {
		case <-ctx.Done():
			return
		case <-cleanup.C:
			s.Cleanup(ctx)
			reclaim()
		case <-space.C:
			reclaim()
		case <-s.reclaim:
			reclaim()
		}
	}
}
