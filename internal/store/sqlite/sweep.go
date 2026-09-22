package sqlite

import (
	"context"
	"fmt"

	"github.com/duskrun/duskrun/internal/core"
)

// InsertSweep records a completed retention sweep and returns its id.
func (s *Store) InsertSweep(ctx context.Context, sw core.Sweep) (int64, error) {
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO retention_sweep
		        (started_at, finished_at, status, source, deleted_count, freed_bytes, orphan_count, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sw.StartedAt.Unix(), sw.FinishedAt.Unix(), string(sw.Status), string(sw.Source),
		sw.DeletedCount, sw.FreedBytes, sw.OrphanCount, sw.Error,
	)
	if err != nil {
		return 0, fmt.Errorf("insert retention sweep: %w", err)
	}
	return res.LastInsertId()
}

// ListSweeps returns the most recent sweeps, newest first. limit <= 0 falls back
// to defaultSweepLimit.
func (s *Store) ListSweeps(ctx context.Context, limit int) ([]core.Sweep, error) {
	if limit <= 0 {
		limit = defaultSweepLimit
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, started_at, finished_at, status, source, deleted_count, freed_bytes, orphan_count, error
		   FROM retention_sweep
		  ORDER BY started_at DESC, id DESC
		  LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list retention sweeps: %w", err)
	}
	defer rows.Close()

	var out []core.Sweep
	for rows.Next() {
		var (
			sw              core.Sweep
			started, finish int64
			status, source  string
		)
		if err := rows.Scan(&sw.ID, &started, &finish, &status, &source,
			&sw.DeletedCount, &sw.FreedBytes, &sw.OrphanCount, &sw.Error); err != nil {
			return nil, fmt.Errorf("scan retention sweep: %w", err)
		}
		sw.StartedAt = unixToTime(started)
		sw.FinishedAt = unixToTime(finish)
		sw.Status = core.SweepStatus(status)
		sw.Source = core.SweepSource(source)
		out = append(out, sw)
	}
	return out, rows.Err()
}

// ArtifactStats is the per-task catalog footprint: how many artifacts a task
// currently holds and how many bytes they occupy.
type ArtifactStats struct {
	Count int
	Bytes int64
}

// ArtifactStatsByTask aggregates the catalog per task in one query, so the
// retention page does not need an artifact listing per task.
func (s *Store) ArtifactStatsByTask(ctx context.Context) (map[int64]ArtifactStats, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT r.task_id, COUNT(*), COALESCE(SUM(a.size), 0)
		   FROM artifact a
		   JOIN run r ON r.id = a.run_id
		  GROUP BY r.task_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("artifact stats by task: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]ArtifactStats)
	for rows.Next() {
		var (
			taskID int64
			st     ArtifactStats
		)
		if err := rows.Scan(&taskID, &st.Count, &st.Bytes); err != nil {
			return nil, fmt.Errorf("scan artifact stats: %w", err)
		}
		out[taskID] = st
	}
	return out, rows.Err()
}

// defaultSweepLimit bounds an unqualified history listing.
const defaultSweepLimit = 50
