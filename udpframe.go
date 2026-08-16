package main

import (
	"encoding/binary"
	"fmt"
	"io"
)

func writeUDPFrameClient(w io.Writer, payload []byte) error {
	if len(payload) > 0xFFFF {
		return fmt.Errorf("udp packet too large: %d bytes", len(payload))
	}
	lenBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(lenBuf, uint16(len(payload)))
	if _, err := w.Write(lenBuf); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readUDPFrameClient(r io.Reader) ([]byte, error) {
	lenBuf := make([]byte, 2)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(lenBuf)
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	return payload, nil
}
