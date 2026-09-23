// Package elitech implements the USB HID protocol of the Elitech "MSC+HID"
// generation of temperature data loggers (USB id 246c:9001, e.g. RC-5).
//
// The protocol was reverse engineered by the elitech-hid-webui and
// python-elitech projects; this is an independent implementation of what
// they documented. Every exchange is one 64-byte report each way.
package elitech

import (
	"errors"
	"fmt"
)

// reportSize is the fixed HID report length in both directions.
const reportSize = 64

// maxData is the largest payload a frame can carry (11 header + 52 + checksum = 64).
const maxData = 52

// op is the 16-bit operation code carried little-endian in bytes 4-5.
type op uint16

const (
	opGetRecord    op = 0x0001
	opGetParameter op = 0x0003
	opSetParameter op = 0x0004
	opRead05       op = 0x0005 // only seen in the factory connect preamble
	opFormat       op = 0x02C0 // erases the record store
	opStop         op = 0x03C0
)

// frame is a decoded report.
type frame struct {
	Op     op
	Offset uint32
	N      byte   // requested count, declared length, or status length
	Data   []byte // payload from byte 11 up to the checksum
}

// encodeFrame builds a 64-byte report:
//
//	33 CC 00 LEN op_lo op_hi 00 off_mid off_lo off_hi N [data] cksum
//
// LEN counts everything up to and including the checksum. n is written
// verbatim so a caller can reproduce the factory off-by-one packet whose
// declared length differs from len(data).
func encodeFrame(o op, offset uint32, n byte, data []byte) []byte {
	if len(data) > maxData {
		panic(fmt.Sprintf("elitech: frame data %d exceeds %d bytes", len(data), maxData))
	}
	buf := make([]byte, reportSize)
	length := 11 + len(data) + 1
	buf[0], buf[1], buf[2] = 0x33, 0xCC, 0x00
	buf[3] = byte(length)
	buf[4], buf[5] = byte(o), byte(o>>8)
	buf[6] = 0
	buf[7] = byte(offset >> 8)
	buf[8] = byte(offset)
	buf[9] = byte(offset >> 16)
	buf[10] = n
	copy(buf[11:], data)
	buf[length-1] = checksum(buf[:length-1])
	return buf
}

func checksum(b []byte) byte {
	var sum byte
	for _, x := range b {
		sum += x
	}
	return sum
}

var errBadFrame = errors.New("elitech: malformed report")

// decodeFrame validates header, length and checksum of a received report.
func decodeFrame(buf []byte) (frame, error) {
	if len(buf) < 12 || buf[0] != 0x33 || buf[1] != 0xCC || buf[2] != 0x00 {
		return frame{}, fmt.Errorf("%w: bad header % X", errBadFrame, head(buf))
	}
	length := int(buf[3])
	if length < 12 || length > len(buf) {
		return frame{}, fmt.Errorf("%w: LEN %d", errBadFrame, length)
	}
	if got, want := buf[length-1], checksum(buf[:length-1]); got != want {
		return frame{}, fmt.Errorf("%w: checksum %#x, want %#x", errBadFrame, got, want)
	}
	return frame{
		Op:     op(buf[4]) | op(buf[5])<<8,
		Offset: uint32(buf[9])<<16 | uint32(buf[7])<<8 | uint32(buf[8]),
		N:      buf[10],
		Data:   buf[11 : length-1],
	}, nil
}

// isRecordAck reports the empty acknowledgement the device sends a few
// milliseconds before a GetRecord data report.
func (f frame) isRecordAck() bool {
	return f.Op == opGetRecord && f.N == 0 && len(f.Data) == 0
}

func head(b []byte) []byte {
	if len(b) > 12 {
		return b[:12]
	}
	return b
}
