// Command elitelog finds an attached Elitech USB temperature logger and
// either serves the web UI or dumps its state and records.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"git.bytestone.uk/hum3/go-elite-log/internal/elitech"
	"git.bytestone.uk/hum3/go-elite-log/internal/hid"
	"git.bytestone.uk/hum3/go-elite-log/internal/web"
)

// version is stamped by the linker (task install); otherwise it is derived
// from the build's VCS time or, failing that, the binary's own timestamp.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var rev, when string
		for _, kv := range info.Settings {
			switch kv.Key {
			case "vcs.revision":
				rev = kv.Value
			case "vcs.time":
				when = kv.Value
			}
		}
		if when != "" {
			if t, err := time.Parse(time.RFC3339, when); err == nil {
				when = t.Local().Format("2006-01-02 15:04")
			}
			if len(rev) > 7 {
				rev = rev[:7]
			}
			version = "dev " + when + " " + rev
			return
		}
	}
	if exe, err := os.Executable(); err == nil {
		if st, err := os.Stat(exe); err == nil {
			version = "dev built " + st.ModTime().Format("2006-01-02 15:04")
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	svc := elitech.Service{Roots: hid.DefaultRoots}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(svc, os.Args[2:])
	case "info":
		err = info(svc)
	case "records":
		err = records(svc, os.Args[2:])
	case "start":
		err = start(svc, os.Args[2:])
	case "stop":
		err = stop(svc)
	case "peek":
		err = peek(svc, os.Args[2:])
	case "version":
		fmt.Println("elitelog", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "elitelog:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  elitelog serve [-addr :1350]   web UI
  elitelog info                  print device state
  elitelog records [-last N]     print records as CSV
  elitelog start [-interval 10s] [-confirm N]
                                 erase the store and start recording now
  elitelog stop                  stop recording
  elitelog version`)
}

func serve(svc elitech.Service, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":1350", "listen address")
	fs.Parse(args)
	web.Version = "elitelog " + version
	shown := *addr
	if strings.HasPrefix(shown, ":") {
		shown = "localhost" + shown
	}
	log.Printf("elitelog %s serving on http://%s", version, shown)
	return http.ListenAndServe(*addr, web.NewHandler(svc))
}

func firstDevice(svc elitech.Service) (hid.Device, error) {
	devs, err := svc.Find()
	if err != nil {
		return hid.Device{}, err
	}
	if len(devs) == 0 {
		return hid.Device{}, fmt.Errorf("no Elitech logger found (USB %04x:%04x)", elitech.VendorID, elitech.ProductID)
	}
	return devs[0], nil
}

func info(svc elitech.Service) error {
	dev, err := firstDevice(svc)
	if err != nil {
		return err
	}
	in, err := svc.Info(dev)
	if err != nil {
		return err
	}
	fmt.Printf("node:       %s (%s)\n", dev.Path, dev.Name)
	fmt.Printf("model:      0x%04X\nserial:     %s\nprotocol:   0x%02X\n", in.Model, in.Serial, in.Protocol)
	if in.CountKnown {
		fmt.Printf("records:    %d of %d\n", in.RecordCount, in.Capacity)
	} else {
		fmt.Println("records:    unknown")
	}
	fmt.Printf("interval:   %v\nstart:      %v (button stop %v, software stop %v)\nbattery:    %d\n",
		in.Interval, in.Start.Mode, in.Start.ButtonStop, in.Start.SoftwareStop, in.Battery)
	fmt.Printf("configured: %v\nstarted:    %v\nstopped:    %v\nclock:      %v\n",
		in.ConfiguredAt, in.StartedAt, in.StoppedAt, in.DeviceTime)
	return nil
}

func records(svc elitech.Service, args []string) error {
	fs := flag.NewFlagSet("records", flag.ExitOnError)
	last := fs.Int("last", 0, "only the most recent N records (0 = all)")
	fs.Parse(args)
	dev, err := firstDevice(svc)
	if err != nil {
		return err
	}
	recs, err := svc.Records(dev, *last)
	if err != nil {
		return err
	}
	return elitech.WriteCSV(os.Stdout, recs)
}

func start(svc elitech.Service, args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	interval := fs.Duration("interval", 0, "sampling interval (default: keep the logger's current one)")
	confirm := fs.Uint("confirm", 0, "number of stored records you agree to erase")
	fs.Parse(args)
	dev, err := firstDevice(svc)
	if err != nil {
		return err
	}
	in, err := svc.Info(dev)
	if err != nil {
		return err
	}
	set := elitech.Settings{
		Interval:         in.Interval,
		StartImmediately: true,
		ButtonStop:       in.Start.ButtonStop,
		SoftwareStop:     true,
		SyncClock:        true,
	}
	if *interval > 0 {
		set.Interval = *interval
	}
	if in.RecordCount > 0 && uint32(*confirm) != in.RecordCount {
		return fmt.Errorf("the logger holds %d records which starting erases; rerun with -confirm %d", in.RecordCount, in.RecordCount)
	}
	after, err := svc.Configure(dev, set, uint32(*confirm))
	if err != nil {
		return err
	}
	fmt.Printf("started: mode %v, interval %v, started at %v\n", after.Start.Mode, after.Interval, after.StartedAt.Format(time.RFC3339))
	return nil
}

func stop(svc elitech.Service) error {
	dev, err := firstDevice(svc)
	if err != nil {
		return err
	}
	if err := svc.Stop(dev); err != nil {
		return err
	}
	in, err := svc.Info(dev)
	if err != nil {
		return err
	}
	fmt.Printf("stopped at %v, %d records\n", in.StoppedAt, in.RecordCount)
	return nil
}

// peek dumps raw bytes for protocol exploration.
func peek(svc elitech.Service, args []string) error {
	fs := flag.NewFlagSet("peek", flag.ExitOnError)
	operation := fs.Uint("op", 3, "operation code: 3 parameters, 5 status")
	offset := fs.Uint("offset", 0, "start offset")
	n := fs.Uint("len", 32, "bytes to read (max 52)")
	fs.Parse(args)
	dev, err := firstDevice(svc)
	if err != nil {
		return err
	}
	l, err := elitech.Open(dev)
	if err != nil {
		return err
	}
	defer l.Close()
	data, err := l.ReadRaw(uint16(*operation), uint32(*offset), byte(*n))
	if err != nil {
		return err
	}
	for i := 0; i < len(data); i += 16 {
		end := min(i+16, len(data))
		fmt.Printf("%02x:%04x  % X\n", *operation, uint32(*offset)+uint32(i), data[i:end])
	}
	return nil
}
