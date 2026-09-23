package elitech

import (
	"errors"
	"testing"
	"time"
)

func TestInfoDecodesIdentityAndState(t *testing.T) {
	d := newFakeDevice()
	d.addRecords(27)
	info, err := newTestLogger(d).Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != 0x2005 || info.Serial != "RC5A1234567" || info.Protocol != 0x35 {
		t.Errorf("identity = %+v", info.Identity)
	}
	if info.Capacity != 32000 || info.RecordCount != 27 || !info.CountKnown {
		t.Errorf("capacity/count = %d/%d known=%v", info.Capacity, info.RecordCount, info.CountKnown)
	}
	if info.Interval != 10*time.Minute {
		t.Errorf("Interval = %v", info.Interval)
	}
	if info.Start.Mode != StartManual || !info.Start.ButtonStop || !info.Start.SoftwareStop {
		t.Errorf("Start = %+v", info.Start)
	}
	if info.Battery != 10 {
		t.Errorf("Battery = %d", info.Battery)
	}
	if info.ConfiguredAt.IsZero() || info.StartedAt.IsZero() || !info.StoppedAt.IsZero() || info.DeviceTime.IsZero() {
		t.Errorf("times = configured %v started %v stopped %v device %v", info.ConfiguredAt, info.StartedAt, info.StoppedAt, info.DeviceTime)
	}
	// The session prime (0x00/14, 0x94/2) must precede everything else.
	w := d.written
	if len(w) < 2 || w[0].Offset != 0 || w[0].N != 14 || w[1].Offset != 0x94 || w[1].N != 2 {
		t.Errorf("session not primed: %+v", w[:2])
	}
}

func TestInfoRetriesCapacityBlock(t *testing.T) {
	d := newFakeDevice()
	d.capacityFFUntil = 2
	info, err := newTestLogger(d).Info()
	if err != nil {
		t.Fatal(err)
	}
	if !info.CountKnown || info.Capacity != 32000 {
		t.Errorf("want count after retries, got %+v", info)
	}
	d = newFakeDevice()
	d.capacityFFUntil = 3
	info, err = newTestLogger(d).Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.CountKnown {
		t.Error("three FF reads must leave count unknown")
	}
}

func TestInfoSlicesWideResponses(t *testing.T) {
	d := newFakeDevice()
	d.wideReads = true
	info, err := newTestLogger(d).Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Interval != 10*time.Minute || info.Protocol != 0x35 {
		t.Errorf("wide response not sliced: %+v", info)
	}
}

func TestRecordsReadsAllInOrder(t *testing.T) {
	d := newFakeDevice()
	d.addRecords(14)
	recs, err := newTestLogger(d).Records(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 14 {
		t.Fatalf("got %d records, want 14", len(recs))
	}
	for i, r := range recs {
		if r.Index != i {
			t.Errorf("record %d has Index %d", i, r.Index)
		}
		if r.Status != Measurement {
			t.Errorf("record %d status %v", i, r.Status)
		}
	}
	if recs[1].Temperature != 21.7+0.8 {
		t.Errorf("record 1 temperature %v", recs[1].Temperature)
	}
	// Three packets at offsets 0, 6, 12; preamble of 17 reads before the first.
	var gets []frame
	for _, f := range d.written {
		if f.Op == opGetRecord {
			gets = append(gets, f)
		}
	}
	if len(gets) != 3 || gets[0].Offset != 0 || gets[1].Offset != 6 || gets[2].Offset != 12 {
		t.Errorf("GetRecord offsets: %+v", gets)
	}
	ops := d.writtenOps()
	firstGet := -1
	for i, o := range ops {
		if o == opGetRecord {
			firstGet = i
			break
		}
	}
	preamble := 0
	for _, o := range ops[:firstGet] {
		if o == opRead05 {
			preamble++
		}
	}
	if preamble != 3 { // status-space reads only; no connect preamble when the logger answers
		t.Errorf("want 3 op-0x05 reads before first GetRecord, got %d", preamble)
	}
	// Re-prime after the download.
	last := d.written[len(d.written)-2:]
	if last[0].Op != opGetParameter || last[0].Offset != 0 || last[1].Offset != 0x94 {
		t.Errorf("session not re-primed after download: %+v", last)
	}
}

func TestRecordsLastN(t *testing.T) {
	d := newFakeDevice()
	d.addRecords(10)
	recs, err := newTestLogger(d).Records(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].Index != 7 || recs[2].Index != 9 {
		t.Errorf("got %+v", recs)
	}
	for _, f := range d.written {
		if f.Op == opGetRecord && f.Offset != 6 {
			t.Errorf("last-3 of 10 should only fetch packet at 6, fetched %d", f.Offset)
		}
	}
}

func TestRecordsEmptyStore(t *testing.T) {
	d := newFakeDevice()
	recs, err := newTestLogger(d).Records(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("got %d records", len(recs))
	}
	for _, o := range d.writtenOps() {
		if o == opGetRecord || o == opRead05 {
			t.Fatal("no record traffic expected on an empty store")
		}
	}
}

func TestConfigureRefusesUnconfirmedErase(t *testing.T) {
	d := newFakeDevice()
	d.addRecords(5)
	err := newTestLogger(d).Configure(Settings{Interval: time.Minute}, 0)
	if !errors.Is(err, ErrRecordsPresent) {
		t.Errorf("err = %v, want ErrRecordsPresent", err)
	}
	err = newTestLogger(d).Configure(Settings{Interval: time.Minute}, 4)
	if !errors.Is(err, ErrRecordsPresent) {
		t.Errorf("stale confirmation: err = %v, want ErrRecordsPresent", err)
	}
	if d.formats != 0 {
		t.Error("device was formatted")
	}
}

func TestConfigureRejectsBadInterval(t *testing.T) {
	d := newFakeDevice()
	for _, iv := range []time.Duration{0, 5 * time.Second, 15 * time.Second, 24*time.Hour + 10*time.Second} {
		if err := newTestLogger(d).Configure(Settings{Interval: iv}, 0); !errors.Is(err, ErrInvalidInterval) {
			t.Errorf("%v: err = %v, want ErrInvalidInterval", iv, err)
		}
	}
}

func TestConfigureWritesFactorySequence(t *testing.T) {
	d := newFakeDevice()
	d.addRecords(5)
	d.mem[0x20] = 0xE2 // upper bits must survive, mode becomes manual
	l := newTestLogger(d)
	now := time.Date(2026, 9, 23, 15, 0, 0, 0, time.Local)
	l.now = func() time.Time { return now }

	err := l.Configure(Settings{Interval: 2 * time.Minute, ButtonStop: true, SoftwareStop: false, SyncClock: true}, 5)
	if err != nil {
		t.Fatal(err)
	}
	var sets []frame
	for _, f := range d.written {
		if f.Op == opSetParameter {
			sets = append(sets, f)
		}
	}
	want := []struct {
		off  uint32
		n    byte
		data int
	}{{0x00, 0x30, 48}, {0x30, 0x30, 48}, {0x60, 0x30, 47}, {0x98, 0x34, 52}, {0xCC, 0x30, 48}, {0xFD, 0x2F, 47}}
	if len(sets) != len(want) {
		t.Fatalf("got %d SetParameter frames, want %d", len(sets), len(want))
	}
	for i, w := range want {
		if sets[i].Offset != w.off || sets[i].N != w.n || len(sets[i].Data) != w.data {
			t.Errorf("packet %d: off %#x n %#x data %d, want off %#x n %#x data %d",
				i+1, sets[i].Offset, sets[i].N, len(sets[i].Data), w.off, w.n, w.data)
		}
	}
	if d.formats != 1 {
		t.Errorf("formats = %d, want 1", d.formats)
	}
	ops := d.writtenOps()
	if ops[len(ops)-1] != opFormat {
		t.Errorf("Format must be the last frame, got %v", ops[len(ops)-1])
	}
	if d.mem[0x20] != 0xE0|0x01|0x08 {
		t.Errorf("start byte = %#x", d.mem[0x20])
	}
	if got := d.mem[0x4C:0x4E]; got[0] != 0 || got[1] != 12 {
		t.Errorf("interval bytes = % X, want 00 0C", got)
	}
	if got := d.mem[0x28:0x2F]; string(got) != string([]byte{26, 9, 3, 23, 15, 0, 0}) {
		t.Errorf("config time = % X", got)
	}
	if !isZero(d.mem[0x88:0x8F]) {
		t.Errorf("device time not zeroed: % X", d.mem[0x88:0x8F])
	}
	if !d.closed {
		t.Error("Configure must close the transport after the post-format hold")
	}
}

func TestConfigureWithoutSyncKeepsConfigTime(t *testing.T) {
	d := newFakeDevice()
	before := append([]byte(nil), d.mem[0x28:0x2F]...)
	if err := newTestLogger(d).Configure(Settings{Interval: time.Minute}, 0); err != nil {
		t.Fatal(err)
	}
	if string(d.mem[0x28:0x2F]) != string(before) {
		t.Errorf("config time changed: % X", d.mem[0x28:0x2F])
	}
}

func TestVerifySettings(t *testing.T) {
	info := Info{Interval: time.Minute, Start: StartSettings{Mode: StartManual, ButtonStop: true}}
	if err := VerifySettings(info, Settings{Interval: time.Minute, ButtonStop: true}); err != nil {
		t.Errorf("unexpected: %v", err)
	}
	if err := VerifySettings(info, Settings{Interval: 2 * time.Minute, ButtonStop: true}); err == nil {
		t.Error("interval mismatch not reported")
	}
	if err := VerifySettings(info, Settings{Interval: time.Minute, SoftwareStop: true}); err == nil {
		t.Error("stop bit mismatch not reported")
	}
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func TestStopSendsStopCommand(t *testing.T) {
	d := newFakeDevice()
	d.addRecords(3)
	if err := newTestLogger(d).Stop(); err != nil {
		t.Fatal(err)
	}
	if d.stops != 1 {
		t.Errorf("stops = %d", d.stops)
	}
	ops := d.writtenOps()
	if ops[0] != opGetParameter || ops[len(ops)-1] != opStop {
		t.Errorf("want prime then Stop, got %v", ops)
	}
}

func TestConfigureImmediateStart(t *testing.T) {
	d := newFakeDevice()
	if err := newTestLogger(d).Configure(Settings{Interval: time.Minute, StartImmediately: true, SoftwareStop: true}, 0); err != nil {
		t.Fatal(err)
	}
	if got := d.mem[0x20] & 0x1F; got != 0x10 {
		t.Errorf("start byte low bits = %#x, want immediate + software stop (0x10)", got)
	}
}

func TestVerifySettingsChecksStartMode(t *testing.T) {
	info := Info{Interval: time.Minute, Start: StartSettings{Mode: StartImmediate}}
	if err := VerifySettings(info, Settings{Interval: time.Minute, StartImmediately: true}); err != nil {
		t.Errorf("unexpected: %v", err)
	}
	if err := VerifySettings(info, Settings{Interval: time.Minute}); err == nil {
		t.Error("manual wanted but immediate read back should fail")
	}
}

func TestStopWithoutReplyIsUnacknowledged(t *testing.T) {
	d := newFakeDevice()
	d.silentStop = true
	err := newTestLogger(d).Stop()
	if !errors.Is(err, ErrStopUnacknowledged) {
		t.Errorf("err = %v, want ErrStopUnacknowledged", err)
	}
	if d.stops != 1 {
		t.Errorf("stops = %d", d.stops)
	}
}

func TestRecordsTHDeriveTimesFromStart(t *testing.T) {
	d := newFakeDevice()
	d.addRecordsTH(8)
	recs, err := newTestLogger(d).Records(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 8 {
		t.Fatalf("got %d records", len(recs))
	}
	start := time.Date(2026, 9, 23, 14, 58, 34, 0, time.Local)
	for i, r := range recs {
		if want := start.Add(time.Duration(i) * 10 * time.Minute); !r.Time.Equal(want) {
			t.Errorf("record %d time %v, want %v", i, r.Time, want)
		}
		if !r.HasHumidity || r.Humidity != 68.1 {
			t.Errorf("record %d humidity %+v", i, r)
		}
	}
	if recs[1].Temperature != 27.8 {
		t.Errorf("record 1 temperature %v", recs[1].Temperature)
	}
}

func TestInfoUsesStatusSpace(t *testing.T) {
	d := newFakeDevice()
	d.addRecordsTH(3)
	copy(d.mem[0x30:], []byte{12, 1, 0, 1, 1, 1, 1}) // factory placeholder start time
	info, err := newTestLogger(d).Info()
	if err != nil {
		t.Fatal(err)
	}
	if !info.Recording {
		t.Error("should be recording")
	}
	if want := time.Date(2026, 9, 23, 14, 58, 34, 0, time.Local); !info.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v, want status-space start %v", info.StartedAt, want)
	}
}

func TestRecordsFallsBackToConnectPreamble(t *testing.T) {
	d := newFakeDevice()
	d.addRecords(7)
	d.needsPreamble = true
	l := newTestLogger(d)
	// Freeze time so the first GetRecord's wait loop exits at once.
	base := time.Now()
	calls := 0
	l.now = func() time.Time { calls++; return base.Add(time.Duration(calls) * 100 * time.Millisecond) }
	recs, err := l.Records(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 7 {
		t.Errorf("got %d records", len(recs))
	}
	if !d.preambleSeen {
		t.Error("preamble was not sent")
	}
	gets := 0
	for _, o := range d.writtenOps() {
		if o == opGetRecord {
			gets++
		}
	}
	if gets != 3 { // 1 unanswered + 2 packets
		t.Errorf("GetRecord frames = %d, want 3", gets)
	}
}

func TestInfoReadsTemperatureUnit(t *testing.T) {
	d := newFakeDevice()
	info, err := newTestLogger(d).Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Fahrenheit || info.TemperatureUnit() != "°C" {
		t.Errorf("default should be Celsius: %+v", info.Fahrenheit)
	}
	d.mem[0x21] |= 0x08
	info, _ = newTestLogger(d).Info()
	if !info.Fahrenheit || info.TemperatureUnit() != "°F" {
		t.Error("bit 3 of 0x21 should mean Fahrenheit")
	}
}
