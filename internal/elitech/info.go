package elitech

import (
	"encoding/binary"
	"time"
)

// StartMode is how a recording session begins.
type StartMode int

const (
	StartImmediate StartMode = 0
	StartManual    StartMode = 1
	StartTimer     StartMode = 2
	StartUnknown   StartMode = 7
)

func (m StartMode) String() string {
	switch m {
	case StartImmediate:
		return "immediate"
	case StartManual:
		return "manual (button)"
	case StartTimer:
		return "timer"
	}
	return "unknown"
}

// StartSettings is the decoded start byte at address 0x20.
type StartSettings struct {
	Mode         StartMode
	ButtonStop   bool // recording can be stopped from the button
	SoftwareStop bool // recording can be stopped by software
}

func decodeStartByte(b byte) StartSettings {
	return StartSettings{
		Mode:         StartMode(b & 0x07),
		ButtonStop:   b&0x08 != 0,
		SoftwareStop: b&0x10 != 0,
	}
}

// encodeStartByte keeps the upper three bits of the existing byte, sets
// Immediate or Manual start and the two stop-permission bits.
func encodeStartByte(old byte, immediate, buttonStop, softwareStop bool) byte {
	b := old & 0xE0
	if immediate {
		b |= byte(StartImmediate)
	} else {
		b |= byte(StartManual)
	}
	if buttonStop {
		b |= 0x08
	}
	if softwareStop {
		b |= 0x10
	}
	return b
}

// decodeDateTime reads the 7-byte [yy mm dow dd hh mi ss] form. Stopped
// loggers fill runtime fields with 0xFF; those and all-zero decode as no
// value. The day-of-week byte is ignored.
func decodeDateTime(b []byte) (time.Time, bool) {
	if len(b) < 7 {
		return time.Time{}, false
	}
	allZero, allFF := true, true
	for _, x := range b[:7] {
		allZero = allZero && x == 0
		allFF = allFF && x == 0xFF
	}
	if allZero || allFF {
		return time.Time{}, false
	}
	for _, i := range []int{0, 1, 3, 4, 5, 6} {
		if b[i] == 0xFF {
			return time.Time{}, false
		}
	}
	month, day := int(b[1]), int(b[3])
	if month < 1 || month > 12 || day < 1 || day > 31 || b[4] > 23 || b[5] > 59 || b[6] > 59 {
		return time.Time{}, false
	}
	t := time.Date(2000+int(b[0]), time.Month(month), day, int(b[4]), int(b[5]), int(b[6]), 0, time.Local)
	if t.Day() != day {
		return time.Time{}, false
	}
	return t, true
}

// encodeDateTime writes the factory form, including .NET DayOfWeek (Sunday=0).
func encodeDateTime(t time.Time) [7]byte {
	return [7]byte{
		byte(t.Year() - 2000), byte(t.Month()), byte(t.Weekday()),
		byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second()),
	}
}

// decodeCapacityCount reads the 10-byte block at 0x42: capacity u32 at
// 0x42, record count u32 at 0x46 (protocol >= 0x24) or u16 at 0x48.
// A stopped logger overwrites the first and last two bytes with 0xFF.
func decodeCapacityCount(b []byte, protocol byte) (capacity, count uint32, ok bool) {
	if len(b) < 10 {
		return 0, 0, false
	}
	switch {
	case isFF(b[0:4]):
		return 0, 0, false
	case isFF(b[0:2]) && !isFF(b[2:4]):
		capacity = uint32(binary.BigEndian.Uint16(b[2:4]))
	default:
		capacity = binary.BigEndian.Uint32(b[0:4])
	}
	if protocol >= 0x24 {
		if isFF(b[4:8]) {
			return 0, 0, false
		}
		count = binary.BigEndian.Uint32(b[4:8])
	} else {
		if isFF(b[6:8]) {
			return 0, 0, false
		}
		count = uint32(binary.BigEndian.Uint16(b[6:8]))
	}
	if count > capacity {
		return 0, 0, false
	}
	return capacity, count, true
}

// decodeInterval reads the u16 sampling interval in units of ten seconds.
func decodeInterval(b []byte) (time.Duration, bool) {
	if len(b) < 2 || isFF(b[:2]) {
		return 0, false
	}
	v := binary.BigEndian.Uint16(b[:2])
	if v == 0 {
		return 0, false
	}
	return time.Duration(v) * 10 * time.Second, true
}

func encodeInterval(d time.Duration) [2]byte {
	var out [2]byte
	binary.BigEndian.PutUint16(out[:], uint16(d/(10*time.Second)))
	return out
}

func isFF(b []byte) bool {
	for _, x := range b {
		if x != 0xFF {
			return false
		}
	}
	return len(b) > 0
}

// Status is the live state read from the status space (op 0x05): byte 0 is
// 1 while recording, bytes 8-14 the actual start time, 0x8A-0x8B the record
// count (little-endian), and 0x90/0x98/0xB0/0xB8 the extremes seen so far.
// Extremes are 0 when the logger reports none.
type Status struct {
	Recording      bool
	StartedAt      time.Time
	RecordCount    uint32
	MaxTemperature float64
	MinTemperature float64
	MaxHumidity    float64
	MinHumidity    float64
}

// decodeStatus takes the 0x20 bytes at status offset 0 and at least 0x40
// bytes from status offset 0x80.
func decodeStatus(head, tail []byte) Status {
	var st Status
	if len(head) >= 0x0F {
		st.Recording = head[0] == 1
		st.StartedAt, _ = decodeDateTime(head[8:15])
	}
	if len(tail) >= 0x40 {
		st.RecordCount = uint32(tail[0x8A-0x80]) | uint32(tail[0x8B-0x80])<<8
		st.MaxTemperature, _ = decodeTenths(tail[0x90-0x80], tail[0x91-0x80])
		st.MinTemperature, _ = decodeTenths(tail[0x98-0x80], tail[0x99-0x80])
		st.MaxHumidity, _ = decodeTenths(tail[0xB0-0x80], tail[0xB1-0x80])
		st.MinHumidity, _ = decodeTenths(tail[0xB8-0x80], tail[0xB9-0x80])
	}
	return st
}
