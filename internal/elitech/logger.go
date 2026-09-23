package elitech

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"git.bytestone.uk/hum3/go-elite-log/internal/hid"
)

// USB identity of the MSC+HID logger generation.
const (
	VendorID  = 0x246c
	ProductID = 0x9001
)

// transport is one open HID report channel. hid.Conn satisfies it.
type transport interface {
	Write(report []byte) error
	Read(buf []byte, timeout time.Duration) (int, error)
	Close() error
}

// Find lists attached loggers.
func Find(roots hid.Roots) ([]hid.Device, error) {
	return hid.Enumerate(roots, VendorID, ProductID)
}

// Open opens a logger found by Find.
func Open(dev hid.Device) (*Logger, error) {
	conn, err := dev.Open()
	if err != nil {
		return nil, err
	}
	return newLogger(conn), nil
}

// Logger is an open session with one device. Methods are serialised; the
// device tolerates only one transaction at a time.
type Logger struct {
	mu       sync.Mutex
	t        transport
	sleep    func(time.Duration)
	now      func() time.Time
	protocol byte
}

func newLogger(t transport) *Logger {
	return &Logger{t: t, sleep: time.Sleep, now: time.Now}
}

// Close releases the device.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.t.Close()
}

// Identity is what never changes about a logger.
type Identity struct {
	Model    uint16
	Serial   string
	Protocol byte
}

// Info is the logger's identity and current state. Zero times mean the
// logger reported no value (stopped loggers blank their runtime fields).
type Info struct {
	Identity
	Capacity     uint32
	RecordCount  uint32
	CountKnown   bool
	Interval     time.Duration
	Start        StartSettings
	Recording    bool // from the status space
	Status       Status
	Fahrenheit   bool // the unit the logger records temperatures in
	Battery      int  // 0..15, -1 unknown
	ConfiguredAt time.Time
	StartedAt    time.Time
	StoppedAt    time.Time
	DeviceTime   time.Time
}

// TemperatureUnit is the symbol for the logger's temperature unit.
func (i Info) TemperatureUnit() string {
	if i.Fahrenheit {
		return "°F"
	}
	return "°C"
}

// Settings is what Configure writes.
type Settings struct {
	Interval         time.Duration // multiple of 10 s, at most 24 h
	StartImmediately bool          // start recording on configure; false = long-press the button
	ButtonStop       bool
	SoftwareStop     bool
	SyncClock        bool // write the host clock as the configuration time
}

var (
	// ErrRecordsPresent means Configure would erase records the caller has
	// not confirmed (confirmedCount must equal the device's current count).
	ErrRecordsPresent  = errors.New("elitech: logger holds records; configuring erases them")
	ErrInvalidInterval = errors.New("elitech: interval must be a multiple of 10 s between 10 s and 24 h")
	ErrTimeout         = errors.New("elitech: no response from logger")
	ErrRejected        = errors.New("elitech: logger rejected the write")
	// ErrStopUnacknowledged means the stop command was sent but the logger
	// did not reply; an idle logger stays silent, so check Info afterwards.
	ErrStopUnacknowledged = errors.New("elitech: stop command sent but not acknowledged")
)

const (
	timeoutGet         = time.Second
	timeoutGetCleanup  = 750 * time.Millisecond
	timeoutSet         = time.Second
	timeoutCommand     = 2 * time.Second
	timeoutRecord      = 1500 * time.Millisecond
	timeoutRecordSlice = 350 * time.Millisecond
	pausePreamble      = 80 * time.Millisecond
	pauseCapacityRetry = 60 * time.Millisecond
	pauseAfterRecords  = 50 * time.Millisecond
	holdAfterFormat    = 550 * time.Millisecond
)

// Info reads identity and state.
func (l *Logger) Info() (Info, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id, err := l.prime()
	if err != nil {
		return Info{}, err
	}
	info := Info{Identity: id, Battery: -1}
	info.Capacity, info.RecordCount, info.CountKnown = l.readCapacityCount()

	if b, err := l.get(0x4C, 2); err == nil {
		info.Interval, _ = decodeInterval(b)
	}
	if b, err := l.get(0x20, 1); err == nil {
		info.Start = decodeStartByte(b[0])
	}
	if b, err := l.get(0x21, 1); err == nil && b[0] != 0xFF {
		info.Fahrenheit = b[0]&0x08 != 0
	}
	if b, err := l.get(0x26, 2); err == nil && b[1] != 0xFF {
		info.Battery = int(b[1] & 0x0F)
	}
	if b, err := l.get(0x28, 7); err == nil {
		info.ConfiguredAt, _ = decodeDateTime(b)
	}
	if b, err := l.get(0x30, 15); err == nil {
		info.StartedAt, _ = decodeDateTime(b[0:7])
		info.StoppedAt, _ = decodeDateTime(b[8:15])
	}
	if b, err := l.get(0x88, 7); err == nil {
		info.DeviceTime, _ = decodeDateTime(b)
	}
	info.Status = l.readStatus()
	info.Recording = info.Status.Recording
	if !info.Status.StartedAt.IsZero() {
		// The parameter space keeps a factory placeholder start time on
		// this generation; the status space has the real one.
		info.StartedAt = info.Status.StartedAt
	}
	return info, nil
}

// readStatus reads the two status-space blocks; failures yield a zero Status.
func (l *Logger) readStatus() Status {
	head, err := l.readOp5(0x00, 0x20)
	if err != nil {
		return Status{}
	}
	tail, err := l.readOp5(0x80, 0x30)
	if err != nil {
		return Status{}
	}
	more, err := l.readOp5(0xB0, 0x10)
	if err != nil {
		return Status{}
	}
	return decodeStatus(head, append(tail, more...))
}

func (l *Logger) readOp5(offset uint32, n byte) ([]byte, error) {
	l.drain()
	if err := l.t.Write(encodeFrame(opRead05, offset, n, nil)); err != nil {
		return nil, err
	}
	f, err := l.awaitReply(opRead05, timeoutGet)
	if err != nil {
		return nil, err
	}
	if f.Offset != offset || len(f.Data) < int(n) {
		return nil, fmt.Errorf("%w: status reply covers %#x+%d", errBadFrame, f.Offset, len(f.Data))
	}
	return f.Data[:n], nil
}

// Records downloads stored records; last limits to the most recent n
// (0 = all). Records the device could not encode are skipped.
func (l *Logger) Records(last int) ([]Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.prime(); err != nil {
		return nil, err
	}
	_, count, ok := l.readCapacityCount()
	if !ok {
		return nil, errors.New("elitech: logger did not report its record count")
	}
	if count == 0 {
		return nil, nil
	}
	wantedStart := 0
	if last > 0 && uint32(last) < count {
		wantedStart = int(count) - last
	}
	// Timestamps for the 4-byte record format come from the status space.
	status := l.readStatus()
	interval := time.Duration(0)
	if b, err := l.get(0x4C, 2); err == nil {
		interval, _ = decodeInterval(b)
	}

	var recs []Record
	preambleSent := false
	for off := wantedStart / recordsPerPacket * recordsPerPacket; off < int(count); off += recordsPerPacket {
		f, err := l.getRecordPacket(uint32(off))
		if errors.Is(err, ErrTimeout) && !preambleSent {
			// Some firmware only serves records after the factory
			// software's connect sequence; newer firmware does not need it.
			l.sendConnectPreamble()
			preambleSent = true
			f, err = l.getRecordPacket(uint32(off))
		}
		if err != nil {
			return recs, fmt.Errorf("records at %d: %w", off, err)
		}
		width := recordWidth(f)
		valid := min(recordsPerPacket, int(count)-off)
		for i := 0; i < valid; i++ {
			idx := off + i
			if idx < wantedStart || (i+1)*width > len(f.Data) {
				continue
			}
			r, err := decodeRecordAt(f.Data[i*width:(i+1)*width], l.protocol)
			if err != nil {
				continue
			}
			r.Index = idx
			if r.Time.IsZero() {
				r.Time = status.StartedAt.Add(time.Duration(idx) * interval)
			}
			recs = append(recs, r)
		}
	}
	l.sleep(pauseAfterRecords)
	l.primeWith(timeoutGetCleanup) // failure is non-fatal
	return recs, nil
}

// Configure writes settings with the factory save sequence and then erases
// the record store, which is the only sequence known to persist across a
// replug. confirmedCount must equal the device's record count (or the store
// must be empty). The transport is closed afterwards; reopen and check with
// VerifySettings.
func (l *Logger) Configure(s Settings, confirmedCount uint32) error {
	if s.Interval < 10*time.Second || s.Interval > 24*time.Hour || s.Interval%(10*time.Second) != 0 {
		return ErrInvalidInterval
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.prime(); err != nil {
		return err
	}
	_, count, ok := l.readCapacityCount()
	if !ok {
		return errors.New("elitech: logger did not report its record count; refusing to erase")
	}
	if count != 0 && count != confirmedCount {
		return fmt.Errorf("%w (%d records)", ErrRecordsPresent, count)
	}

	blocks := make([][]byte, len(configBlocks))
	for i, b := range configBlocks {
		data, err := l.get(b.offset, b.length)
		if err != nil {
			return fmt.Errorf("read config block %#x: %w", b.offset, err)
		}
		blocks[i] = append([]byte(nil), data...)
	}
	blocks[0][0x20] = encodeStartByte(blocks[0][0x20], s.StartImmediately, s.ButtonStop, s.SoftwareStop)
	if s.SyncClock {
		dt := encodeDateTime(l.now())
		copy(blocks[0][0x28:], dt[:])
	}
	iv := encodeInterval(s.Interval)
	copy(blocks[1][0x4C-0x30:], iv[:])
	for i := 0x88 - 0x60; i < 0x8F-0x60; i++ {
		blocks[2][i] = 0
	}

	for i, b := range configBlocks {
		data := blocks[i][:b.written]
		if err := l.set(b.offset, b.declared, data); err != nil {
			return fmt.Errorf("write config block %#x: %w", b.offset, err)
		}
	}
	l.drain()
	if err := l.t.Write(encodeFrame(opFormat, 0, 1, []byte{0})); err != nil {
		return err
	}
	l.readReport(timeoutCommand)
	l.sleep(holdAfterFormat)
	return l.t.Close()
}

// Stop sends the stop command. The logger must have been configured with
// SoftwareStop; the reply is not decoded, so check Info afterwards.
func (l *Logger) Stop() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.prime(); err != nil {
		return err
	}
	l.drain()
	if err := l.t.Write(encodeFrame(opStop, 0, 1, []byte{0})); err != nil {
		return err
	}
	if _, err := l.readReport(timeoutCommand); err != nil {
		if errors.Is(err, ErrTimeout) {
			return ErrStopUnacknowledged
		}
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}

// ReadRaw performs one read-type request with the given operation code
// (3 = parameter space, 5 = status space) and returns the data bytes. It
// exists for protocol exploration; nothing in the UI depends on it.
func (l *Logger) ReadRaw(operation uint16, offset uint32, n byte) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.prime(); err != nil {
		return nil, err
	}
	l.drain()
	if err := l.t.Write(encodeFrame(op(operation), offset, n, nil)); err != nil {
		return nil, err
	}
	f, err := l.awaitReply(op(operation), timeoutGet)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), f.Data...), nil
}

// VerifySettings checks a fresh Info against what Configure wrote.
func VerifySettings(info Info, s Settings) error {
	var problems []string
	if info.Interval != s.Interval {
		problems = append(problems, fmt.Sprintf("interval %v, wanted %v", info.Interval, s.Interval))
	}
	wantMode := StartManual
	if s.StartImmediately {
		wantMode = StartImmediate
	}
	if info.Start.Mode != wantMode {
		problems = append(problems, fmt.Sprintf("start mode %v, wanted %v", info.Start.Mode, wantMode))
	}
	if info.Start.ButtonStop != s.ButtonStop || info.Start.SoftwareStop != s.SoftwareStop {
		problems = append(problems, fmt.Sprintf("stop bits button=%v software=%v, wanted %v/%v",
			info.Start.ButtonStop, info.Start.SoftwareStop, s.ButtonStop, s.SoftwareStop))
	}
	if len(problems) > 0 {
		return fmt.Errorf("elitech: settings did not persist: %v", problems)
	}
	return nil
}

// configBlocks are the six parameter ranges the factory software rewrites,
// in order. Packet 3 reproduces a factory off-by-one: 47 bytes sent under a
// declared length of 0x30.
var configBlocks = []struct {
	offset   uint32
	length   byte // bytes read
	written  int  // bytes actually sent
	declared byte // N byte in the SetParameter frame
}{
	{0x00, 0x30, 48, 0x30},
	{0x30, 0x30, 48, 0x30},
	{0x60, 0x30, 47, 0x30},
	{0x98, 0x34, 52, 0x34},
	{0xCC, 0x30, 48, 0x30},
	{0xFD, 0x2F, 47, 0x2F},
}

// connectPreamble is the 17-read sequence the factory software sends before
// the first GetRecord. Responses are discarded.
var connectPreamble = []struct {
	op     op
	offset uint32
	n      byte
}{
	{opGetParameter, 0x000000, 0x30}, {opGetParameter, 0x000030, 0x30}, {opGetParameter, 0x000060, 0x30},
	{opGetParameter, 0x000090, 0x08}, {opGetParameter, 0x000098, 0x34}, {opGetParameter, 0x0000CC, 0x30},
	{opRead05, 0x000000, 0x20}, {opRead05, 0x000070, 0x10}, {opRead05, 0x000080, 0x30},
	{opRead05, 0x0000B0, 0x30}, {opRead05, 0x0000E0, 0x30}, {opRead05, 0x000110, 0x30},
	{opRead05, 0x000140, 0x30}, {opRead05, 0x000020, 0x30}, {opRead05, 0x000050, 0x20},
	{opGetParameter, 0x00012C, 0x30}, {opGetParameter, 0x0000FD, 0x30},
}

// sendConnectPreamble replays the 17 reads the factory software issues
// before its first GetRecord. Replies are discarded.
func (l *Logger) sendConnectPreamble() {
	l.drain()
	for _, p := range connectPreamble {
		if err := l.t.Write(encodeFrame(p.op, p.offset, p.n, nil)); err != nil {
			return
		}
		l.readReport(timeoutGet)
		l.sleep(pausePreamble)
	}
}

// prime performs the two reads that put the device into a state where short
// parameter reads answer properly, and returns the identity they carry.
func (l *Logger) prime() (Identity, error) { return l.primeWith(timeoutGet) }

func (l *Logger) primeWith(timeout time.Duration) (Identity, error) {
	idb, err := l.getWith(0x00, 14, timeout)
	if err != nil {
		return Identity{}, fmt.Errorf("identity: %w", err)
	}
	pb, err := l.getWith(0x94, 2, timeout)
	if err != nil {
		return Identity{}, fmt.Errorf("protocol: %w", err)
	}
	l.protocol = pb[1]
	return Identity{
		Model:    uint16(idb[0])<<8 | uint16(idb[1]),
		Serial:   trimASCII(idb[2:14]),
		Protocol: pb[1],
	}, nil
}

// readCapacityCount reads the 0x42 block up to three times; a stopped or
// busy logger may answer with 0xFF sentinels at first.
func (l *Logger) readCapacityCount() (capacity, count uint32, ok bool) {
	for try := 0; try < 3; try++ {
		if try > 0 {
			l.sleep(pauseCapacityRetry)
		}
		b, err := l.get(0x42, 10)
		if err != nil {
			continue
		}
		if capacity, count, ok = decodeCapacityCount(b, l.protocol); ok {
			return capacity, count, true
		}
	}
	return 0, 0, false
}

// get reads n parameter bytes at offset.
func (l *Logger) get(offset uint32, n byte) ([]byte, error) { return l.getWith(offset, n, timeoutGet) }

func (l *Logger) getWith(offset uint32, n byte, timeout time.Duration) ([]byte, error) {
	l.drain()
	if err := l.t.Write(encodeFrame(opGetParameter, offset, n, nil)); err != nil {
		return nil, err
	}
	f, err := l.awaitReply(opGetParameter, timeout)
	if err != nil {
		return nil, err
	}
	// The device may answer with a wider range than asked for.
	start := int(offset) - int(f.Offset)
	if start < 0 || start+int(n) > len(f.Data) {
		return nil, fmt.Errorf("%w: reply covers %#x+%d, wanted %#x+%d", errBadFrame, f.Offset, len(f.Data), offset, n)
	}
	return f.Data[start : start+int(n)], nil
}

// set writes data at offset, declaring the given length, and checks the
// status reply.
func (l *Logger) set(offset uint32, declared byte, data []byte) error {
	l.drain()
	if err := l.t.Write(encodeFrame(opSetParameter, offset, declared, data)); err != nil {
		return err
	}
	f, err := l.awaitReply(opSetParameter, timeoutSet)
	if err != nil {
		return err
	}
	if f.Offset != offset || f.N != 1 || len(f.Data) < 1 {
		return fmt.Errorf("%w: unexpected SetParameter reply %+v", errBadFrame, f)
	}
	if f.Data[0] != 1 {
		return fmt.Errorf("%w: status %d at %#x", ErrRejected, f.Data[0], offset)
	}
	return nil
}

// getRecordPacket requests six records at index off. The device answers
// with an empty ACK first and the data a few milliseconds later, so nothing
// is drained between request and data and the ACK is skipped.
func (l *Logger) getRecordPacket(off uint32) (frame, error) {
	if err := l.t.Write(encodeFrame(opGetRecord, off, recordsPerPacket, nil)); err != nil {
		return frame{}, err
	}
	deadline := l.now().Add(timeoutRecord)
	sawAck := false
	for l.now().Before(deadline) || sawAck && !l.now().After(deadline.Add(timeoutRecordSlice)) {
		f, err := l.readReport(timeoutRecordSlice)
		if err != nil {
			continue // timeout or malformed report; keep waiting for the data report
		}
		if f.Op != opGetRecord {
			continue
		}
		if f.isRecordAck() {
			sawAck = true
			continue
		}
		if plausibleRecords(f, l.protocol) > 0 {
			return f, nil
		}
	}
	if sawAck {
		return frame{}, fmt.Errorf("%w: acknowledged but no data", ErrTimeout)
	}
	return frame{}, ErrTimeout
}

// recordWidth infers bytes per record from a data report: the 4-byte
// temperature+humidity format or the 8-byte timestamped format.
func recordWidth(f frame) int {
	if f.N > 0 && len(f.Data)/int(f.N) == 4 {
		return 4
	}
	return recordSize
}

// decodeRecordAt decodes one record of either width.
func decodeRecordAt(b []byte, protocol byte) (Record, error) {
	switch len(b) {
	case 4:
		return decodeRecordTH([4]byte(b))
	case recordSize:
		return decodeRecord([8]byte(b), protocol)
	}
	return Record{}, fmt.Errorf("elitech: unsupported record width %d", len(b))
}

func plausibleRecords(f frame, protocol byte) int {
	width := recordWidth(f)
	n := 0
	for i := 0; (i+1)*width <= len(f.Data) && i < recordsPerPacket; i++ {
		if _, err := decodeRecordAt(f.Data[i*width:(i+1)*width], protocol); err == nil {
			n++
		}
	}
	return n
}

// awaitReply reads until a report for the wanted operation arrives, skipping
// stale replies to earlier requests, or the timeout passes.
func (l *Logger) awaitReply(want op, timeout time.Duration) (frame, error) {
	deadline := l.now().Add(timeout)
	for {
		remaining := deadline.Sub(l.now())
		if remaining <= 0 {
			return frame{}, fmt.Errorf("%w after %v", ErrTimeout, timeout)
		}
		f, err := l.readReport(remaining)
		if err != nil {
			return frame{}, err
		}
		if f.Op == want {
			return f, nil
		}
	}
}

// readReport waits for one report and decodes it.
func (l *Logger) readReport(timeout time.Duration) (frame, error) {
	buf := make([]byte, reportSize)
	n, err := l.t.Read(buf, timeout)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return frame{}, fmt.Errorf("%w after %v", ErrTimeout, timeout)
	}
	if err != nil {
		return frame{}, err
	}
	return decodeFrame(buf[:n])
}

// drainTimeout is long enough for the kernel to hand over a queued report
// and short enough not to slow a parameter read noticeably. A zero deadline
// would not do: Go's poller fails an expired deadline before reading.
const drainTimeout = 5 * time.Millisecond

// drain discards stale reports.
func (l *Logger) drain() {
	buf := make([]byte, reportSize)
	for i := 0; i < 16; i++ {
		if _, err := l.t.Read(buf, drainTimeout); err != nil {
			return
		}
	}
}

func trimASCII(b []byte) string {
	end := len(b)
	for end > 0 && (b[end-1] == 0 || b[end-1] == ' ' || b[end-1] == 0xFF) {
		end--
	}
	return string(b[:end])
}
