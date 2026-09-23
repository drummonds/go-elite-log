package elitech

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"

	"git.bytestone.uk/hum3/go-elite-log/internal/hid"
)

// Service performs one complete operation per call, opening and closing
// the device each time so a replug between calls needs no special handling.
type Service struct {
	Roots hid.Roots
}

// Find lists attached loggers.
func (s Service) Find() ([]hid.Device, error) { return Find(s.Roots) }

// Info opens the logger and reads its state.
func (s Service) Info(dev hid.Device) (Info, error) {
	l, err := Open(dev)
	if err != nil {
		return Info{}, err
	}
	defer l.Close()
	return l.Info()
}

// Records opens the logger and downloads the last n records (0 = all).
func (s Service) Records(dev hid.Device, last int) ([]Record, error) {
	l, err := Open(dev)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	return l.Records(last)
}

// Stop opens the logger and sends the stop command.
func (s Service) Stop(dev hid.Device) error {
	l, err := Open(dev)
	if err != nil {
		return err
	}
	defer l.Close()
	return l.Stop()
}

// Configure writes settings (erasing records, see Logger.Configure), waits
// for the logger to settle, reopens it and verifies the settings persisted.
func (s Service) Configure(dev hid.Device, set Settings, confirmedCount uint32) (Info, error) {
	l, err := Open(dev)
	if err != nil {
		return Info{}, err
	}
	if err := l.Configure(set, confirmedCount); err != nil {
		l.Close()
		return Info{}, err
	}
	time.Sleep(time.Second)
	l, err = Open(dev)
	if err != nil {
		return Info{}, fmt.Errorf("reopen after configure: %w", err)
	}
	defer l.Close()
	info, err := l.Info()
	if err != nil {
		return Info{}, fmt.Errorf("read back after configure: %w", err)
	}
	return info, VerifySettings(info, set)
}

// WriteCSV writes records as index,time,temperature[,humidity],status with
// a 1-based index and RFC 3339 times. The humidity column is present when
// any record carries humidity. Non-measurement rows have empty values.
func WriteCSV(w io.Writer, recs []Record) error {
	withHumidity := false
	for _, r := range recs {
		withHumidity = withHumidity || r.HasHumidity
	}
	cw := csv.NewWriter(w)
	header := []string{"index", "time", "temperature", "status"}
	if withHumidity {
		header = []string{"index", "time", "temperature", "humidity", "status"}
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, r := range recs {
		temp, humidity := "", ""
		if r.Status == Measurement {
			temp = strconv.FormatFloat(r.Temperature, 'f', 1, 64)
			if r.HasHumidity {
				humidity = strconv.FormatFloat(r.Humidity, 'f', 1, 64)
			}
		}
		row := []string{strconv.Itoa(r.Index + 1), r.Time.Format(time.RFC3339), temp, r.Status.String()}
		if withHumidity {
			row = []string{strconv.Itoa(r.Index + 1), r.Time.Format(time.RFC3339), temp, humidity, r.Status.String()}
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
