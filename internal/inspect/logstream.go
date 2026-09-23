package inspect

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/runtime"
)

// This file is the streaming half of the log answer: `Logs` returns a tail,
// `FollowLogs` keeps going.
//
// The trail it follows is the copy SRCOS itself wrote (the record's LogPath,
// outside the workspace), so a tool cannot rewrite its own history — the same
// property that makes the one-shot answer trustworthy.
//
// It is deliberately transport-agnostic: no HTTP, no SSE, no channel. The
// front-end decides how to frame an event (the gateway turns it into
// `text/event-stream`), and the loop stays a single function with a single
// owner, which is what makes cancellation and shutdown easy to reason about.

// LogEventKind classifies one event of a followed log.
type LogEventKind string

const (
	// LogEventLine carries one line of output.
	LogEventLine LogEventKind = "line"
	// LogEventPing carries no data: it exists because a service can be silent
	// for hours and a proxy that sees no bytes will drop the connection.
	LogEventPing LogEventKind = "ping"
	// LogEventEnd is the last event. State is the instance's terminal state
	// when the stream ended because the work finished; Note explains an end
	// that happened for another reason.
	LogEventEnd LogEventKind = "end"
)

// LogEvent is one event of a followed log.
type LogEvent struct {
	Kind  LogEventKind
	Line  string
	State string
	Note  string
}

// defaultFollowInterval is how often the follower looks for new bytes and
// re-reads the instance record.
//
// Polling (rather than a filesystem watch) trades a little latency for a lot of
// robustness: it survives append, truncation, and a record another process
// rewrites, none of which a watch would handle without extra state. A Reader
// may override it (FollowInterval) so a test does not have to wait a second per
// observation.
const defaultFollowInterval = time.Second

const (
	// followPing is how long the stream may stay silent before a keep-alive.
	followPing = 15 * time.Second
	// maxFollowLine caps one line: a tool that prints a megabyte without a
	// newline must not make the buffer grow without bound.
	maxFollowLine = 16 * 1024
	// followTailBytes bounds how much of the file is read to find the last N
	// lines of history.
	followTailBytes = 8 << 20
	// maxReadChunk bounds one read. New data is drained in chunks of this size,
	// so a firehose of output costs bounded memory per iteration rather than one
	// allocation the size of the backlog.
	maxReadChunk = 1 << 20
	// maxChunksPerTick bounds how many chunks one poll drains before yielding to
	// the terminal check and the context. 128 MiB in one tick is far beyond any
	// log a tool should produce, and the next tick continues where this stopped.
	maxChunksPerTick = 128
)

// FollowLogs replays an instance's log tail and then streams new lines as they
// appear, returning when the instance reaches a terminal state or ctx ends.
//
// It reads the copy SRCOS wrote. If the log file does not exist yet (a task
// that has been submitted but not started) the stream simply waits: the work
// may still begin, and ending early would be a lie about the instance.
//
// The returned error is a setup failure (unknown instance, ambiguous needle)
// and is only possible before the first event — after that, an abnormal end is
// reported as a final event with Note set, because the stream is already open
// and a bare error would be invisible to the client.
func (r *Reader) FollowLogs(ctx context.Context, username, needle string, tail int, emit func(LogEvent)) error {
	rec, _, err := r.record(username, needle)
	if err != nil {
		return err
	}
	if tail <= 0 {
		tail = DefaultLogTail
	}
	if tail > MaxLogTail {
		tail = MaxLogTail
	}
	recordPath := runtime.InstancePath(r.ConfigDir, rec.ID)

	var (
		f       *os.File
		offset  int64
		started bool
		split   lineSplitter
		last    = time.Now()
	)
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	sendLine := func(line string) {
		emit(LogEvent{Kind: LogEventLine, Line: line})
		last = time.Now()
	}

	for {
		if f == nil {
			if h, err := os.Open(rec.LogPath); err == nil {
				f = h
			}
		}
		if f != nil && !started {
			off, err := replayTail(f, tail, sendLine)
			if err != nil {
				return err
			}
			offset = off
			started = true
		}
		if f != nil {
			offset = readNewBytes(f, offset, &split, sendLine)
		}

		cur, err := runtime.LoadInstance(recordPath)
		switch {
		case err != nil:
			// The record vanished (deleted by hand, or a reconcile decided the
			// instance never was). Say so rather than hanging forever.
			split.flush(sendLine)
			emit(LogEvent{Kind: LogEventEnd, Note: "the instance record is gone"})
			return nil

		case cur.State.Terminal():
			// One more read before closing: a backend that writes the record
			// before flushing the last output must not lose the tail.
			if f != nil {
				offset = readNewBytes(f, offset, &split, sendLine)
			}
			split.flush(sendLine)
			emit(LogEvent{Kind: LogEventEnd, State: string(cur.State)})
			return nil
		}

		if time.Since(last) >= followPing {
			emit(LogEvent{Kind: LogEventPing})
			last = time.Now()
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.followTick()):
		}
	}
}

// followTick is the poll interval to use.
func (r *Reader) followTick() time.Duration {
	if r.FollowInterval > 0 {
		return r.FollowInterval
	}
	return defaultFollowInterval
}

// replayTail emits the last n complete lines already in the file and returns the
// offset the live reads should continue from, reading at most followTailBytes
// from the end so a huge log does not have to be read to show its tail.
//
// The returned offset is *not* the file size: a file that ends mid-line ends in
// a line that is not finished, and that fragment is left for the live path (or
// for the final flush when the instance ends). Emitting it here would also
// split it, because the continuation would arrive as a line of its own.
func replayTail(f *os.File, n int, emit func(string)) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := fi.Size()
	start := int64(0)
	if size > followTailBytes {
		start = size - followTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(f, followTailBytes+1))
	if err != nil {
		return 0, err
	}

	// A window that did not begin at byte 0 may begin mid-line; drop the
	// fragment, whose beginning we cannot see.
	begin := 0
	if start > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			// One unfinished line spans the whole window.
			return start, nil
		}
		begin = i + 1
	}
	rest := data[begin:]
	resume := start + int64(begin)
	complete := []byte(nil)
	if cut := bytes.LastIndexByte(rest, '\n'); cut >= 0 {
		complete = rest[:cut+1]
		resume = start + int64(begin) + int64(cut) + 1
	}

	lines := strings.Split(string(complete), "\n")
	// The trailing newline leaves an empty element; it is not a line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		emit(sanitizeLine(line))
	}
	return resume, nil
}

// readNewBytes drains whatever the file has appended since offset and returns
// the new offset. A file that shrank (truncated and rewritten) restarts from 0.
//
// It loops until it reaches EOF so a fast producer is followed rather than
// lagged, reading in bounded chunks so the memory cost does not scale with the
// backlog.
func readNewBytes(f *os.File, offset int64, split *lineSplitter, emit func(string)) int64 {
	for i := 0; i < maxChunksPerTick; i++ {
		fi, err := f.Stat()
		if err != nil {
			return offset
		}
		size := fi.Size()
		if size < offset {
			offset = 0
			split.reset()
		}
		if size == offset {
			return offset
		}
		want := size - offset
		if want > maxReadChunk {
			want = maxReadChunk
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return offset
		}
		buf := make([]byte, want)
		n, err := io.ReadFull(f, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return offset
		}
		if n == 0 {
			return offset
		}
		split.write(buf[:n], emit)
		offset += int64(n)
	}
	return offset
}

// lineSplitter turns a byte stream into lines, holding back the unterminated
// tail until its newline arrives.
type lineSplitter struct {
	buf []byte
}

func (s *lineSplitter) write(chunk []byte, emit func(string)) {
	s.buf = append(s.buf, chunk...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		emit(sanitizeLine(string(s.buf[:i])))
		s.buf = s.buf[i+1:]
		if len(s.buf) == 0 {
			// Reclaim the array once it is empty, so a long stream does not
			// keep a large buffer alive through forward slicing.
			s.buf = nil
		}
	}
	if len(s.buf) > maxFollowLine {
		emit(sanitizeLine(string(s.buf[:maxFollowLine])) + " …")
		s.buf = nil
	}
}

func (s *lineSplitter) flush(emit func(string)) {
	if len(s.buf) == 0 {
		return
	}
	emit(sanitizeLine(string(s.buf)))
	s.buf = nil
}

func (s *lineSplitter) reset() { s.buf = nil }

// sanitizeLine drops the carriage returns a progress bar leaves behind (SSE
// data must not contain a bare CR) and any NUL bytes.
func sanitizeLine(line string) string {
	// Keep the visible text of a \r-overwritten line: the last segment is what
	// a terminal would show. This also removes a trailing \r.
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	if strings.IndexByte(line, 0) >= 0 {
		line = strings.ReplaceAll(line, "\x00", "")
	}
	return line
}
