// Package protoio provides the tiny length-prefixed framing shared by the
// hf-ipfs libp2p streams and the local control socket.
//
// Frame layout: 4 byte big-endian length, then that many bytes of payload.
// JSON messages are frames whose payload is UTF-8 JSON; block frames carry raw
// block bytes.
package protoio

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// MaxFrame bounds any single frame payload.
const MaxFrame = 8 << 20

// ErrFrameTooLarge is returned when a peer announces an oversized frame.
var ErrFrameTooLarge = fmt.Errorf("frame exceeds %d bytes", MaxFrame)

// WriteFrame writes one length-prefixed frame.
func WriteFrame(w io.Writer, data []byte) error {
	if len(data) > MaxFrame {
		return ErrFrameTooLarge
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	_, err := w.Write(data)
	return err
}

// ReadFrame reads one length-prefixed frame.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// WriteJSON marshals v into a frame.
func WriteJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal frame payload: %w", err)
	}
	return WriteFrame(w, data)
}

// ReadJSON reads a frame and unmarshals it into v.
func ReadJSON(r io.Reader, v any) error {
	data, err := ReadFrame(r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal frame payload: %w", err)
	}
	return nil
}
