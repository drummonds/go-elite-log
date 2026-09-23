package elitech

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Vectors from the reverse-engineered reference (checksums verified there).
func TestEncodeFrameVectors(t *testing.T) {
	cases := []struct {
		name   string
		op     op
		offset uint32
		n      byte
		data   []byte
		want   string
	}{
		{"GetParameter 0x00/14", opGetParameter, 0x000000, 14, nil, "33 CC 00 0C 03 00 00 00 00 00 0E 1C"},
		{"GetParameter 0x94/2", opGetParameter, 0x000094, 2, nil, "33 CC 00 0C 03 00 00 00 94 00 02 A4"},
		{"GetParameter 0x12C/48", opGetParameter, 0x00012C, 48, nil, "33 CC 00 0C 03 00 00 01 2C 00 30 6B"},
		{"GetRecord 0/6", opGetRecord, 0, 6, nil, "33 CC 00 0C 01 00 00 00 00 00 06 12"},
		{"GetRecord 6/6", opGetRecord, 6, 6, nil, "33 CC 00 0C 01 00 00 00 06 00 06 18"},
		{"Format", opFormat, 0, 1, []byte{0}, "33 CC 00 0D C0 02 00 00 00 00 01 00 CF"},
		{"Stop", opStop, 0, 1, []byte{0}, "33 CC 00 0D C0 03 00 00 00 00 01 00 D0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := encodeFrame(c.op, c.offset, c.n, c.data)
			if len(got) != reportSize {
				t.Fatalf("frame length %d, want %d", len(got), reportSize)
			}
			want := hexBytes(t, c.want)
			if !bytes.Equal(got[:len(want)], want) {
				t.Errorf("got % X\nwant % X", got[:len(want)], want)
			}
			if !bytes.Equal(got[len(want):], make([]byte, reportSize-len(want))) {
				t.Errorf("padding is not zero")
			}
		})
	}
}

// Packet 3 of the factory configure flow declares 0x30 bytes but carries 47:
// LEN 0x3B, checksum at index 58. The encoder must reproduce that exactly.
func TestEncodeFrameDeclaredLengthOverride(t *testing.T) {
	data := bytes.Repeat([]byte{0xAA}, 47)
	got := encodeFrame(opSetParameter, 0x60, 0x30, data)
	if got[3] != 0x3B {
		t.Errorf("LEN = %#x, want 0x3B", got[3])
	}
	if got[10] != 0x30 {
		t.Errorf("declared N = %#x, want 0x30", got[10])
	}
	var sum byte
	for _, b := range got[:58] {
		sum += b
	}
	if got[58] != sum {
		t.Errorf("checksum at 58 = %#x, want %#x", got[58], sum)
	}
	if got[59] != 0 {
		t.Errorf("byte 59 should be padding")
	}
}

func TestEncodeFrameOffsetByteOrder(t *testing.T) {
	got := encodeFrame(opGetParameter, 0x123456, 1, nil)
	if got[7] != 0x34 || got[8] != 0x56 || got[9] != 0x12 {
		t.Errorf("offset bytes 7-9 = %02X %02X %02X, want 34 56 12 (mid, low, high)", got[7], got[8], got[9])
	}
}

func TestDecodeFrameGetParameterResponse(t *testing.T) {
	// Response to GetParameter 0x94/2 carrying 2 data bytes 00 35.
	raw := []byte{0x33, 0xCC, 0x00, 0x0E, 0x03, 0x00, 0x00, 0x00, 0x94, 0x00, 0x02, 0x00, 0x35}
	var sum byte
	for _, b := range raw {
		sum += b
	}
	raw = append(raw, sum)
	buf := make([]byte, reportSize)
	copy(buf, raw)

	f, err := decodeFrame(buf)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != opGetParameter || f.Offset != 0x94 || f.N != 2 {
		t.Errorf("header = %+v", f)
	}
	if !bytes.Equal(f.Data, []byte{0x00, 0x35}) {
		t.Errorf("data = % X", f.Data)
	}
}

func TestDecodeFrameRejectsBadChecksum(t *testing.T) {
	buf := make([]byte, reportSize)
	copy(buf, hexBytes(t, "33 CC 00 0C 03 00 00 00 00 00 0E 1D"))
	if _, err := decodeFrame(buf); err == nil {
		t.Error("want checksum error")
	}
}

func TestDecodeFrameRejectsBadHeader(t *testing.T) {
	buf := make([]byte, reportSize)
	copy(buf, hexBytes(t, "FF FF FF FF FF FF FF FF FF FF FF FF"))
	if _, err := decodeFrame(buf); err == nil {
		t.Error("want header error")
	}
}

func TestDecodeFrameRecognisesRecordAck(t *testing.T) {
	buf := make([]byte, reportSize)
	copy(buf, hexBytes(t, "33 CC 00 0C 01 00 00 00 00 00 00 0C"))
	f, err := decodeFrame(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !f.isRecordAck() {
		t.Errorf("want ack-only, got %+v", f)
	}
}
