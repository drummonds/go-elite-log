package elitech

import (
	"errors"
	"fmt"
	"time"
)

// recordSize is the stored length of one measurement.
const recordSize = 8

// recordsPerPacket is how many records one GetRecord report carries.
const recordsPerPacket = 6

// RecordStatus says what kind of event a record is.
type RecordStatus int

const (
	Measurement RecordStatus = iota
	Pause
	Stop
	Error
)

func (s RecordStatus) String() string {
	switch s {
	case Measurement:
		return "measurement"
	case Pause:
		return "pause"
	case Stop:
		return "stop"
	case Error:
		return "error"
	}
	return fmt.Sprintf("status(%d)", int(s))
}

// Record is one stored sample. Temperature is only meaningful when Status
// is Measurement; its unit is whatever the logger records in (see Info).
type Record struct {
	Index       int // 0-based position in the logger's store
	Time        time.Time
	Temperature float64
	Humidity    float64 // percent relative humidity, when HasHumidity
	HasHumidity bool
	Status      RecordStatus
	Mark        bool
	Light       bool
	Vibration   bool
}

var errNoRecord = errors.New("elitech: empty record slot")

// decodeRecord unpacks the 8-byte bit-packed record. Every record carries
// its own timestamp; nothing is derived from the start time and interval.
func decodeRecord(b [8]byte, protocol byte) (Record, error) {
	if b == [8]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF} {
		return Record{}, errNoRecord
	}
	flags := b[0]
	if protocol >= 0x35 {
		// Normal records carry 0xC0 in the top two bits as a format marker.
		flags &= 0x3F
	}
	second := int(b[1]>>2) & 0x3F
	year := 2000 + int(b[2]&0x7F)
	month := int(b[3]&0x07)<<1 | int(b[2]>>7)
	day := int(b[3]>>3) & 0x1F
	hour := int(b[4]) & 0x1F
	minute := int(b[6]) & 0x3F
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59 {
		return Record{}, fmt.Errorf("elitech: record % X has invalid timestamp", b[:])
	}
	ts := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.Local)
	if ts.Day() != day || ts.Month() != time.Month(month) {
		return Record{}, fmt.Errorf("elitech: record % X has invalid calendar date", b[:])
	}

	raw := int(b[5])<<3 | int(b[4]>>5)
	if protocol >= 0x23 {
		raw |= int(b[1]>>1&1) << 11
	}
	temp := float64(raw) / 10
	if flags&0x08 != 0 {
		temp = -temp
	}

	r := Record{
		Time:      ts,
		Mark:      flags&0x01 != 0,
		Light:     flags&0x10 != 0,
		Vibration: flags&0x20 != 0,
	}
	switch {
	case flags&0x02 != 0:
		r.Status = Pause
	case flags&0x04 != 0:
		r.Status = Stop
	case flags&0x80 != 0 && protocol < 0x35:
		r.Status = Error
	default:
		r.Status = Measurement
		r.Temperature = temp
	}
	return r, nil
}

// decodeRecordTH unpacks the 4-byte record of the temperature-and-humidity
// loggers: temperature then humidity, each a big-endian tenth with 0x8000
// marking a negative value and 0xFFFF meaning no reading. These records
// carry no timestamp; the caller derives it from the start time.
func decodeRecordTH(b [4]byte) (Record, error) {
	if b == [4]byte{0xFF, 0xFF, 0xFF, 0xFF} {
		return Record{}, errNoRecord
	}
	temp, ok := decodeTenths(b[0], b[1])
	if !ok {
		return Record{}, fmt.Errorf("elitech: record % X has no temperature", b[:])
	}
	r := Record{Temperature: temp, Status: Measurement}
	r.Humidity, r.HasHumidity = decodeTenths(b[2], b[3])
	return r, nil
}

// decodeTenths reads a big-endian value in tenths; 0x8000 flags negative
// and 0xFFFF means absent.
func decodeTenths(hi, lo byte) (float64, bool) {
	v := uint16(hi)<<8 | uint16(lo)
	if v == 0xFFFF {
		return 0, false
	}
	if v&0x8000 != 0 {
		return -float64(v&0x7FFF) / 10, true
	}
	return float64(v) / 10, true
}
