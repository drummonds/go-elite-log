// Package hid discovers and opens Linux hidraw devices without cgo.
//
// Discovery walks sysfs (/sys/class/hidraw/*/device/uevent) and matches the
// HID_ID line, which encodes bus:vendor:product. Opening is a plain
// read/write on the /dev/hidrawN node.
package hid

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Roots locates the sysfs and dev trees. Tests substitute a fake tree.
type Roots struct {
	Sys string // normally /sys
	Dev string // normally /dev
}

// DefaultRoots is the real system.
var DefaultRoots = Roots{Sys: "/sys", Dev: "/dev"}

// Device is one hidraw node identified by its USB vendor and product ids.
type Device struct {
	Path      string // /dev/hidrawN
	Bus       uint16 // 0x0003 for USB
	VendorID  uint16
	ProductID uint16
	Name      string // HID_NAME from uevent
}

// Enumerate returns every hidraw node whose HID_ID matches vendor:product,
// ordered by node number. A missing or empty sysfs tree yields no devices
// and no error; only an unreadable tree is an error.
func Enumerate(roots Roots, vendor, product uint16) ([]Device, error) {
	classDir := filepath.Join(roots.Sys, "class", "hidraw")
	entries, err := os.ReadDir(classDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hid: read %s: %w", classDir, err)
	}
	var found []Device
	for _, e := range entries {
		d, ok := readDevice(roots, classDir, e.Name())
		if ok && d.VendorID == vendor && d.ProductID == product {
			found = append(found, d)
		}
	}
	sort.Slice(found, func(i, j int) bool { return nodeNumber(found[i].Path) < nodeNumber(found[j].Path) })
	return found, nil
}

func readDevice(roots Roots, classDir, node string) (Device, bool) {
	f, err := os.Open(filepath.Join(classDir, node, "device", "uevent"))
	if err != nil {
		return Device{}, false
	}
	defer f.Close()
	d := Device{Path: filepath.Join(roots.Dev, node)}
	haveID := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch key {
		case "HID_ID":
			bus, vid, pid, err := parseHIDID(val)
			if err != nil {
				return Device{}, false
			}
			d.Bus, d.VendorID, d.ProductID = bus, vid, pid
			haveID = true
		case "HID_NAME":
			d.Name = val
		}
	}
	return d, haveID
}

// parseHIDID decodes "0003:0000246C:00009001" into bus, vendor, product.
func parseHIDID(s string) (bus, vendor, product uint16, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("hid: malformed HID_ID %q", s)
	}
	var vals [3]uint16
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 32)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("hid: malformed HID_ID %q: %w", s, err)
		}
		vals[i] = uint16(v)
	}
	return vals[0], vals[1], vals[2], nil
}

func nodeNumber(path string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "hidraw"))
	return n
}
