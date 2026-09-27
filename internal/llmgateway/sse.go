package llmgateway

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// errEventTooLarge ends a stream whose event exceeds the gateway's bound.
var errEventTooLarge = errors.New("llmgateway: stream event too large")

// event is one server-sent event: its bytes as received (through the blank
// line that ends it) and its data lines joined.
type event struct {
	raw  []byte
	data []byte
}

// eventReader splits a text/event-stream into events of at most max bytes.
type eventReader struct {
	br  *bufio.Reader
	max int
}

func newEventReader(r io.Reader, max int) *eventReader {
	return &eventReader{br: bufio.NewReaderSize(r, 64<<10), max: max}
}

// next returns the next complete event. At the end of the stream it returns
// io.EOF, or io.ErrUnexpectedEOF when an event was left unfinished.
func (e *eventReader) next() (event, error) {
	var ev event
	var data [][]byte
	for {
		line, err := e.line(len(ev.raw))
		ev.raw = append(ev.raw, line...)
		if err != nil {
			if errors.Is(err, io.EOF) && len(ev.raw) > 0 {
				return event{}, io.ErrUnexpectedEOF
			}
			return event{}, err
		}
		content := bytes.TrimRight(line, "\r\n")
		if len(content) == 0 {
			ev.data = bytes.Join(data, []byte("\n"))
			return ev, nil
		}
		if v, ok := bytes.CutPrefix(content, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(v, []byte(" ")))
		}
	}
}

// line reads one line (with its end), refusing to grow an event of used
// bytes past max.
func (e *eventReader) line(used int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := e.br.ReadSlice('\n')
		if used+len(out)+len(chunk) > e.max {
			return nil, errEventTooLarge
		}
		out = append(out, chunk...)
		switch {
		case err == nil:
			return out, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(out) > 0:
			return out, io.ErrUnexpectedEOF
		default:
			return out, err
		}
	}
}
