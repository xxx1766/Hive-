package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// ErrClosed is returned by Reader/Writer after the underlying stream is closed.
var ErrClosed = errors.New("protocol: stream closed")

// Reader reads newline-delimited JSON messages from r.
// It is NOT safe for concurrent use.
type Reader struct {
	br *bufio.Reader
}

func NewReader(r io.Reader) *Reader {
	// MaxScanTokenSize default is 64KB; we use bufio.Reader + ReadBytes
	// which auto-grows, so we can handle larger messages (e.g. LLM responses).
	return &Reader{br: bufio.NewReaderSize(r, 64*1024)}
}

// Read blocks until a full \n-terminated message is available or the stream errors.
func (r *Reader) Read() (*Message, error) {
	line, err := r.br.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrClosed
		}
		return nil, err
	}
	m := &Message{}
	if err := json.Unmarshal(line, m); err != nil {
		return nil, err
	}
	return m, nil
}

// Writer writes newline-delimited JSON messages to w.
// Writes are serialized via an internal mutex so it is safe for concurrent use
// from multiple goroutines (important: the router fans multiple senders into
// one Agent's stdin).
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (w *Writer) Write(m *Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(b)
	return err
}
