package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"git.bytestone.uk/hum3/go-elite-log/internal/elitech"
	"git.bytestone.uk/hum3/go-elite-log/internal/hid"
)

type stubLoggers struct {
	devices    []hid.Device
	info       elitech.Info
	infoErr    error
	records    []elitech.Record
	configured []elitech.Settings
	confirmed  []uint32
	configErr  error
	stops      int
	stopErr    error
}

func (s *stubLoggers) Find() ([]hid.Device, error) { return s.devices, nil }
func (s *stubLoggers) Stop(hid.Device) error       { s.stops++; return s.stopErr }
func (s *stubLoggers) Info(hid.Device) (elitech.Info, error) {
	return s.info, s.infoErr
}
func (s *stubLoggers) Records(_ hid.Device, last int) ([]elitech.Record, error) {
	if last > 0 && last < len(s.records) {
		return s.records[len(s.records)-last:], nil
	}
	return s.records, nil
}
func (s *stubLoggers) Configure(_ hid.Device, set elitech.Settings, confirmed uint32) (elitech.Info, error) {
	s.configured = append(s.configured, set)
	s.confirmed = append(s.confirmed, confirmed)
	return s.info, s.configErr
}

func newStub() *stubLoggers {
	base := time.Date(2026, 9, 23, 9, 0, 0, 0, time.Local)
	s := &stubLoggers{
		devices: []hid.Device{{Path: "/dev/hidraw3", Name: "FMSH MSC+HID", VendorID: 0x246c, ProductID: 0x9001}},
		info: elitech.Info{
			Identity:    elitech.Identity{Model: 0x2005, Serial: "RC5A1234567", Protocol: 0x35},
			Capacity:    32000,
			RecordCount: 4,
			CountKnown:  true,
			Interval:    10 * time.Minute,
			Start:       elitech.StartSettings{Mode: elitech.StartManual, ButtonStop: true},
			Battery:     10,
			DeviceTime:  base.Add(3 * time.Hour),
		},
	}
	for i := 0; i < 4; i++ {
		s.records = append(s.records, elitech.Record{Index: i, Time: base.Add(time.Duration(i) * 10 * time.Minute), Temperature: 20 + float64(i)*0.5})
	}
	s.records[2].Status = elitech.Pause
	return s
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
	return rr.Code, rr.Body.String()
}

func TestHomeShowsLoggerAndState(t *testing.T) {
	code, body := get(t, NewHandler(newStub()), "/")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"RC5A1234567", "/dev/hidraw3", "10m", "manual", "4", "0x35"} {
		if !strings.Contains(body, want) {
			t.Errorf("home page missing %q", want)
		}
	}
	if !strings.Contains(body, `href="records"`) {
		t.Error("home page has no link to records")
	}
}

func TestHomeWithNoLogger(t *testing.T) {
	s := newStub()
	s.devices = nil
	code, body := get(t, NewHandler(s), "/")
	if code != 200 || !strings.Contains(body, "No Elitech logger found") {
		t.Errorf("status %d body %q", code, body)
	}
}

func TestHomeReportsPermissionProblem(t *testing.T) {
	s := newStub()
	s.infoErr = hid.ErrPermission
	_, body := get(t, NewHandler(s), "/")
	if !strings.Contains(body, "udev") {
		t.Error("permission error should point at the udev rule")
	}
}

func TestRecordsPageHasChartAndTable(t *testing.T) {
	code, body := get(t, NewHandler(newStub()), "/records")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(body, "<svg") {
		t.Error("no chart")
	}
	for _, want := range []string{"20.0", "21.5", "pause", "09:00"} {
		if !strings.Contains(body, want) {
			t.Errorf("records page missing %q", want)
		}
	}
	if !strings.Contains(body, "records.csv") {
		t.Error("no CSV link")
	}
}

func TestRecordsPageSummaryExcludesNonMeasurements(t *testing.T) {
	_, body := get(t, NewHandler(newStub()), "/records")
	// min 20.0, max 21.5, mean of 20.0/20.5/21.5 = 20.67 (pause row excluded)
	if !strings.Contains(body, "20.67") {
		t.Error("mean should exclude pause records")
	}
}

func TestRecordsLastParameter(t *testing.T) {
	_, body := get(t, NewHandler(newStub()), "/records?last=2")
	if strings.Contains(body, "<td>1</td>") || !strings.Contains(body, "<td>3</td>") {
		t.Error("last=2 should show rows 3 and 4 only")
	}
}

func TestRecordsCSV(t *testing.T) {
	rr := httptest.NewRecorder()
	NewHandler(newStub()).ServeHTTP(rr, httptest.NewRequest("GET", "/records.csv", nil))
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type %q", ct)
	}
	lines := strings.Split(strings.TrimSpace(rr.Body.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines: %v", len(lines), lines)
	}
	if lines[0] != "index,time,temperature,status" {
		t.Errorf("header %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "1,2026-09-23T09:00:00") || !strings.HasSuffix(lines[1], ",20.0,measurement") {
		t.Errorf("row %q", lines[1])
	}
	if !strings.HasSuffix(lines[3], ",,pause") {
		t.Errorf("pause row should have empty temperature: %q", lines[3])
	}
}

func post(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, req)
	return rr
}

func TestConfigureRequiresConfirmationWhenRecordsPresent(t *testing.T) {
	s := newStub()
	rr := post(t, NewHandler(s), "/configure", url.Values{
		"interval_minutes": {"5"}, "button_stop": {"on"},
	})
	if len(s.configured) != 0 {
		t.Fatal("configure must not run without confirmation")
	}
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, "Please confirm") || !strings.Contains(body, `name="confirm" value="yes"`) {
		t.Errorf("status %d, body should ask to confirm: %q", rr.Code, body)
	}
	// The confirmation form must carry the submitted settings forward.
	if !strings.Contains(body, `name="interval_minutes" value="5"`) || !strings.Contains(body, `name="button_stop" value="on"`) {
		t.Error("confirmation form should repeat the settings as hidden fields")
	}
	if strings.Contains(body, "Type") {
		t.Error("no record-count typing")
	}
}

func TestConfigurePassesSettingsAndConfirmedCount(t *testing.T) {
	s := newStub()
	rr := post(t, NewHandler(s), "/configure", url.Values{
		"interval_minutes": {"5"}, "button_stop": {"on"}, "sync_clock": {"on"}, "confirm": {"yes"},
	})
	if len(s.configured) != 1 {
		t.Fatalf("configured %d times", len(s.configured))
	}
	got := s.configured[0]
	if got.Interval != 5*time.Minute || !got.ButtonStop || got.SoftwareStop || !got.SyncClock {
		t.Errorf("settings %+v", got)
	}
	if s.confirmed[0] != 4 {
		t.Errorf("confirmed count %d", s.confirmed[0])
	}
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Errorf("want redirect to /, got %d %q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestConfigureShowsDeviceError(t *testing.T) {
	s := newStub()
	s.info.RecordCount = 0
	s.configErr = errors.New("settings did not persist")
	rr := post(t, NewHandler(s), "/configure", url.Values{"interval_minutes": {"5"}})
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "did not persist") {
		t.Errorf("status %d body %q", rr.Code, rr.Body.String())
	}
}

func TestConfigureRejectsBadInterval(t *testing.T) {
	s := newStub()
	s.info.RecordCount = 0
	rr := post(t, NewHandler(s), "/configure", url.Values{"interval_minutes": {"0"}})
	if len(s.configured) != 0 || !strings.Contains(rr.Body.String(), "interval") {
		t.Errorf("bad interval accepted or not explained: %q", rr.Body.String())
	}
}

func TestHomeShowsLatestReading(t *testing.T) {
	s := newStub()
	s.records[3].Status = elitech.Pause // latest record is not a measurement
	s.records[2].Status = elitech.Measurement
	s.records[2].Temperature = 33.3
	_, body := get(t, NewHandler(s), "/")
	if !strings.Contains(body, "Latest reading") || !strings.Contains(body, "33.3") {
		t.Errorf("home page should show the latest measurement 33.3: %q", body)
	}
	if !strings.Contains(body, "09:20:00") {
		t.Error("latest reading should show its time")
	}
}

func TestHomeWithoutRecordsHasNoLatestReading(t *testing.T) {
	s := newStub()
	s.info.RecordCount = 0
	s.records = nil
	_, body := get(t, NewHandler(s), "/")
	if strings.Contains(body, "Latest reading") {
		t.Error("no records means no latest reading")
	}
}

func TestHomeEstimatesLoggingTime(t *testing.T) {
	// 32000 records at 10 min = 222d 5h 20m total; 4 used = 40m elapsed,
	// 31996 remaining = 222d 4h 40m.
	_, body := get(t, NewHandler(newStub()), "/")
	for _, want := range []string{"222d 5h 20m", "222d 4h 40m", "40m"} {
		if !strings.Contains(body, want) {
			t.Errorf("home page missing logging time %q", want)
		}
	}
}

func TestFormatSpan(t *testing.T) {
	cases := map[time.Duration]string{
		40 * time.Minute:                                "40m",
		5*time.Hour + 20*time.Minute:                    "5h 20m",
		222*24*time.Hour + 5*time.Hour + 20*time.Minute: "222d 5h 20m",
		2 * 24 * time.Hour:                              "2d",
		0:                                               "0m",
	}
	for d, want := range cases {
		if got := formatSpan(d); got != want {
			t.Errorf("formatSpan(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestNavBarShowsVersion(t *testing.T) {
	old := Version
	Version = "elitelog vTEST"
	defer func() { Version = old }()
	_, body := get(t, NewHandler(newStub()), "/")
	if !strings.Contains(body, `class="navbar-item">elitelog vTEST`) {
		t.Error("version not in the nav bar")
	}
}

func TestConfigureIntervalMinutesAndSeconds(t *testing.T) {
	s := newStub()
	s.info.RecordCount = 0
	post(t, NewHandler(s), "/configure", url.Values{"interval_minutes": {"0"}, "interval_seconds": {"30"}})
	post(t, NewHandler(s), "/configure", url.Values{"interval_minutes": {"2"}, "interval_seconds": {"10"}})
	if len(s.configured) != 2 || s.configured[0].Interval != 30*time.Second || s.configured[1].Interval != 2*time.Minute+10*time.Second {
		t.Errorf("configured %+v", s.configured)
	}
	rr := post(t, NewHandler(s), "/configure", url.Values{"interval_minutes": {"1"}, "interval_seconds": {"15"}})
	if len(s.configured) != 2 || !strings.Contains(rr.Body.String(), "10 seconds") {
		t.Error("seconds must be a multiple of 10")
	}
	rr = post(t, NewHandler(s), "/configure", url.Values{"interval_minutes": {"0"}, "interval_seconds": {"0"}})
	if len(s.configured) != 2 || !strings.Contains(rr.Body.String(), "at least 10 seconds") {
		t.Error("zero interval must be rejected")
	}
}

func TestSettingsFormPrefillsMinutesAndSeconds(t *testing.T) {
	s := newStub()
	s.info.Interval = 2*time.Minute + 30*time.Second
	_, body := get(t, NewHandler(s), "/")
	if !strings.Contains(body, `name="interval_minutes" min="0" max="1440" value="2"`) {
		t.Error("minutes field not prefilled with 2")
	}
	if !strings.Contains(body, `<option value="30" selected>`) {
		t.Error("seconds select not prefilled with 30")
	}
	if !strings.Contains(body, "<td>2m 30s</td>") {
		t.Error("interval in the state table should read 2m 30s")
	}
}

func TestRecordsPageHasStartStopControls(t *testing.T) {
	_, body := get(t, NewHandler(newStub()), "/records")
	if !strings.Contains(body, `action="/stop"`) || !strings.Contains(body, `action="/start"`) {
		t.Error("records page needs stop and start forms")
	}
	if strings.Contains(body, `confirm_erase`) {
		t.Error("no record-count field on the start form")
	}
}

func TestStopSendsStopAndReturnsToRecords(t *testing.T) {
	s := newStub()
	rr := post(t, NewHandler(s), "/stop", nil)
	if s.stops != 1 {
		t.Errorf("stops = %d", s.stops)
	}
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/records" {
		t.Errorf("want redirect to /records, got %d %q", rr.Code, rr.Header().Get("Location"))
	}
	s.stopErr = errors.New("no response from logger")
	rr = post(t, NewHandler(s), "/stop", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "no response from logger") {
		t.Errorf("stop failure should render on the records page: %d %q", rr.Code, rr.Body.String())
	}
}

func TestStartConfiguresImmediateWithCurrentSettings(t *testing.T) {
	s := newStub()
	rr := post(t, NewHandler(s), "/start", url.Values{"confirm": {"yes"}})
	if len(s.configured) != 1 {
		t.Fatalf("configured %d times", len(s.configured))
	}
	got := s.configured[0]
	if !got.StartImmediately || got.Interval != 10*time.Minute || !got.ButtonStop || !got.SoftwareStop || !got.SyncClock {
		t.Errorf("settings %+v", got)
	}
	if s.confirmed[0] != 4 {
		t.Errorf("confirmed %d", s.confirmed[0])
	}
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/records" {
		t.Errorf("want redirect to /records, got %d", rr.Code)
	}
}

func TestStartAsksForConfirmationFirst(t *testing.T) {
	s := newStub()
	rr := post(t, NewHandler(s), "/start", nil)
	body := rr.Body.String()
	if len(s.configured) != 0 || !strings.Contains(body, "Please confirm") || !strings.Contains(body, `action="/start"`) || !strings.Contains(body, `name="confirm" value="yes"`) {
		t.Errorf("first click must ask to confirm, not configure: %q", body)
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("confirmation prompt must not refresh away")
	}
	s.info.RecordCount = 0
	s.records = nil
	post(t, NewHandler(s), "/start", nil)
	if len(s.configured) != 1 {
		t.Error("with nothing to erase, start should not need confirming")
	}
}

func TestRecordsPageShowsArmedState(t *testing.T) {
	s := newStub()
	s.info.RecordCount = 0
	s.records = nil
	s.info.Start.Mode = elitech.StartImmediate
	_, body := get(t, NewHandler(s), "/records")
	if !strings.Contains(body, "hold the button for 10 s") {
		t.Error("armed logger should say to hold the button for 10 s")
	}
}

func TestStopUnacknowledgedIsShownAsWarning(t *testing.T) {
	s := newStub()
	s.stopErr = elitech.ErrStopUnacknowledged
	rr := post(t, NewHandler(s), "/stop", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "did not acknowledge") || strings.Contains(rr.Body.String(), "Stop failed") {
		t.Errorf("status %d body %q", rr.Code, rr.Body.String())
	}
}

func TestMonitorPageRefreshesAndShowsState(t *testing.T) {
	s := newStub()
	s.info.Recording = true
	s.info.StartedAt = time.Date(2026, 9, 23, 14, 58, 34, 0, time.Local)
	s.info.Status = elitech.Status{Recording: true, MaxTemperature: 28.3, MinTemperature: 26.3, MaxHumidity: 68.1, MinHumidity: 47.6}
	s.records[3].HasHumidity = true
	s.records[3].Humidity = 48.2
	code, body := get(t, NewHandler(s), "/monitor")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{`http-equiv="refresh"`, "Recording", "14:58:34", "21.5", "48.2", "28.3", "26.3", "68.1", "47.6", "4 of 32000"} {
		if !strings.Contains(body, want) {
			t.Errorf("monitor missing %q", want)
		}
	}
}

func TestMonitorStates(t *testing.T) {
	s := newStub()
	s.devices = nil
	_, body := get(t, NewHandler(s), "/monitor")
	if !strings.Contains(body, "Unplugged") {
		t.Error("no device should read Unplugged")
	}
	s = newStub()
	s.infoErr = errors.New("no response from logger")
	_, body = get(t, NewHandler(s), "/monitor")
	if !strings.Contains(body, "Not responding") {
		t.Error("info error should read Not responding")
	}
	s = newStub()
	s.info.StoppedAt = time.Date(2026, 9, 23, 15, 0, 0, 0, time.Local)
	_, body = get(t, NewHandler(s), "/monitor")
	if !strings.Contains(body, "Stopped") {
		t.Error("stopped time should read Stopped")
	}
	s = newStub()
	s.info.RecordCount = 0
	s.records = nil
	_, body = get(t, NewHandler(s), "/monitor")
	if !strings.Contains(body, "Not started") {
		t.Error("empty and not recording should read Not started")
	}
}

func TestRecordsPageShowsHumidity(t *testing.T) {
	s := newStub()
	for i := range s.records {
		s.records[i].HasHumidity = true
		s.records[i].Humidity = 40 + float64(i)
	}
	_, body := get(t, NewHandler(s), "/records")
	if strings.Count(body, "<svg") != 2 {
		t.Errorf("want a temperature and a humidity chart, got %d svg", strings.Count(body, "<svg"))
	}
	if !strings.Contains(body, "<th>Humidity</th>") || !strings.Contains(body, "<td>43.0</td>") {
		t.Error("table should have a humidity column")
	}
	rr := httptest.NewRecorder()
	NewHandler(s).ServeHTTP(rr, httptest.NewRequest("GET", "/records.csv", nil))
	lines := strings.Split(strings.TrimSpace(rr.Body.String()), "\n")
	if lines[0] != "index,time,temperature,humidity,status" || !strings.HasSuffix(lines[1], ",20.0,40.0,measurement") {
		t.Errorf("csv %q / %q", lines[0], lines[1])
	}
}

func TestTemperaturesCarryTheUnit(t *testing.T) {
	s := newStub()
	_, body := get(t, NewHandler(s), "/")
	if !strings.Contains(body, "21.5 °C") {
		t.Error("latest reading should show °C")
	}
	_, body = get(t, NewHandler(s), "/records")
	if !strings.Contains(body, "<th>Temperature (°C)</th>") || !strings.Contains(body, "Temperature (°C)</text>") && !strings.Contains(body, "°C") {
		t.Error("records page should label temperature in °C")
	}
	_, body = get(t, NewHandler(s), "/monitor")
	if !strings.Contains(body, "Temperature min / max (°C)") {
		t.Error("monitor should label extremes in °C")
	}
	s.info.Fahrenheit = true
	_, body = get(t, NewHandler(s), "/")
	if !strings.Contains(body, "21.5 °F") {
		t.Error("Fahrenheit loggers should show °F")
	}
}

func TestRecordsPageRefreshes(t *testing.T) {
	s := newStub()
	_, body := get(t, NewHandler(s), "/records")
	if !strings.Contains(body, `<meta http-equiv="refresh" content="9">`) {
		t.Error("records page should refresh every 9 s")
	}
	s.stopErr = errors.New("no response from logger")
	rr := post(t, NewHandler(s), "/stop", nil)
	if strings.Contains(rr.Body.String(), `http-equiv="refresh"`) {
		t.Error("a page showing an error must not refresh it away")
	}
}

func TestConfirmPromptCancelLinks(t *testing.T) {
	s := newStub()
	rr := post(t, NewHandler(s), "/start", nil)
	if !strings.Contains(rr.Body.String(), `href="records">Cancel`) {
		t.Error("start prompt should cancel back to records")
	}
	rr = post(t, NewHandler(s), "/configure", url.Values{"interval_minutes": {"5"}})
	if !strings.Contains(rr.Body.String(), `href="/">Cancel`) {
		t.Error("settings prompt should cancel back to the device page")
	}
}
