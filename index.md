# go-elite-log

Local web app and CLI for Elitech USB temperature data loggers (the FMSH
"MSC+HID" generation, USB id `246c:9001`, e.g. RC-5). Finds the attached
logger over its HID interface, graphs the recorded temperatures and exposes
the logger's controls (clock, sampling interval, start/stop settings).

Pure Go, no cgo: talks to `/dev/hidrawN` directly. Linux only.

## Install

```
go install git.bytestone.uk/hum3/go-elite-log/cmd/elitelog@latest
sudo cp 99-elitech.rules /etc/udev/rules.d/ && sudo udevadm control --reload
```

Replug the logger after installing the udev rule, then `elitelog serve` and
open http://localhost:1350.

## Screenshots

### Monitor

Live readings, reloading every 5 s.

![Monitor page](screenshots/monitor.svg)

### Records

The recorded temperature and humidity series, with CSV export.

![Records page](screenshots/records.png)

### Device

Logger state and settings: clock, sampling interval, stop permissions,
erase and start a new recording.

![Device page](screenshots/device.svg)

## Command line

```
elitelog serve        # web UI on http://localhost:1350
elitelog info         # print device state
elitelog records      # dump records as CSV (-last N for the newest N)
elitelog start        # erase the store and arm a new recording
elitelog stop         # send the stop command
elitelog peek         # raw parameter/status reads for protocol work
```

## Starting a recording

Save settings (or "Erase and start new recording"), then unplug the logger
and **press and hold its button for 10 seconds**. The logger does not
record while connected over USB; plug it back in to read and graph the
records.

## Protocol

The [HID wire protocol reference](PROTOCOL.html) documents frames,
commands, the config block layout and record decoding, for maintainers and
anyone porting to another Elitech model.

## Links

| | |
|---|---|
| Source | https://git.bytestone.uk/hum3/go-elite-log |
| Mirror (GitHub) | https://github.com/drummonds/go-elite-log |
| Protocol reference | [PROTOCOL.html](PROTOCOL.html) |
