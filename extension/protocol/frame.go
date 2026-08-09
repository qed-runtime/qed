package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Reader reads length-prefixed JSON envelopes from one stream
type Reader struct {
	reader io.Reader
}

// NewReader constructs a framed protocol Reader
func NewReader(reader io.Reader) *Reader {
	return &Reader{reader: reader}
}

// Read returns the next complete protocol envelope
func (reader *Reader) Read() (Envelope, error) {
	if reader == nil || reader.reader == nil {
		return Envelope{}, errors.New("protocol reader is required")
	}
	var header [4]byte
	if _, err := io.ReadFull(reader.reader, header[:]); err != nil {
		return Envelope{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return Envelope{}, errors.New("protocol frame must not be empty")
	}
	if size > MaxFrameBytes {
		return Envelope{}, fmt.Errorf("protocol frame exceeds %d bytes", MaxFrameBytes)
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(reader.reader, payload); err != nil {
		return Envelope{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var envelope Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode protocol envelope: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Envelope{}, errors.New("protocol envelope contains trailing JSON")
		}
		return Envelope{}, fmt.Errorf("decode protocol envelope trailing data: %w", err)
	}
	return envelope, nil
}

// Writer writes length-prefixed JSON envelopes to one stream
//
// Writer is safe for concurrent use
type Writer struct {
	mu     sync.Mutex
	writer io.Writer
}

// NewWriter constructs a framed protocol Writer
func NewWriter(writer io.Writer) *Writer {
	return &Writer{writer: writer}
}

// Write emits one complete protocol envelope atomically with respect to other calls
func (writer *Writer) Write(envelope Envelope) error {
	if writer == nil || writer.writer == nil {
		return errors.New("protocol writer is required")
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode protocol envelope: %w", err)
	}
	if len(payload) == 0 || len(payload) > MaxFrameBytes {
		return fmt.Errorf("encoded protocol frame must contain between 1 and %d bytes", MaxFrameBytes)
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)

	writer.mu.Lock()
	defer writer.mu.Unlock()
	for len(frame) > 0 {
		written, writeErr := writer.writer.Write(frame)
		if writeErr != nil {
			return writeErr
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		frame = frame[written:]
	}
	return nil
}
