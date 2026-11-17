package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const maxACPFrameSize = 10 * 1024 * 1024

var errACPFrameTooLarge = errors.New("ACP frame exceeds 10 MiB limit")

// extensionMethodReader aliases explicitly configured legacy method names into
// ACP's underscore extension namespace, and renames a session/update that
// carries an async task to asyncTaskUpdateMethod (see async_turns.go). The SDK
// continues to own every standard method and all JSON-RPC lifecycle behavior.
type extensionMethodReader struct {
	reader     *bufio.Reader
	aliases    map[string]string
	asyncTasks *asyncTaskBarrier
	buffered   []byte
	pendingErr error
	maxFrame   int
}

func newExtensionMethodReader(reader io.Reader, aliases map[string]string) io.Reader {
	return newConversationReader(reader, aliases, nil)
}

// newConversationReader is the agent's output as the SDK reads it. asyncTasks,
// when set, counts each async task frame it renames.
func newConversationReader(reader io.Reader, aliases map[string]string, asyncTasks *asyncTaskBarrier) io.Reader {
	if len(aliases) == 0 && asyncTasks == nil {
		return reader
	}
	cloned := make(map[string]string, len(aliases))
	for method, alias := range aliases {
		cloned[method] = alias
	}
	return &extensionMethodReader{
		reader: bufio.NewReader(reader), aliases: cloned, asyncTasks: asyncTasks, maxFrame: maxACPFrameSize,
	}
}

func (r *extensionMethodReader) Read(dst []byte) (int, error) {
	if len(r.buffered) == 0 {
		line, err := r.readFrame()
		if len(line) == 0 {
			return 0, err
		}
		r.buffered = r.rewrite(line)
		r.pendingErr = err
	}
	n := copy(dst, r.buffered)
	r.buffered = r.buffered[n:]
	if len(r.buffered) == 0 && r.pendingErr != nil {
		err := r.pendingErr
		r.pendingErr = nil
		return n, err
	}
	return n, nil
}

func (r *extensionMethodReader) readFrame() ([]byte, error) {
	maxFrame := r.maxFrame
	if maxFrame <= 0 {
		maxFrame = maxACPFrameSize
	}
	frame := make([]byte, 0, min(maxFrame, r.reader.Size()))
	for {
		fragment, err := r.reader.ReadSlice('\n')
		if len(frame)+len(fragment) > maxFrame {
			return nil, errACPFrameTooLarge
		}
		frame = append(frame, fragment...)
		switch {
		case err == nil:
			return frame, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return frame, err
		}
	}
}

func (r *extensionMethodReader) rewrite(line []byte) []byte {
	// Most frames are neither: skip decoding them.
	asyncCandidate := r.asyncTasks != nil && hasAsyncTaskMarker(line)
	if len(r.aliases) == 0 && !asyncCandidate {
		return line
	}
	trimmed := bytes.TrimSpace(line)
	var envelope map[string]json.RawMessage
	if json.Unmarshal(trimmed, &envelope) != nil {
		return line
	}
	var method string
	if json.Unmarshal(envelope["method"], &method) != nil {
		return line
	}
	if asyncCandidate {
		if renamed, ok := rewriteAsyncTaskFrame(envelope, method); ok {
			r.asyncTasks.noteSeen()
			return renamed
		}
	}
	alias, ok := r.aliases[method]
	if !ok {
		return line
	}
	encodedMethod, _ := json.Marshal(alias)
	envelope["method"] = encodedMethod
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return line
	}
	return append(encoded, '\n')
}
