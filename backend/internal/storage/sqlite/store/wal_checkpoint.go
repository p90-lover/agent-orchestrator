package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

// Checkpoints run here, never inside a write.
//
// By default SQLite copies the write-ahead log into the database inside the
// commit that takes the log past 1000 frames, and it counts every frame in the
// log, not only the new ones. After a restart that commit is the daemon's first
// write, which runs before it listens. On 2026-11-11 ao.db-wal held 483 MB: the
// first write copied all of it on a busy hard disk (a 78 MB log took 288 s there,
// against 0.4 s with the copy turned off), the launcher gave up on the silent
// daemon and killed it, and the next daemon started the copy over: five starts
// and about 25 minutes before AO answered. While AO runs, the same copy ran
// inside whichever chat write crossed the threshold, holding the single writer.
//
// Open turns that off (wal_autocheckpoint(0)) and the daemon runs CheckpointWAL.
const (
	// walTruncateWait is how long a truncation waits for the readers still on
	// the log. It holds the writer meanwhile, so it stays under the writer
	// report threshold (WatchWriter).
	walTruncateWait = 2 * time.Second
	// walCheckpointSlow is how long a pass takes before it is reported.
	walCheckpointSlow = 2 * time.Second
	// walCheckpointErrorGap keeps a failing checkpoint to one report a minute.
	walCheckpointErrorGap = time.Minute
)

// UseCheckpointConnection gives CheckpointWAL a connection of its own, so a long
// copy (a backlog after a restart) does not take one of the readers. Without
// one it borrows a reader for each pass. Close closes it.
func (s *Store) UseCheckpointConnection(db *sql.DB) {
	s.checkpointDB = db
}

// CheckpointWAL copies the write-ahead log into the database in the background:
// at once, then every `every`, until ctx ends.
//
// A pass is a PASSIVE checkpoint on the checkpoint connection. It takes no
// writer lock and waits for nobody: writes go on appending to the log, and
// frames that a running read still needs are left for a later pass.
//
// SQLite cuts the log file back (journal_size_limit) only when a write restarts
// the log, which needs every frame copied and no read still on the log. With
// chats committing every second and reads overlapping, that moment can fail to
// come for hours: the log grew about 7 MB a minute past 390 MB. So once a pass
// has copied every frame and the file at walPath is still over truncateAbove
// bytes, the pass takes the writer for a TRUNCATE checkpoint. Nothing is left to
// copy by then; it waits at most walTruncateWait for the reads still on the log,
// then empties it. If the reads outlast that, the next pass tries again.
func (s *Store) CheckpointWAL(ctx context.Context, logger *slog.Logger, walPath string, truncateAbove int64, every time.Duration) {
	if every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var lastError time.Time
	for {
		started := time.Now()
		pass, err := s.checkpointWAL(ctx, walPath, truncateAbove)
		if ctx.Err() != nil {
			return
		}
		took := time.Since(started).Round(time.Millisecond)
		if logger != nil {
			switch {
			case err != nil:
				if time.Since(lastError) >= walCheckpointErrorGap {
					lastError = time.Now()
					logger.Warn("sqlite write-ahead log checkpoint failed", "error", err, "took", took)
				}
			case pass.truncated:
				logger.Info("sqlite write-ahead log truncated", "walBytes", pass.walBytes, "frames", pass.frames, "took", took)
			case took >= walCheckpointSlow:
				logger.Info("sqlite write-ahead log checkpointed", "frames", pass.frames, "copied", pass.copied, "walBytes", pass.walBytes, "took", took)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// walCheckpointPass is what one pass did.
type walCheckpointPass struct {
	// frames is how many frames the log held, copied how many of them are in
	// the database after the PASSIVE checkpoint.
	frames, copied int64
	// walBytes is the log file's size when the pass looked at it, truncated
	// whether it was emptied.
	walBytes  int64
	truncated bool
}

func (s *Store) checkpointWAL(ctx context.Context, walPath string, truncateAbove int64) (walCheckpointPass, error) {
	var pass walCheckpointPass
	db := s.checkpointDB
	if db == nil {
		db = s.readDB
	}
	var busy int64
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &pass.frames, &pass.copied); err != nil {
		return pass, fmt.Errorf("passive checkpoint: %w", err)
	}
	// busy: another checkpoint (a truncation, a closing connection) has the log.
	if busy != 0 || pass.copied < pass.frames || walPath == "" || truncateAbove <= 0 {
		return pass, nil
	}
	info, err := os.Stat(walPath)
	if errors.Is(err, fs.ErrNotExist) {
		return pass, nil
	}
	if err != nil {
		return pass, err
	}
	pass.walBytes = info.Size()
	if pass.walBytes <= truncateAbove {
		return pass, nil
	}
	pass.truncated, err = s.truncateWAL(ctx)
	return pass, err
}

// truncateWAL empties a log whose frames are all in the database. It runs on
// the writer, under writeMu, so AO's writes queue behind it the way they queue
// behind any write, for at most walTruncateWait plus the copy of what was
// committed since the passive pass.
func (s *Store) truncateWAL(ctx context.Context) (bool, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	conn, err := s.writeDB.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	// TRUNCATE waits for the readers through the busy handler: a short wait for
	// this checkpoint only, then the writer's own timeout again.
	var timeout int64
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", walTruncateWait.Milliseconds())); err != nil {
		return false, err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout = %d", timeout))
	}()
	var busy, frames, copied int64
	if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &copied); err != nil {
		return false, fmt.Errorf("truncate checkpoint: %w", err)
	}
	return busy == 0, nil
}
