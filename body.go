package llmotel

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

// maxJSONBody caps how much of a non-streaming response is buffered for
// parsing. Larger bodies still reach the caller intact; they are just not
// parsed for usage.
const maxJSONBody = 8 << 20

// maxSSELine caps a single buffered SSE line. Longer lines are skipped.
const maxSSELine = 1 << 20

// sink receives a copy of response bytes as the caller reads them.
type sink interface {
	write(p []byte)
	// finish is called once, at EOF or Close, with the accumulated result.
	finish(r *result)
}

// jsonSink buffers a complete JSON document.
type jsonSink struct {
	buf      bytes.Buffer
	overflow bool
}

func (s *jsonSink) write(p []byte) {
	if s.overflow {
		return
	}
	if s.buf.Len()+len(p) > maxJSONBody {
		s.overflow = true
		s.buf = bytes.Buffer{}
		return
	}
	s.buf.Write(p)
}

func (s *jsonSink) finish(r *result) {
	if !s.overflow {
		parseResponseJSON(s.buf.Bytes(), r)
	}
}

// sseSink parses Server-Sent Events incrementally, keeping only the current
// partial line in memory.
type sseSink struct {
	r    *result
	line []byte
	skip bool // current line exceeded maxSSELine
}

func (s *sseSink) write(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.buffer(p)
			return
		}
		s.buffer(p[:i])
		s.dispatch()
		p = p[i+1:]
	}
}

func (s *sseSink) buffer(p []byte) {
	if s.skip {
		return
	}
	if len(s.line)+len(p) > maxSSELine {
		s.skip, s.line = true, s.line[:0]
		return
	}
	s.line = append(s.line, p...)
}

func (s *sseSink) dispatch() {
	line := bytes.TrimSuffix(s.line, []byte("\r"))
	if !s.skip {
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			parseStreamEvent(data, s.r)
		}
	}
	s.line, s.skip = s.line[:0], false
}

func (s *sseSink) finish(*result) {
	if len(s.line) > 0 {
		s.dispatch() // final line without trailing newline
	}
}

// instrumentedBody tees a response body into a sink and calls done exactly
// once when the body hits EOF, fails, or is closed.
type instrumentedBody struct {
	rc   io.ReadCloser
	sink sink
	res  *result
	done func(r *result, readErr error)

	mu       sync.Mutex
	finished bool
}

func (b *instrumentedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return n, err
	}
	if n > 0 {
		b.sink.write(p[:n])
	}
	if err != nil {
		var readErr error
		if !errors.Is(err, io.EOF) {
			readErr = err
		}
		b.finishLocked(readErr)
	}
	return n, err
}

func (b *instrumentedBody) Close() error {
	err := b.rc.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finishLocked(nil)
	return err
}

func (b *instrumentedBody) finishLocked(readErr error) {
	if b.finished {
		return
	}
	b.finished = true
	b.sink.finish(b.res)
	b.done(b.res, readErr)
}
