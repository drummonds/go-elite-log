package hid

import (
	"errors"
	"fmt"
	"log"
	"os"
	"time"
)

// trace hex-dumps every report to stderr when ELITELOG_TRACE is set.
var trace = os.Getenv("ELITELOG_TRACE") != ""

// Conn is an open hidraw node. Reports are exchanged as whole 64-byte
// buffers; the kernel strips and adds nothing for devices without report ids.
type Conn struct {
	f    *os.File
	path string
}

// ErrPermission is returned by Open when the node exists but is not
// accessible; the fix is a udev rule, so callers should say so.
var ErrPermission = errors.New("hid: permission denied (install the udev rule and replug the device)")

// Open opens the device node read/write.
func (d Device) Open() (*Conn, error) {
	f, err := os.OpenFile(d.Path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrPermission) {
		return nil, fmt.Errorf("%w: %s", ErrPermission, d.Path)
	}
	if err != nil {
		return nil, fmt.Errorf("hid: open %s: %w", d.Path, err)
	}
	return &Conn{f: f, path: d.Path}, nil
}

// Write sends one output report.
func (c *Conn) Write(report []byte) error {
	if trace {
		log.Printf("hid > % X", report)
	}
	n, err := c.f.Write(report)
	if err != nil {
		return fmt.Errorf("hid: write %s: %w", c.path, err)
	}
	if n != len(report) {
		return fmt.Errorf("hid: short write %d/%d to %s", n, len(report), c.path)
	}
	return nil
}

// Read waits up to timeout for one input report and returns it.
// A timeout is reported as os.ErrDeadlineExceeded.
func (c *Conn) Read(buf []byte, timeout time.Duration) (int, error) {
	if err := c.f.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, fmt.Errorf("hid: deadline on %s: %w", c.path, err)
	}
	n, err := c.f.Read(buf)
	if trace {
		if err != nil {
			log.Printf("hid < (%v)", err)
		} else {
			log.Printf("hid < % X", buf[:n])
		}
	}
	return n, err
}

// Close releases the node.
func (c *Conn) Close() error { return c.f.Close() }
