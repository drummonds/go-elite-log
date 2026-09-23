package elitech

import (
	"errors"
	"testing"
	"time"
)

// 2026-09-23 14:05:30, 21.7 degrees, normal record on protocol 0x35.
var baseRecord = [8]byte{0xC0, 0x78, 0x9A, 0xBC, 0x2E, 0x1B, 0x05, 0x00}

func TestDecodeRecordMeasurement(t *testing.T) {
	r, err := decodeRecord(baseRecord, 0x35)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 23, 14, 5, 30, 0, time.Local)
	if !r.Time.Equal(want) {
		t.Errorf("Time = %v, want %v", r.Time, want)
	}
	if r.Status != Measurement {
		t.Errorf("Status = %v, want Measurement", r.Status)
	}
	if r.Temperature != 21.7 {
		t.Errorf("Temperature = %v, want 21.7", r.Temperature)
	}
	if r.Mark || r.Light || r.Vibration {
		t.Errorf("unexpected event flags: %+v", r)
	}
}

func TestDecodeRecordNegativeTemperature(t *testing.T) {
	rec := baseRecord
	rec[0] |= 0x08
	r, err := decodeRecord(rec, 0x35)
	if err != nil {
		t.Fatal(err)
	}
	if r.Temperature != -21.7 {
		t.Errorf("Temperature = %v, want -21.7", r.Temperature)
	}
}

func TestDecodeRecordExtensionBitAddsBit11(t *testing.T) {
	rec := baseRecord
	rec[1] |= 0x02
	r, err := decodeRecord(rec, 0x35)
	if err != nil {
		t.Fatal(err)
	}
	if r.Temperature != 226.5 { // (217 + 2048) / 10
		t.Errorf("Temperature = %v, want 226.5", r.Temperature)
	}
	rec[0] = 0x00                  // pre-0x35 firmware has no 0xC0 format marker
	r, _ = decodeRecord(rec, 0x22) // and pre-0x23 has no extension bit
	if r.Temperature != 21.7 {
		t.Errorf("pre-0x23 Temperature = %v, want 21.7", r.Temperature)
	}
}

func TestDecodeRecordStatusTable(t *testing.T) {
	cases := []struct {
		name     string
		flags    byte
		protocol byte
		want     RecordStatus
	}{
		{"pause wins", 0xC0 | 0x02 | 0x04, 0x35, Pause},
		{"stop", 0xC0 | 0x04, 0x35, Stop},
		{"error only pre-0x35", 0x80, 0x22, Error},
		{"0xC0 is not error on 0x35", 0xC0, 0x35, Measurement},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := baseRecord
			rec[0] = c.flags
			r, err := decodeRecord(rec, c.protocol)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != c.want {
				t.Errorf("Status = %v, want %v", r.Status, c.want)
			}
		})
	}
}

func TestDecodeRecordEventFlags(t *testing.T) {
	rec := baseRecord
	rec[0] = 0xC0 | 0x01 | 0x10 | 0x20
	r, err := decodeRecord(rec, 0x35)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Mark || !r.Light || !r.Vibration {
		t.Errorf("flags not decoded: %+v", r)
	}
	if r.Status != Measurement {
		t.Errorf("events must not change status: %v", r.Status)
	}
}

func TestDecodeRecordSentinel(t *testing.T) {
	_, err := decodeRecord([8]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, 0x35)
	if !errors.Is(err, errNoRecord) {
		t.Errorf("err = %v, want errNoRecord", err)
	}
}

func TestDecodeRecordInvalidDate(t *testing.T) {
	rec := baseRecord
	rec[3] = 0x00 // month bits 1-3 = 0 and day = 0 -> month 1, day 0
	if _, err := decodeRecord(rec, 0x35); err == nil {
		t.Error("want error for day 0")
	}
}

func TestDecodeRecordTH(t *testing.T) {
	r, err := decodeRecordTH([4]byte{0x01, 0x15, 0x02, 0xA9})
	if err != nil {
		t.Fatal(err)
	}
	if r.Temperature != 27.7 || !r.HasHumidity || r.Humidity != 68.1 || r.Status != Measurement {
		t.Errorf("got %+v", r)
	}
	if _, err := decodeRecordTH([4]byte{0xFF, 0xFF, 0xFF, 0xFF}); !errors.Is(err, errNoRecord) {
		t.Errorf("sentinel: %v", err)
	}
	r, _ = decodeRecordTH([4]byte{0x10, 0x0A, 0xFF, 0xFF})
	if r.Temperature != -1.0 || r.HasHumidity {
		t.Errorf("negative/no-humidity: %+v", r)
	}
}

// Captured from a logger going through a freezer: 0.1, then -0.5, -1.1, -1.6.
func TestDecodeRecordTHNegativeRun(t *testing.T) {
	cases := []struct {
		raw  [4]byte
		want float64
	}{
		{[4]byte{0x00, 0x01, 0x01, 0xC0}, 0.1},
		{[4]byte{0x10, 0x05, 0x01, 0xCD}, -0.5},
		{[4]byte{0x10, 0x0B, 0x01, 0xD9}, -1.1},
		{[4]byte{0x10, 0x10, 0x01, 0xE5}, -1.6},
		{[4]byte{0x10, 0xBC, 0x03, 0xC0}, -18.8},
	}
	for _, c := range cases {
		r, err := decodeRecordTH(c.raw)
		if err != nil {
			t.Fatal(err)
		}
		if r.Temperature != c.want {
			t.Errorf("% X -> %v, want %v", c.raw[:], r.Temperature, c.want)
		}
	}
}
