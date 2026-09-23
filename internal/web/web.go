// Package web is the lofigui user interface: a device page with the
// logger's state and controls, and a records page with the gogal chart.
package web

import (
	"errors"
	"fmt"
	"html"
	"html/template"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.bytestone.uk/hum3/gogal"
	"git.bytestone.uk/hum3/lofigui"

	"git.bytestone.uk/hum3/go-elite-log/internal/elitech"
	"git.bytestone.uk/hum3/go-elite-log/internal/hid"
)

// Loggers is what the pages need from the device layer.
type Loggers interface {
	Find() ([]hid.Device, error)
	Info(hid.Device) (elitech.Info, error)
	Records(dev hid.Device, last int) ([]elitech.Record, error)
	Configure(dev hid.Device, s elitech.Settings, confirmedCount uint32) (elitech.Info, error)
	Stop(hid.Device) error
}

const layout = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.title}}</title>
  {{if .refresh}}<meta http-equiv="refresh" content="{{.refresh}}">{{end}}
  <link rel="stylesheet" href="/assets/bulma.min.css">
</head>
<body>
  <nav class="navbar is-primary" role="navigation" aria-label="main navigation">
    <div class="navbar-brand"><span class="navbar-item has-text-weight-bold">Elitech logger</span></div>
    <div class="navbar-start">
      <a class="navbar-item" href="/">Device</a>
      <a class="navbar-item" href="records">Records</a>
      <a class="navbar-item" href="monitor">Monitor</a>
    </div>
    <div class="navbar-end"><span class="navbar-item">{{.version}}</span></div>
  </nav>
  <section class="section"><div class="container">{{.results}}</div></section>
  <footer class="footer"><div class="content has-text-centered"><p>{{.version}}</p></div></footer>
</body>
</html>`

// Version is shown in the footer; main sets it from the build.
var Version = "elitelog dev"

type site struct {
	loggers Loggers
	ctrl    *lofigui.Controller
	mu      sync.Mutex // lofigui's buffer is global
}

// NewHandler builds the site's routes.
func NewHandler(loggers Loggers) http.Handler {
	ctrl, err := lofigui.NewController(lofigui.ControllerConfig{TemplateString: layout, Name: "Elitech logger"})
	if err != nil {
		log.Fatalf("web: layout template: %v", err)
	}
	s := &site{loggers: loggers, ctrl: ctrl}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.home(w, "", "") })
	mux.HandleFunc("POST /configure", s.configure)
	mux.HandleFunc("GET /records", func(w http.ResponseWriter, r *http.Request) { s.records(w, r, "") })
	mux.HandleFunc("GET /monitor", s.monitor)
	mux.HandleFunc("POST /start", s.start)
	mux.HandleFunc("POST /stop", s.stop)
	mux.HandleFunc("GET /records.csv", s.recordsCSV)
	mux.HandleFunc("GET /favicon.ico", lofigui.ServeFavicon)
	mux.HandleFunc("GET /assets/bulma.min.css", lofigui.ServeBulma)
	return mux
}

// render runs fn against lofigui's buffer under the lock and writes the page.
func (s *site) render(w http.ResponseWriter, title string, fn func()) {
	s.renderRefreshing(w, title, 0, fn)
}

// renderRefreshing is render with a meta-refresh every refreshSeconds.
func (s *site) renderRefreshing(w http.ResponseWriter, title string, refreshSeconds int, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lofigui.Reset()
	fn()
	ctx := lofigui.TemplateContext{
		"title":   title,
		"results": template.HTML(lofigui.Buffer()),
		"version": Version,
	}
	if refreshSeconds > 0 {
		ctx["refresh"] = strconv.Itoa(refreshSeconds)
	}
	s.ctrl.RenderTemplate(w, ctx)
}

// monitorRefresh and recordsRefresh are how often those pages reload.
const (
	monitorRefresh = 5
	recordsRefresh = 9
)

// monitor is the live status page: plugged in or not, recording state,
// latest reading and the extremes, reloading itself every few seconds.
func (s *site) monitor(w http.ResponseWriter, r *http.Request) {
	s.renderRefreshing(w, "Monitor", monitorRefresh, func() {
		lofigui.HTML(`<h1 class="title">Monitor <span class="tag is-light">refreshes every ` + strconv.Itoa(monitorRefresh) + ` s</span></h1>`)
		dev, ok, err := s.firstDevice()
		if err != nil {
			renderMonitorState("is-danger", "Scan failed", err.Error())
			return
		}
		if !ok {
			renderMonitorState("is-dark", "Unplugged", "No Elitech logger on USB.")
			return
		}
		info, err := s.loggers.Info(dev)
		if err != nil {
			renderMonitorState("is-danger", "Not responding", html.EscapeString(dev.Path)+": "+html.EscapeString(err.Error()))
			return
		}
		state, colour, detail := recordingState(info)
		renderMonitorState(colour, state, detail)

		var latest []elitech.Record
		if info.CountKnown && info.RecordCount > 0 {
			latest, _ = s.loggers.Records(dev, 6)
		}
		renderLatestReading(latest, info.TemperatureUnit())
		renderMonitorFacts(info)
	})
}

// recordingState is the decision table for the headline state.
func recordingState(info elitech.Info) (state, colour, detail string) {
	switch {
	case !info.StoppedAt.IsZero():
		return "Stopped", "is-warning", "stopped " + timeText(info.StoppedAt)
	case info.Recording:
		return "Recording", "is-success", "since " + timeText(info.StartedAt) + " (" + formatSpan(time.Since(info.StartedAt)) + ")"
	case info.CountKnown && info.RecordCount > 0:
		return "Holding records", "is-info", "not recording; " + countText(info) + " stored"
	default:
		return "Not started", "is-light", "hold the button for 10 s after unplugging to start"
	}
}

func renderMonitorState(colour, state, detail string) {
	lofigui.HTML(`<div class="notification ` + colour + `"><p class="title is-3">` + html.EscapeString(state) + `</p><p>` + detail + `</p></div>`)
}

func renderMonitorFacts(info elitech.Info) {
	st := info.Status
	rows := [][]string{
		{"Records", countText(info)},
		{"Sampling interval", durationText(info.Interval)},
		{"Temperature min / max (" + info.TemperatureUnit() + ")", extremes(st.MinTemperature, st.MaxTemperature)},
		{"Humidity min / max", extremes(st.MinHumidity, st.MaxHumidity)},
		{"Device clock", timeText(info.DeviceTime)},
		{"Battery", batteryText(info.Battery)},
		{"Serial", info.Serial},
	}
	lofigui.Table(rows, lofigui.WithHeader([]string{"Field", "Value"}))
}

func extremes(lo, hi float64) string {
	if lo == 0 && hi == 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f / %.1f", lo, hi)
}

func (s *site) firstDevice() (hid.Device, bool, error) {
	devs, err := s.loggers.Find()
	if err != nil || len(devs) == 0 {
		return hid.Device{}, false, err
	}
	return devs[0], true, nil
}

// home is the device page: state and controls. errMsg and notice are shown
// above the content when a POST lands back here.
func (s *site) home(w http.ResponseWriter, errMsg, prompt string) {
	s.render(w, "Elitech logger", func() {
		lofigui.HTML(`<h1 class="title">Device</h1>`)
		if errMsg != "" {
			lofigui.HTML(`<div class="notification is-danger">` + html.EscapeString(errMsg) + `</div>`)
		}
		if prompt != "" {
			lofigui.HTML(prompt)
		}
		dev, ok, err := s.firstDevice()
		if err != nil {
			lofigui.HTML(`<div class="notification is-danger">Scanning USB failed: ` + html.EscapeString(err.Error()) + `</div>`)
			return
		}
		if !ok {
			lofigui.HTML(`<div class="notification is-warning">No Elitech logger found. Plug one in and <a href="/">rescan</a>.</div>`)
			return
		}
		info, err := s.loggers.Info(dev)
		if err != nil {
			renderDeviceError(dev, err)
			return
		}
		renderInfo(dev, info)
		if info.CountKnown && info.RecordCount > 0 {
			// The logger has no live-reading register; the newest stored
			// measurement is the closest thing. One packet is enough.
			recs, err := s.loggers.Records(dev, 6)
			if err != nil {
				lofigui.HTML(`<div class="notification is-warning">Could not read the latest record: ` + html.EscapeString(err.Error()) + `</div>`)
			} else {
				renderLatestReading(recs, info.TemperatureUnit())
			}
		}
		renderControls(info)
	})
}

// renderLatestReading shows the newest measurement among recs.
func renderLatestReading(recs []elitech.Record, unit string) {
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if r.Status != elitech.Measurement {
			continue
		}
		reading := fmt.Sprintf("%.1f %s", r.Temperature, unit)
		if r.HasHumidity {
			reading += fmt.Sprintf(` <span class="has-text-grey">&middot;</span> %.1f %%`, r.Humidity)
		}
		lofigui.HTML(fmt.Sprintf(`<div class="box"><p class="heading">Latest reading</p><p class="title">%s</p><p class="subtitle is-6">logged %s (%s ago)</p></div>`,
			reading, timeText(r.Time), formatSpan(time.Since(r.Time))))
		return
	}
}

// formatSpan renders a duration as days, hours and minutes, dropping
// leading zero units: "222d 5h 20m", "5h 20m", "40m".
func formatSpan(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Minute)
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	mins := int(d % time.Hour / time.Minute)
	var parts []string
	if days > 0 {
		parts = append(parts, strconv.Itoa(days)+"d")
	}
	if hours > 0 {
		parts = append(parts, strconv.Itoa(hours)+"h")
	}
	if mins > 0 || len(parts) == 0 {
		parts = append(parts, strconv.Itoa(mins)+"m")
	}
	return strings.Join(parts, " ")
}

func renderDeviceError(dev hid.Device, err error) {
	msg := html.EscapeString(err.Error())
	if errors.Is(err, hid.ErrPermission) {
		msg += `<br>Install the udev rule shipped with this program (<code>99-elitech.rules</code>) into <code>/etc/udev/rules.d/</code>, reload udev and replug the logger.`
	}
	lofigui.HTML(`<div class="notification is-danger">Found ` + html.EscapeString(dev.Path) + ` but could not talk to it: ` + msg + `</div>`)
}

func renderInfo(dev hid.Device, info elitech.Info) {
	rows := [][]string{
		{"Node", dev.Path},
		{"Name", dev.Name},
		{"Model", fmt.Sprintf("0x%04X", info.Model)},
		{"Serial", info.Serial},
		{"Protocol", fmt.Sprintf("0x%02X", info.Protocol)},
		{"Records", countText(info)},
		{"Sampling interval", durationText(info.Interval)},
		{"Logging time at this interval", loggingTimeText(info)},
		{"Start mode", info.Start.Mode.String()},
		{"Stop by button", yesNo(info.Start.ButtonStop)},
		{"Stop by software", yesNo(info.Start.SoftwareStop)},
		{"Battery", batteryText(info.Battery)},
		{"Configured at", timeText(info.ConfiguredAt)},
		{"Started at", timeText(info.StartedAt)},
		{"Stopped at", timeText(info.StoppedAt)},
		{"Device clock", timeText(info.DeviceTime)},
	}
	lofigui.Table(rows, lofigui.WithHeader([]string{"Field", "Value"}))
	lofigui.HTML(`<p><a class="button is-link" href="records">Show records</a> <a class="button" href="/">Rescan</a></p>`)
}

func renderControls(info elitech.Info) {
	minutes := int(info.Interval / time.Minute)
	seconds := int(info.Interval % time.Minute / time.Second)
	var secOptions strings.Builder
	for sec := 0; sec < 60; sec += 10 {
		sel := ""
		if sec == seconds {
			sel = " selected"
		}
		fmt.Fprintf(&secOptions, `<option value="%d"%s>%d</option>`, sec, sel, sec)
	}
	var confirm string
	if info.RecordCount > 0 {
		confirm = `<div class="notification is-warning">Saving settings erases the stored records (this is how the logger's own software works). Download them first. You will be asked to confirm.</div>`
	}
	lofigui.HTML(fmt.Sprintf(`<h2 class="title is-4">Settings</h2>
<form method="post" action="/configure">
<div class="field"><label class="label">Sampling interval</label>
<div class="field has-addons">
<div class="control"><input class="input" type="number" name="interval_minutes" min="0" max="1440" value="%d"></div>
<div class="control"><a class="button is-static">minutes</a></div>
<div class="control"><div class="select"><select name="interval_seconds">%s</select></div></div>
<div class="control"><a class="button is-static">seconds</a></div>
</div></div>
<div class="field"><label class="checkbox"><input type="checkbox" name="button_stop" %s> Allow stopping with the button</label></div>
<div class="field"><label class="checkbox"><input type="checkbox" name="software_stop" %s> Allow stopping by software</label></div>
<div class="field"><label class="checkbox"><input type="checkbox" name="sync_clock" checked> Write the computer's clock as the configuration time</label></div>
%s
<div class="field"><div class="control"><button class="button is-danger" type="submit">Save settings and erase records</button></div></div>
<p class="help">After saving, unplug the logger and press and hold its button for 10 seconds to begin a new recording.</p>
</form>`, minutes, secOptions.String(), checked(info.Start.ButtonStop), checked(info.Start.SoftwareStop), confirm))
}

// confirmForm is the "Please confirm" step: a warning that re-posts the
// original form values as hidden fields plus confirm=yes.
func confirmForm(action, cancel, warning string, values map[string][]string) string {
	var sb strings.Builder
	sb.WriteString(`<div class="notification is-warning"><p class="title is-5">Please confirm</p><p>` + html.EscapeString(warning) + ` This cannot be undone.</p><form method="post" action="` + action + `" class="mt-3">`)
	for name, vs := range values {
		if name == "confirm" {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&sb, `<input type="hidden" name="%s" value="%s">`, html.EscapeString(name), html.EscapeString(v))
		}
	}
	sb.WriteString(`<input type="hidden" name="confirm" value="yes"><div class="buttons"><button class="button is-danger" type="submit">Please confirm</button><a class="button" href="` + cancel + `">Cancel</a></div></form></div>`)
	return sb.String()
}

func (s *site) configure(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.home(w, "bad form: "+err.Error(), "")
		return
	}
	interval, err := parseInterval(r.FormValue("interval_minutes"), r.FormValue("interval_seconds"))
	if err != nil {
		s.home(w, err.Error(), "")
		return
	}
	set := elitech.Settings{
		Interval:     interval,
		ButtonStop:   r.FormValue("button_stop") != "",
		SoftwareStop: r.FormValue("software_stop") != "",
		SyncClock:    r.FormValue("sync_clock") != "",
	}
	dev, ok, err := s.firstDevice()
	if err != nil || !ok {
		s.home(w, "No logger found.", "")
		return
	}
	info, err := s.loggers.Info(dev)
	if err != nil {
		s.home(w, err.Error(), "")
		return
	}
	if info.RecordCount > 0 && r.FormValue("confirm") != "yes" {
		s.home(w, "", confirmForm("/configure", "/", "Saving these settings erases the stored records.", r.Form))
		return
	}
	if _, err := s.loggers.Configure(dev, set, info.RecordCount); err != nil {
		s.home(w, "Configure failed: "+err.Error(), "")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// parseInterval reads the minutes and seconds fields; an empty seconds
// field means zero. The logger stores intervals in 10-second units.
func parseInterval(minutesField, secondsField string) (time.Duration, error) {
	minutes, err := strconv.Atoi(strings.TrimSpace(minutesField))
	if err != nil || minutes < 0 || minutes > 1440 {
		return 0, errors.New("The sampling interval minutes must be a whole number from 0 to 1440.")
	}
	seconds := 0
	if f := strings.TrimSpace(secondsField); f != "" {
		seconds, err = strconv.Atoi(f)
		if err != nil || seconds < 0 || seconds >= 60 || seconds%10 != 0 {
			return 0, errors.New("The sampling interval seconds must be a multiple of 10 seconds below 60.")
		}
	}
	d := time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second
	if d < 10*time.Second {
		return 0, errors.New("The sampling interval must be at least 10 seconds.")
	}
	if d > 24*time.Hour {
		return 0, errors.New("The sampling interval must be at most 24 hours.")
	}
	return d, nil
}

func (s *site) loadRecords(r *http.Request) ([]elitech.Record, int, error) {
	last, _ := strconv.Atoi(r.URL.Query().Get("last"))
	dev, ok, err := s.firstDevice()
	if err != nil {
		return nil, last, err
	}
	if !ok {
		return nil, last, errors.New("no Elitech logger found")
	}
	recs, err := s.loggers.Records(dev, last)
	return recs, last, err
}

func (s *site) records(w http.ResponseWriter, r *http.Request, errMsg string) {
	s.recordsPage(w, r, errMsg, "")
}

func (s *site) recordsPage(w http.ResponseWriter, r *http.Request, errMsg, prompt string) {
	recs, last, err := s.loadRecords(r)
	var info elitech.Info
	if err == nil {
		if dev, ok, derr := s.firstDevice(); derr == nil && ok {
			info, _ = s.loggers.Info(dev)
		}
	}
	refresh := recordsRefresh
	if errMsg != "" || prompt != "" {
		refresh = 0 // keep the message on screen
	}
	s.renderRefreshing(w, "Records", refresh, func() {
		lofigui.HTML(`<h1 class="title">Records <span class="tag is-light">refreshes every ` + strconv.Itoa(recordsRefresh) + ` s</span></h1>`)
		if errMsg != "" {
			lofigui.HTML(`<div class="notification is-danger">` + html.EscapeString(errMsg) + `</div>`)
		}
		if prompt != "" {
			lofigui.HTML(prompt)
		}
		if err != nil {
			lofigui.HTML(`<div class="notification is-danger">` + html.EscapeString(err.Error()) + `</div>`)
			return
		}
		renderRecordingControls(info)
		lofigui.HTML(`<div class="buttons"><a class="button is-small" href="records">All</a><a class="button is-small" href="records?last=100">Last 100</a><a class="button is-small" href="records?last=500">Last 500</a><a class="button is-small" href="records?last=2000">Last 2000</a><a class="button is-small is-link" href="records.csv` + lastQuery(last) + `">Download CSV</a></div>`)
		if len(recs) == 0 {
			lofigui.HTML(`<div class="notification is-warning">The logger holds no records.</div>`)
			return
		}
		unit := info.TemperatureUnit()
		renderSummary(recs, unit)
		lofigui.HTML(chartSVG(recs, unit))
		if anyHumidity(recs) {
			lofigui.HTML(humidityChartSVG(recs))
		}
		renderRecordTable(recs, unit)
	})
}

// renderRecordingControls draws the stop and start forms. Starting is a
// reconfigure with immediate start, which erases the store, so it asks for
// the record count when there is anything to lose.
func renderRecordingControls(info elitech.Info) {
	status := "not started"
	switch {
	case !info.StoppedAt.IsZero():
		status = "stopped " + timeText(info.StoppedAt)
	case info.CountKnown && info.RecordCount > 0:
		status = "recording since " + timeText(recordingSince(info))
	case info.Start.Mode == elitech.StartImmediate:
		status = "armed: hold the button for 10 s to start"
	}
	lofigui.HTML(fmt.Sprintf(`<div class="box"><div class="level">
<div class="level-left"><div class="level-item"><div><p class="heading">Recording</p><p class="subtitle is-6">%s</p></div></div></div>
<div class="level-right">
<div class="level-item"><form method="post" action="/stop"><button class="button is-warning" type="submit">Stop recording</button></form></div>
<div class="level-item"><form method="post" action="/start"><button class="button is-danger" type="submit">Erase and start new recording</button></form></div>
</div></div>
<p class="help">Starting keeps the current interval and stop settings, sets the logger's clock and erases stored records. <strong>To start recording, unplug the logger and press and hold its button for 10 seconds.</strong> It does not record while on USB; plug it back in to read and graph the records.</p></div>`,
		status))
}

func recordingSince(info elitech.Info) time.Time {
	if !info.StartedAt.IsZero() && info.StartedAt.Year() > 2012 {
		return info.StartedAt
	}
	return info.ConfiguredAt
}

func (s *site) stop(w http.ResponseWriter, r *http.Request) {
	dev, ok, err := s.firstDevice()
	if err != nil || !ok {
		s.records(w, r, "No logger found.")
		return
	}
	if err := s.loggers.Stop(dev); err != nil {
		if errors.Is(err, elitech.ErrStopUnacknowledged) {
			s.records(w, r, "Stop command sent, but the logger did not acknowledge it. An idle logger stays silent; if it was recording, unplug and replug it and check the state above.")
			return
		}
		s.records(w, r, "Stop failed: "+err.Error())
		return
	}
	http.Redirect(w, r, "/records", http.StatusSeeOther)
}

func (s *site) start(w http.ResponseWriter, r *http.Request) {
	dev, ok, err := s.firstDevice()
	if err != nil || !ok {
		s.records(w, r, "No logger found.")
		return
	}
	info, err := s.loggers.Info(dev)
	if err != nil {
		s.records(w, r, err.Error())
		return
	}
	if info.CountKnown && info.RecordCount > 0 && r.FormValue("confirm") != "yes" {
		r.ParseForm()
		s.recordsPage(w, r, "", confirmForm("/start", "records", "Starting a new recording erases the stored records.", r.PostForm))
		return
	}
	confirmed := info.RecordCount
	set := elitech.Settings{
		Interval:         info.Interval,
		StartImmediately: true,
		ButtonStop:       info.Start.ButtonStop,
		SoftwareStop:     true,
		SyncClock:        true,
	}
	if set.Interval == 0 {
		set.Interval = time.Minute
	}
	if _, err := s.loggers.Configure(dev, set, confirmed); err != nil {
		s.records(w, r, "Start failed: "+err.Error())
		return
	}
	http.Redirect(w, r, "/records", http.StatusSeeOther)
}

func (s *site) recordsCSV(w http.ResponseWriter, r *http.Request) {
	recs, _, err := s.loadRecords(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="elitech-records.csv"`)
	if err := elitech.WriteCSV(w, recs); err != nil {
		log.Printf("web: csv: %v", err)
	}
}

func renderSummary(recs []elitech.Record, unit string) {
	n := 0
	minT, maxT, sum := math.Inf(1), math.Inf(-1), 0.0
	for _, r := range recs {
		if r.Status != elitech.Measurement {
			continue
		}
		n++
		sum += r.Temperature
		minT = math.Min(minT, r.Temperature)
		maxT = math.Max(maxT, r.Temperature)
	}
	if n == 0 {
		lofigui.HTML(`<p>No temperature measurements among these records.</p>`)
		return
	}
	lofigui.HTML(fmt.Sprintf(`<div class="level">
<div class="level-item has-text-centered"><div><p class="heading">Measurements</p><p class="title">%d</p></div></div>
<div class="level-item has-text-centered"><div><p class="heading">Min (%s)</p><p class="title">%.1f</p></div></div>
<div class="level-item has-text-centered"><div><p class="heading">Mean (%s)</p><p class="title">%.2f</p></div></div>
<div class="level-item has-text-centered"><div><p class="heading">Max (%s)</p><p class="title">%.1f</p></div></div>
<div class="level-item has-text-centered"><div><p class="heading">From</p><p class="subtitle">%s</p></div></div>
<div class="level-item has-text-centered"><div><p class="heading">To</p><p class="subtitle">%s</p></div></div>
</div>`, n, unit, minT, unit, sum/float64(n), unit, maxT, timeText(recs[0].Time), timeText(recs[len(recs)-1].Time)))
}

// chartSVG draws the measurement records as one gogal time-series line.
func chartSVG(recs []elitech.Record, unit string) string {
	var times []time.Time
	var values []float64
	for _, r := range recs {
		if r.Status == elitech.Measurement {
			times = append(times, r.Time)
			values = append(values, r.Temperature)
		}
	}
	if len(times) == 0 {
		return ""
	}
	chart := gogal.NewLineChart(
		gogal.WithTitle("Temperature ("+unit+")"),
		gogal.WithSize(960, 400),
		gogal.WithAxisMode(gogal.Temporal),
		gogal.WithTimeFormat(timeFormatFor(times[0], times[len(times)-1])),
		gogal.WithYFormat("%.1f"),
		gogal.WithYTitle(unit),
		gogal.WithGrid(true),
		gogal.WithLegend(false),
		gogal.WithTooltips(true),
	)
	chart.AddTimeSeries("Temperature", times, values)
	svg, err := chart.RenderString()
	if err != nil {
		return `<div class="notification is-danger">chart: ` + html.EscapeString(err.Error()) + `</div>`
	}
	return `<div class="box">` + svg + `</div>`
}

func anyHumidity(recs []elitech.Record) bool {
	for _, r := range recs {
		if r.HasHumidity {
			return true
		}
	}
	return false
}

// humidityChartSVG is a second chart rather than a second axis: humidity
// and temperature have different scales.
func humidityChartSVG(recs []elitech.Record) string {
	var times []time.Time
	var values []float64
	for _, r := range recs {
		if r.Status == elitech.Measurement && r.HasHumidity {
			times = append(times, r.Time)
			values = append(values, r.Humidity)
		}
	}
	if len(times) == 0 {
		return ""
	}
	chart := gogal.NewLineChart(
		gogal.WithTitle("Relative humidity"),
		gogal.WithSize(960, 300),
		gogal.WithAxisMode(gogal.Temporal),
		gogal.WithTimeFormat(timeFormatFor(times[0], times[len(times)-1])),
		gogal.WithYFormat("%.0f"),
		gogal.WithYTitle("%"),
		gogal.WithGrid(true),
		gogal.WithLegend(false),
		gogal.WithTooltips(true),
	)
	chart.AddTimeSeries("Humidity", times, values)
	svg, err := chart.RenderString()
	if err != nil {
		return `<div class="notification is-danger">chart: ` + html.EscapeString(err.Error()) + `</div>`
	}
	return `<div class="box">` + svg + `</div>`
}

func timeFormatFor(first, last time.Time) string {
	if last.Sub(first) < 36*time.Hour {
		return "15:04"
	}
	return "02 Jan 15:04"
}

func renderRecordTable(recs []elitech.Record, unit string) {
	withHumidity := anyHumidity(recs)
	var sb strings.Builder
	sb.WriteString(`<table class="table is-striped is-narrow is-fullwidth"><thead><tr><th>#</th><th>Time</th><th>Temperature (` + unit + `)</th>`)
	if withHumidity {
		sb.WriteString(`<th>Humidity</th>`)
	}
	sb.WriteString(`<th>Status</th><th>Events</th></tr></thead><tbody>`)
	for _, r := range recs {
		temp := ""
		if r.Status == elitech.Measurement {
			temp = fmt.Sprintf("%.1f", r.Temperature)
		}
		humidity := ""
		if withHumidity {
			if r.HasHumidity {
				humidity = fmt.Sprintf("<td>%.1f</td>", r.Humidity)
			} else {
				humidity = "<td></td>"
			}
		}
		var events []string
		if r.Mark {
			events = append(events, "mark")
		}
		if r.Light {
			events = append(events, "light")
		}
		if r.Vibration {
			events = append(events, "vibration")
		}
		fmt.Fprintf(&sb, `<tr><td>%d</td><td>%s</td><td>%s</td>%s<td>%s</td><td>%s</td></tr>`,
			r.Index+1, timeText(r.Time), temp, humidity, r.Status, strings.Join(events, ", "))
	}
	sb.WriteString(`</tbody></table>`)
	lofigui.HTML(sb.String())
}

func lastQuery(last int) string {
	if last > 0 {
		return "?last=" + strconv.Itoa(last)
	}
	return ""
}

// loggingTimeText estimates how long the store lasts at the current
// interval: total capacity, time already recorded and time remaining.
func loggingTimeText(info elitech.Info) string {
	if !info.CountKnown || info.Interval == 0 {
		return "unknown"
	}
	total := time.Duration(info.Capacity) * info.Interval
	elapsed := time.Duration(info.RecordCount) * info.Interval
	return fmt.Sprintf("%s total, %s recorded, %s remaining", formatSpan(total), formatSpan(elapsed), formatSpan(total-elapsed))
}

func countText(info elitech.Info) string {
	if !info.CountKnown {
		return "unknown"
	}
	return fmt.Sprintf("%d of %d", info.RecordCount, info.Capacity)
}

func durationText(d time.Duration) string {
	if d == 0 {
		return "unknown"
	}
	var parts []string
	if h := int(d / time.Hour); h > 0 {
		parts = append(parts, strconv.Itoa(h)+"h")
	}
	if m := int(d % time.Hour / time.Minute); m > 0 {
		parts = append(parts, strconv.Itoa(m)+"m")
	}
	if sec := int(d % time.Minute / time.Second); sec > 0 {
		parts = append(parts, strconv.Itoa(sec)+"s")
	}
	return strings.Join(parts, " ")
}

func timeText(t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	return t.Format("2006-01-02 15:04:05")
}

func batteryText(level int) string {
	if level < 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d / 15", level)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func checked(b bool) string {
	if b {
		return "checked"
	}
	return ""
}
