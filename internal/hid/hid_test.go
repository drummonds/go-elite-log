package hid

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeSys builds a sysfs-shaped tree: <root>/class/hidraw/<node>/device/uevent.
func fakeSys(t *testing.T, nodes map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for node, uevent := range nodes {
		dir := filepath.Join(root, "class", "hidraw", node, "device")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "uevent"), []byte(uevent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const elitechUevent = "DRIVER=hid-generic\nHID_ID=0003:0000246C:00009001\nHID_NAME=FMSH MSC+HID\nHID_PHYS=usb-0000:65:00.3-1.1/input1\nHID_UNIQ=\nMODALIAS=hid:b0003g0001v0000246Cp00009001\n"
const yubikeyUevent = "DRIVER=hid-generic\nHID_ID=0003:00001050:00000402\nHID_NAME=Yubico YubiKey FIDO\n"
const i2cUevent = "HID_ID=0018:0000093A:00000255\nHID_NAME=UNIW0001:00 093A:0255\n"

func TestEnumerateMatchesVendorAndProduct(t *testing.T) {
	sys := fakeSys(t, map[string]string{
		"hidraw0": i2cUevent,
		"hidraw2": yubikeyUevent,
		"hidraw3": elitechUevent,
	})
	got, err := Enumerate(Roots{Sys: sys, Dev: "/dev"}, 0x246c, 0x9001)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 device, got %d: %+v", len(got), got)
	}
	d := got[0]
	if d.Path != "/dev/hidraw3" {
		t.Errorf("Path = %q, want /dev/hidraw3", d.Path)
	}
	if d.VendorID != 0x246c || d.ProductID != 0x9001 {
		t.Errorf("ids = %04x:%04x", d.VendorID, d.ProductID)
	}
	if d.Name != "FMSH MSC+HID" {
		t.Errorf("Name = %q", d.Name)
	}
	if d.Bus != 0x0003 {
		t.Errorf("Bus = %#x, want USB (3)", d.Bus)
	}
}

func TestEnumerateNoMatch(t *testing.T) {
	sys := fakeSys(t, map[string]string{"hidraw2": yubikeyUevent})
	got, err := Enumerate(Roots{Sys: sys, Dev: "/dev"}, 0x246c, 0x9001)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("want none, got %+v", got)
	}
}

func TestEnumerateMissingSysfsIsNotAnError(t *testing.T) {
	got, err := Enumerate(Roots{Sys: t.TempDir(), Dev: "/dev"}, 0x246c, 0x9001)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("want none, got %+v", got)
	}
}

func TestEnumerateSkipsMalformedUevent(t *testing.T) {
	sys := fakeSys(t, map[string]string{
		"hidraw1": "HID_ID=garbage\n",
		"hidraw3": elitechUevent,
	})
	got, err := Enumerate(Roots{Sys: sys, Dev: "/dev"}, 0x246c, 0x9001)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/dev/hidraw3" {
		t.Errorf("got %+v", got)
	}
}

func TestEnumerateIsSortedByNode(t *testing.T) {
	sys := fakeSys(t, map[string]string{
		"hidraw10": elitechUevent,
		"hidraw3":  elitechUevent,
	})
	got, err := Enumerate(Roots{Sys: sys, Dev: "/dev"}, 0x246c, 0x9001)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != "/dev/hidraw3" || got[1].Path != "/dev/hidraw10" {
		t.Errorf("got %+v", got)
	}
}
