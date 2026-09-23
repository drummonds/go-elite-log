package elitech

import (
	"encoding/binary"
	"os"
	"sync"
	"time"
)

// fakeDevice simulates a 246c:9001 logger behind the transport interface:
// a flat parameter memory, a record store, the ACK-then-data GetRecord
// behaviour and SetParameter status replies. It logs every frame written.
type fakeDevice struct {
	mu         sync.Mutex
	mem        [0x200]byte
	stat       [0x200]byte // op 0x05 status space
	records    [][8]byte
	recordsTH  [][4]byte // when set, GetRecord serves 4-byte records
	queue      [][]byte
	written    []frame
	formats    int
	stops      int
	silentStop bool
	// needsPreamble makes GetRecord go unanswered until the last preamble
	// read (op 5 at 0x140) has been seen, like the firmware the reference
	// project had to work around.
	needsPreamble bool
	preambleSeen  bool
	closed        bool
	protocol      byte
	// capacityReads counts 0x42 reads so tests can make early ones fail.
	capacityReads   int
	capacityFFUntil int
	// wideReads makes GetParameter answer with 4 extra leading bytes.
	wideReads bool
}

func newFakeDevice() *fakeDevice {
	d := &fakeDevice{protocol: 0x35}
	binary.BigEndian.PutUint16(d.mem[0x00:], 0x2005)
	copy(d.mem[0x02:], "RC5A1234567")
	d.mem[0x95] = d.protocol
	d.mem[0x20] = 0x19 // manual, button stop, software stop
	copy(d.mem[0x28:], []byte{26, 9, 3, 23, 9, 0, 0})
	copy(d.mem[0x30:], []byte{26, 9, 3, 23, 9, 5, 0})
	copy(d.mem[0x38:], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	binary.BigEndian.PutUint32(d.mem[0x42:], 32000)
	binary.BigEndian.PutUint16(d.mem[0x4C:], 60) // 10 minutes
	copy(d.mem[0x88:], []byte{26, 9, 3, 23, 14, 5, 30})
	d.mem[0x27] = 0x0A
	return d
}

// addRecordsTH stores n 4-byte temperature+humidity records and marks the
// status space as recording since 14:58:34 with n records.
func (d *fakeDevice) addRecordsTH(n int) {
	for i := 0; i < n; i++ {
		d.recordsTH = append(d.recordsTH, [4]byte{0x01, byte(0x15 + i), 0x02, 0xA9})
	}
	binary.BigEndian.PutUint32(d.mem[0x46:], uint32(n))
	d.stat[0] = 1
	copy(d.stat[8:], []byte{26, 9, 0, 23, 14, 58, 34})
	d.stat[0x8A], d.stat[0x8B] = byte(n), byte(n>>8)
}

// addRecords stores n measurement records at 10-minute spacing from base.
func (d *fakeDevice) addRecords(n int) {
	for i := 0; i < n; i++ {
		rec := baseRecord
		rec[6] = byte(i % 60)   // minute
		rec[5] = byte(0x1B + i) // vary temperature
		d.records = append(d.records, rec)
	}
	binary.BigEndian.PutUint32(d.mem[0x46:], uint32(len(d.records)))
}

func (d *fakeDevice) Close() error { d.closed = true; return nil }

func (d *fakeDevice) Read(buf []byte, timeout time.Duration) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	r := d.queue[0]
	d.queue = d.queue[1:]
	return copy(buf, r), nil
}

func (d *fakeDevice) Write(report []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, err := decodeFrame(report)
	if err != nil {
		return err
	}
	d.written = append(d.written, f)
	switch f.Op {
	case opRead05:
		off, n := int(f.Offset), int(f.N)
		if off == 0x140 {
			d.preambleSeen = true
		}
		d.queue = append(d.queue, encodeFrame(f.Op, f.Offset, f.N, d.stat[off:off+n]))
	case opGetParameter:
		off, n := int(f.Offset), int(f.N)
		if off == 0x42 {
			d.capacityReads++
			if d.capacityReads <= d.capacityFFUntil {
				d.queue = append(d.queue, encodeFrame(f.Op, f.Offset, f.N, isFFs(n)))
				return nil
			}
		}
		if d.wideReads && off >= 4 {
			d.queue = append(d.queue, encodeFrame(f.Op, f.Offset-4, f.N+4, d.mem[off-4:off+n]))
			return nil
		}
		d.queue = append(d.queue, encodeFrame(f.Op, f.Offset, f.N, d.mem[off:off+n]))
	case opSetParameter:
		copy(d.mem[f.Offset:], f.Data)
		d.queue = append(d.queue, encodeFrame(opSetParameter, f.Offset, 1, []byte{1}))
	case opGetRecord:
		if d.needsPreamble && !d.preambleSeen {
			return nil // silence
		}
		d.queue = append(d.queue, encodeFrame(opGetRecord, 0, 0, nil)) // ACK first
		var data []byte
		for i := 0; i < recordsPerPacket; i++ {
			idx := int(f.Offset) + i
			switch {
			case d.recordsTH != nil && idx < len(d.recordsTH):
				data = append(data, d.recordsTH[idx][:]...)
			case d.recordsTH != nil:
				data = append(data, isFFs(4)...)
			case idx < len(d.records):
				data = append(data, d.records[idx][:]...)
			default:
				data = append(data, isFFs(recordSize)...)
			}
		}
		d.queue = append(d.queue, encodeFrame(opGetRecord, f.Offset, recordsPerPacket, data))
	case opStop:
		d.stops++
		if !d.silentStop {
			d.queue = append(d.queue, encodeFrame(opStop, 0, 1, []byte{1}))
		}
	case opFormat:
		d.formats++
		d.records = nil
		binary.BigEndian.PutUint32(d.mem[0x46:], 0)
		d.queue = append(d.queue, encodeFrame(opFormat, 0, 1, []byte{1}))
	}
	return nil
}

func isFFs(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}

// writtenOps lists the op sequence written, for assertions.
func (d *fakeDevice) writtenOps() []op {
	ops := make([]op, len(d.written))
	for i, f := range d.written {
		ops[i] = f.Op
	}
	return ops
}

func newTestLogger(d *fakeDevice) *Logger {
	l := newLogger(d)
	l.sleep = func(time.Duration) {}
	return l
}
