package proto

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Frame types.
const (
	FrameBridge  = "bridge"
	FrameHello   = "hello"
	FrameWelcome = "welcome"
	FrameReq     = "req"
	FrameRes     = "res"
	FrameEvent   = "evt"
)

// MaxFrame bounds one encoded frame, newline included.
const MaxFrame = 4 << 20

// ErrFrameTooLarge means a frame exceeded MaxFrame.
var ErrFrameTooLarge = errors.New("frame larger than 4 MiB")

// Frame is one line on the wire. Which fields are set depends on T; see the
// package documentation for the key names.
type Frame struct {
	T  string `json:"t"`
	ID uint64 `json:"id,omitempty"`

	Op   string          `json:"op,omitempty"`
	Args json.RawMessage `json:"a,omitempty"`
	OK   json.RawMessage `json:"ok,omitempty"`
	Err  *Error          `json:"err,omitempty"`

	Version string `json:"v,omitempty"`
	Proto   int    `json:"p,omitempty"`
	Min     int    `json:"min,omitempty"`
	Max     int    `json:"max,omitempty"`
	Project string `json:"proj,omitempty"`
	Session string `json:"s,omitempty"`
	Machine string `json:"m,omitempty"`
	Latest  string `json:"latest,omitempty"`

	Principal string `json:"pr,omitempty"`
}

// Encoder writes frames. It is safe for concurrent use.
type Encoder struct {
	mu sync.Mutex
	w  io.Writer
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Encode writes f as one line.
func (e *Encoder) Encode(f *Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode %s frame: %w", f.T, err)
	}
	if len(b)+1 > MaxFrame {
		return fmt.Errorf("encode %s frame: %w", f.T, ErrFrameTooLarge)
	}
	b = append(b, '\n')
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(b); err != nil {
		return fmt.Errorf("write %s frame: %w", f.T, err)
	}
	return nil
}

// Decoder reads frames. It is not safe for concurrent use.
type Decoder struct {
	s *bufio.Scanner
}

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader) *Decoder {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64<<10), MaxFrame)
	return &Decoder{s: s}
}

// Decode reads the next frame. It returns io.EOF at a clean end of stream.
func (d *Decoder) Decode() (*Frame, error) {
	if !d.s.Scan() {
		err := d.s.Err()
		switch {
		case err == nil:
			return nil, io.EOF
		case errors.Is(err, bufio.ErrTooLong):
			return nil, ErrFrameTooLarge
		default:
			return nil, fmt.Errorf("read frame: %w", err)
		}
	}
	var f Frame
	if err := json.Unmarshal(d.s.Bytes(), &f); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	if f.T == "" {
		return nil, errors.New("decode frame: no frame type")
	}
	return &f, nil
}

// Request builds a req frame with args encoded.
func Request(id uint64, op string, args any) (*Frame, error) {
	a, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("encode %s args: %w", op, err)
	}
	return &Frame{T: FrameReq, ID: id, Op: op, Args: a}, nil
}

// Response builds a res frame: an error when e is non-nil, else the result.
func Response(id uint64, result any, e *Error) (*Frame, error) {
	if e != nil {
		return &Frame{T: FrameRes, ID: id, Err: e}, nil
	}
	ok, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	return &Frame{T: FrameRes, ID: id, OK: ok}, nil
}
