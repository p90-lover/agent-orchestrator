package persistenthost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

const maxACPJournalBytes int64 = 256 << 20

// acpJournalMemoryBytes is how much of one prompt's journal is held in memory
// before it moves to its file. Every provider notification of an active prompt
// passes through the journal, and writing each one to disk cost the HDD a write
// per streamed token chunk; a whole turn is typically well under 2 MB.
const acpJournalMemoryBytes int64 = 16 << 20

var errACPJournalFull = errors.New("persistent ACP prompt journal is full")

// acpPromptJournal is a bounded, host-owned WAL for provider notifications
// emitted during one active prompt. It is intentionally separate from AO's
// SQLite projection: the host appends provider bytes before delivery, while the
// daemon owns semantic normalization and transactional deduplication.
//
// It replays to a daemon that reattaches to this living host, so it only has to
// outlive the daemon, not the host: a new host truncates whatever an old one
// left. That is why it starts in memory and only uses its file once a prompt
// outgrows memLimit, until the next reset.
type acpPromptJournal struct {
	path     string
	memLimit int64
	mem      []byte   // the journal while it fits in memLimit
	file     *os.File // the whole journal once it outgrew memLimit
	size     int64
}

type acpJournalSpan struct {
	offset int64
	length int64
}

func openACPPromptJournal(ctx context.Context, path string) (*acpPromptJournal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A previous host's journal holds provider output nobody can replay any more.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale persistent ACP prompt journal: %w", err)
	}
	return &acpPromptJournal{path: path, memLimit: acpJournalMemoryBytes}, nil
}

func (j *acpPromptJournal) append(ctx context.Context, frame []byte) (acpJournalSpan, error) {
	if err := ctx.Err(); err != nil {
		return acpJournalSpan{}, err
	}
	if int64(len(frame)) > maxACPJournalBytes-j.size {
		return acpJournalSpan{}, fmt.Errorf("%w: limit=%d", errACPJournalFull, maxACPJournalBytes)
	}
	span := acpJournalSpan{offset: j.size, length: int64(len(frame))}
	if j.file == nil && j.size+int64(len(frame)) <= j.memLimit {
		j.mem = append(j.mem, frame...)
		j.size += int64(len(frame))
		return span, nil
	}
	if j.file == nil {
		if err := j.spill(); err != nil {
			return acpJournalSpan{}, err
		}
	}
	written, err := j.file.Write(frame)
	j.size += int64(written)
	if err != nil {
		return acpJournalSpan{}, fmt.Errorf("append persistent ACP prompt journal: %w", err)
	}
	if written != len(frame) {
		return acpJournalSpan{}, io.ErrShortWrite
	}
	return span, nil
}

// spill moves the in-memory journal into its file, which then holds every
// byte from offset zero so spans keep their meaning.
func (j *acpPromptJournal) spill() error {
	file, err := os.OpenFile(j.path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open persistent ACP prompt journal: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure persistent ACP prompt journal: %w", err)
	}
	if _, err := file.Write(j.mem); err != nil {
		_ = file.Close()
		return fmt.Errorf("append persistent ACP prompt journal: %w", err)
	}
	j.file, j.mem = file, nil
	return nil
}

func (j *acpPromptJournal) replayTo(ctx context.Context, dst io.Writer, span acpJournalSpan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if span.length == 0 {
		return nil
	}
	if span.offset < 0 || span.length < 0 || span.offset+span.length > j.size {
		return errors.New("invalid persistent ACP prompt journal span")
	}
	if j.file == nil {
		_, err := io.Copy(dst, bytes.NewReader(j.mem[span.offset:span.offset+span.length]))
		return err
	}
	_, err := io.Copy(dst, io.NewSectionReader(j.file, span.offset, span.length))
	return err
}

func (j *acpPromptJournal) reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	j.size = 0
	// Keep a small buffer for the next prompt; drop a large one.
	if int64(cap(j.mem)) > 1<<20 {
		j.mem = nil
	}
	j.mem = j.mem[:0]
	if j.file == nil {
		return nil
	}
	file := j.file
	j.file = nil
	if err := errors.Join(file.Close(), os.Remove(j.path)); err != nil {
		return fmt.Errorf("discard persistent ACP prompt journal: %w", err)
	}
	return nil
}

func (j *acpPromptJournal) close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	j.mem = nil
	var closeErr error
	if j.file != nil {
		closeErr = j.file.Close()
		j.file = nil
	}
	removeErr := os.Remove(j.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}
