package elitech

import (
	"testing"
	"time"
)

func TestDecodeDateTime(t *testing.T) {
	got, ok := decodeDateTime([]byte{26, 9, 3, 23, 14, 5, 30})
	want := time.Date(2026, 9, 23, 14, 5, 30, 0, time.Local)
	if !ok || !got.Equal(want) {
		t.Errorf("got %v ok=%v, want %v", got, ok, want)
	}
	for _, bad := range [][]byte{
		{0, 0, 0, 0, 0, 0, 0},
		{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		{26, 0xFF, 3, 23, 14, 5, 30},
		{26, 13, 3, 23, 14, 5, 30},
	} {
		if _, ok := decodeDateTime(bad); ok {
			t.Errorf("% X should decode as no value", bad)
		}
	}
}

func TestEncodeDateTimeWritesFactoryDayOfWeek(t *testing.T) {
	// 2026-09-23 is a Wednesday: .NET DayOfWeek Sunday=0 -> 3.
	got := encodeDateTime(time.Date(2026, 9, 23, 14, 5, 30, 0, time.Local))
	want := []byte{26, 9, 3, 23, 14, 5, 30}
	if string(got[:]) != string(want) {
		t.Errorf("got % X want % X", got, want)
	}
}

func TestDecodeCapacityCount(t *testing.T) {
	cases := []struct {
		name          string
		block         []byte
		protocol      byte
		wantCap, want uint32
		ok            bool
	}{
		{"running", []byte{0x00, 0x00, 0x7d, 0x00, 0x00, 0x00, 0x00, 0x1b, 0x00, 0x00}, 0x35, 32000, 27, true},
		{"stopped sentinel pattern", []byte{0xff, 0xff, 0x7d, 0x00, 0x00, 0x00, 0x00, 0x1b, 0xff, 0xff}, 0x35, 32000, 27, true},
		{"old protocol u16 count", []byte{0x00, 0x00, 0x7d, 0x00, 0xAA, 0xAA, 0x00, 0x1b, 0x00, 0x1b}, 0x20, 32000, 27, true},
		{"all unknown", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, 0x35, 0, 0, false},
		{"count exceeds capacity", []byte{0x00, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x20, 0x00, 0x00}, 0x35, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			capacity, count, ok := decodeCapacityCount(c.block, c.protocol)
			if ok != c.ok || capacity != c.wantCap || count != c.want {
				t.Errorf("got cap=%d count=%d ok=%v, want cap=%d count=%d ok=%v", capacity, count, ok, c.wantCap, c.want, c.ok)
			}
		})
	}
}

func TestDecodeInterval(t *testing.T) {
	if d, ok := decodeInterval([]byte{0x00, 0x3C}); !ok || d != 10*time.Minute {
		t.Errorf("got %v ok=%v, want 10m", d, ok)
	}
	if _, ok := decodeInterval([]byte{0xFF, 0xFF}); ok {
		t.Error("FF FF should be unknown")
	}
	if _, ok := decodeInterval([]byte{0x00, 0x00}); ok {
		t.Error("zero should be unknown")
	}
}

func TestStartByte(t *testing.T) {
	s := decodeStartByte(0x19) // 0b11001
	if s.Mode != StartManual || !s.ButtonStop || !s.SoftwareStop {
		t.Errorf("got %+v", s)
	}
	if decodeStartByte(0x02).Mode != StartTimer {
		t.Error("0x02 should be Timer")
	}
	if decodeStartByte(0x00).Mode != StartImmediate {
		t.Error("0x00 should be Immediate")
	}
	// Re-encoding preserves the upper bits the factory software leaves alone,
	// and always writes Manual mode, matching the factory save flow.
	if got := encodeStartByte(0xE2, false, true, false); got != 0xE0|0x01|0x08 {
		t.Errorf("encodeStartByte manual = %#x", got)
	}
	if got := encodeStartByte(0xE2, true, false, true); got != 0xE0|0x00|0x10 {
		t.Errorf("encodeStartByte immediate = %#x", got)
	}
}

func TestDecodeStatus(t *testing.T) {
	head := make([]byte, 0x20)
	head[0] = 1
	copy(head[8:], []byte{26, 9, 0, 23, 14, 58, 34})
	tail := make([]byte, 0x48)                    // 0x80..0xC7
	tail[0x8A-0x80], tail[0x8B-0x80] = 0x1E, 0x01 // 286 records, little-endian
	copy(tail[0x90-0x80:], []byte{0x01, 0x1B})
	copy(tail[0x98-0x80:], []byte{0x01, 0x0B})
	copy(tail[0xB0-0x80:], []byte{0x02, 0xA9})
	copy(tail[0xB8-0x80:], []byte{0x01, 0xDC})
	st := decodeStatus(head, tail)
	if !st.Recording {
		t.Error("state byte 1 should mean recording")
	}
	if want := time.Date(2026, 9, 23, 14, 58, 34, 0, time.Local); !st.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v, want %v", st.StartedAt, want)
	}
	if st.RecordCount != 286 {
		t.Errorf("RecordCount = %d", st.RecordCount)
	}
	if st.MaxTemperature != 28.3 || st.MinTemperature != 26.7 || st.MaxHumidity != 68.1 || st.MinHumidity != 47.6 {
		t.Errorf("extremes %+v", st)
	}
	st = decodeStatus(make([]byte, 0x20), make([]byte, 0x48))
	if st.Recording || !st.StartedAt.IsZero() {
		t.Errorf("zero status decoded as %+v", st)
	}
}
