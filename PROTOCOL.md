# Elitech RC-5 (USB 246c:9001, protocol 0x35) HID wire protocol

Reference for maintainers, distilled from the reverse-engineering work of
[elitech-hid-webui](https://github.com/andrasschad/elitech-hid-webui) and
[python-elitech](https://github.com/pasccom/python-elitech). Line numbers
refer to those projects' sources at the time of writing.

Extracted from `elitech-hid-webui/elitech-webui.py` ("primary", line numbers
below refer to it unless prefixed `pe:`) and cross-checked against
`python-elitech/elitech/src/{frames,commands,device,record,parameters}.py`
("python-elitech", `pe:`). Primary was tested end-to-end on real 246c:9001
hardware with protocol version 0x35 and reproduces flows statically
reverse-engineered from ElitechLog Win V8.0.5.0. python-elitech was tested on
RC-5+ (04d8:3005) and RC-51H; its VID/PID list is at `pe:device.py:31-53`.

Notation: all multi-byte hex is shown in transmit order. "u16 BE" = big-endian
unsigned 16-bit. Config addresses are byte offsets in the device's parameter
space (a flat address space read/written by GetParameter/SetParameter).

---

## 1. Device discovery and I/O

### 1.1 Locating the hidraw node (lines 77-146)

1. Enumerate `/sys/class/hidraw/hidraw*` (sorted).
2. For each, parse `/sys/class/hidraw/hidrawN/device/uevent` as `KEY=VALUE`
   lines (line 77).
3. `HID_ID` has the form `BUS:VVVVVVVV:PPPPPPPP` (three colon-separated
   fields). Vendor = last 4 hex chars of field 1, product = last 4 hex chars of
   field 2, compared upper-case (lines 93-100). Match `246C` / `9001`
   (lines 28-29).
4. Device node is `/dev/hidrawN`. Also read from uevent: `HID_NAME`
   (display name, fallback "Elitech RC-5"), `HID_UNIQ` (USB serial string),
   `HID_PHYS` (lines 122-133).

python-elitech instead walks the resolved sysfs path's parents looking for
`idVendor`/`idProduct` files (`pe:device.py:167-181`) and matches against its
own VID/PID table. Either approach yields the same node.

udev rule used by primary (line 2662):
`KERNEL=="hidraw*", ATTRS{idVendor}=="246c", ATTRS{idProduct}=="9001", MODE="0660", TAG+="uaccess"`

### 1.2 Report size and report ID

- Every packet written is exactly **64 bytes**; the frame is zero-padded to 64
  (`packet + bytes(64 - len(packet))`, lines 174, 745, 873, 1084, 1342, 1497).
- Every read is `os.read(fd, 64)` (lines 180, 264, 928, 1011, 1130, 1442, 1720).
- There is **no HID report-ID byte**: byte 0 of the written buffer is the
  frame header `0x33` itself, and byte 0 of what is read is `0x33`
  (`response[0:3] != bytes([0x33,0xCC,0x00])`, line 191).
- python-elitech reads the input/output report sizes from
  `/sys/class/hidraw/hidrawN/device/report_descriptor` and defaults to 64
  (`pe:device.py:118-134`). Nothing in either source uses a report other than
  64 bytes.

### 1.3 Open mode, timeouts, drain

- Primary opens with `os.open(path, O_RDWR | O_NONBLOCK)` (line 580 and every
  other flow) and waits for input with `select.select([fd],[],[],timeout)`.
- python-elitech opens `open(path, 'rb+')` (blocking) and does a blocking
  `read(inReportSize)` with no timeout (`pe:device.py:77, 143-146`).
- `drain_hid(fd)` (lines 177-184): loop `os.read(fd, 64)` until
  `BlockingIOError` or empty read. Primary calls this **before** every
  GetParameter (line 235), SetParameter (lines 886, 980) and generic command
  (line 1098), and once before the record preamble (lines 1362-1389).
  It deliberately does **not** drain between GetRecord packets (see §6.5).

Timeouts (seconds, select on the fd):

| Operation | Timeout | Line |
|---|---|---|
| GetParameter | 1.0 | 232 |
| GetParameter during post-record cleanup | 0.75 | 2047 |
| SetParameter | 1.0 | 883, 977 |
| Generic command (Format/Stop) | 2.0 | 1094, 1165 |
| Preamble read packet | 1.0 per packet | 1346 |
| GetRecord (total wait for data report) | 1.5, polled in slices of ≤0.35 | 1733, 1770 |

Sleeps:

| Where | Sleep | Line |
|---|---|---|
| After each preamble packet's response ("factory Thread.Sleep(80)") | 80 ms | 1456 |
| Between retries of the 0x42 capacity/count read | 60 ms | 1246 |
| After last GetRecord, before re-priming the parameter session | 50 ms | 2042 |
| After FormatCommand, keep fd open ("factory Thread.Sleep(500)") | 550 ms | 2556 |
| After closing fd post-configure, before reopening to verify | 1000 ms | 2564 |

No sleep is inserted between ordinary GetParameter requests.

---

## 2. Frame format

### 2.1 Request frame (all operations)

```
idx  0    1    2    3    4      5      6    7        8        9         10   11..
     33   CC   00   LEN  OP_LO  OP_HI  00   OFF_MID  OFF_LO   OFF_HI    N    DATA... CKSUM
```

| Byte | Meaning |
|---|---|
| 0-2 | Constant header `33 CC 00` |
| 3 | `LEN` = total frame length in bytes **including** the checksum = `11 + len(DATA) + 1` (lines 736, 1081; `pe:frames.py:126`) |
| 4-5 | Operation code, **little-endian u16** (`op & 0xFF`, `op >> 8`; lines 1062-1063, `pe:frames.py:125`) |
| 6 | Always `0x00` |
| 7 | `(offset >> 8) & 0xFF` — middle byte |
| 8 | `offset & 0xFF` — low byte |
| 9 | `(offset >> 16) & 0xFF` — high byte |
| 10 | `N`: for reads, number of bytes (GetParameter) or records (GetRecord) requested; for writes, declared data length |
| 11.. | `DATA` (absent for reads) |
| last | `CKSUM` = `sum(frame[0 : LEN-1]) & 0xFF` — byte-wise sum modulo 256 over every byte before it, header included |

The offset byte order `[mid, low, high]` is deliberate and identical in both
sources (lines 165-167; `pe:frames.py:125`). Offset range 0..0xFFFFFF.

Padding: append zero bytes to reach 64. The zeros are **not** included in the
checksum (the checksum is appended before padding).

Data length limit: primary's builders cap at 52 data bytes (`1 <= len <= 52`,
lines 155, 725); python-elitech also caps at 52 (`pe:frames.py:112-118`).
52 data bytes yields exactly a 64-byte frame (11 + 52 + 1).

Read frames (no DATA) are always 12 bytes with `LEN = 0x0C`.

### 2.2 Response frame

Same layout. `LEN` at byte 3, op at byte 4, offset in bytes 7-9 (decode
`(b9 << 16) | (b7 << 8) | b8`, line 218), data length at byte 10, data from
byte 11, checksum at index `LEN-1` = `sum(frame[0:LEN-1]) & 0xFF`
(lines 203-211). Trailing bytes after `LEN` up to 64 are padding and ignored.

Primary validation for a GetParameter response (`parse_response`, lines
187-225): `len >= 12`, header `33 CC 00`, `byte4 == 0x03`, `LEN >= 12`,
`LEN <= bytes read`, checksum matches, `11 + N <= LEN - 1`.

python-elitech is looser: it warns instead of failing on offset/length/checksum
mismatch and re-slices from the offset the device reported (`pe:frames.py:
153-187`). Primary requires the requested range to be contained in the returned
range and slices out the requested bytes (lines 269-283) — i.e. the device
**may return a wider range than requested** and an implementation must slice
by offset rather than assume `data[0]` is the requested address.

### 2.3 Worked frames (test vectors; 64-byte zero padding omitted)

| Purpose | Frame |
|---|---|
| GetParameter 0x000000, 14 bytes | `33 CC 00 0C 03 00 00 00 00 00 0E 1C` |
| GetParameter 0x000094, 2 bytes | `33 CC 00 0C 03 00 00 00 94 00 02 A4` |
| GetParameter 0x00012C, 48 bytes | `33 CC 00 0C 03 00 00 01 2C 00 30 6B` |
| GetRecord from record 0, 6 records | `33 CC 00 0C 01 00 00 00 00 00 06 12` |
| GetRecord from record 6, 6 records | `33 CC 00 0C 01 00 00 00 06 00 06 18` |
| FormatCommand (0x02C0), offset 0, data `00` | `33 CC 00 0D C0 02 00 00 00 00 01 00 CF` |
| StopCommand (0x03C0), offset 0, data `00` | `33 CC 00 0D C0 03 00 00 00 00 01 00 D0` |
| GetRecord ACK-only response observed on 0x35 hardware (line 1506) | `33 CC 00 0C 01 00 00 00 00 00 00 0C` then zeros |

Note the ACK has `N = 0` and `LEN = 0x0C`; its checksum 0x0C is consistent
with the rule above.

---

## 3. Commands

| Name | Op (u16) | Bytes 4-5 | Request | Response | Purpose |
|---|---|---|---|---|---|
| GetRecord | 0x0001 | `01 00` | offset = record index (0-based), N = record count (primary always 6) | data = N × 8-byte records at byte 11 (see §6); may be preceded by an ACK-only report | Read stored measurements |
| GetParameter | 0x0003 | `03 00` | offset = parameter address, N = byte count (1..52) | data = N bytes (or a superset range) at byte 11 | Read config/info |
| SetParameter | 0x0004 | `04 00` | offset, N = declared length, DATA | N = 1, byte 11 = status, 1 = success (lines 749-804) | Write config |
| "Read 0x05" | 0x0005 | `05 00` | offset, N = length, no data | read and discarded | Only used inside the factory connect preamble (§3.3). Semantics unknown; primary comments only "the factory program sends several 0x0005 read packets on every first connection" (lines 1277-1280). |
| FormatCommand | 0x02C0 | `C0 02` | offset 0, DATA = `00` | raw response, not parsed (line 1152-1166) | **Erases stored records**; sent at the end of the factory configure flow |
| StopCommand | 0x03C0 | `C0 03` | offset 0, DATA = `00` | raw, not parsed (`pe:commands.py:633-636`) | Stop recording. python-elitech only; primary never sends it (it is listed in the comment at line 1046 but unused). |

Primary uses no other opcode. python-elitech's enum is exactly these five
minus 0x0005 (`pe:frames.py:105-110`).

### 3.1 SetParameter response detail (lines 749-804)

```
33 CC 00 0D 04 00 00 OFF_MID OFF_LO OFF_HI 01 STATUS CKSUM
```
Validation: `len >= 13`, header, `byte4 == 0x04`, `13 <= LEN <= bytes read`,
checksum ok, response offset == requested offset, `byte10 == 1`,
`byte11 == 1` (any other status = device rejected the write).

### 3.2 Session prime ("prime_parameter_session", lines 303-376)

Two ordinary GetParameter reads, performed at the **start of every high-level
operation** and again at the **end of a record download**:

1. GetParameter offset 0x000000, 14 bytes (identity)
2. GetParameter offset 0x000094, 2 bytes (protocol)

Rationale quoted from the source (lines 309-321, translated): on the
246c:9001 / protocol 0x35 device, after the GetRecord download flow some short
GetParameter reads temporarily return only 0xFF sentinel data; performing these
two reads consistently restores normal parameter reading. It is read-only.

### 3.3 Factory connect preamble (lines 1282-1303, 1344-1458)

Sent once before the first GetRecord, after one explicit drain. 17 read-type
frames, each a 12-byte read frame (§2.1, `LEN = 0x0C`) with the given op,
waiting for one response report (timeout 1.0 s) then sleeping 80 ms. The
responses are logged and **discarded**.

| # | Op | Offset | N |
|---|---|---|---|
| 1 | 0x03 | 0x000000 | 0x30 |
| 2 | 0x03 | 0x000030 | 0x30 |
| 3 | 0x03 | 0x000060 | 0x30 |
| 4 | 0x03 | 0x000090 | 0x08 |
| 5 | 0x03 | 0x000098 | 0x34 |
| 6 | 0x03 | 0x0000CC | 0x30 |
| 7 | 0x05 | 0x000000 | 0x20 |
| 8 | 0x05 | 0x000070 | 0x10 |
| 9 | 0x05 | 0x000080 | 0x30 |
| 10 | 0x05 | 0x0000B0 | 0x30 |
| 11 | 0x05 | 0x0000E0 | 0x30 |
| 12 | 0x05 | 0x000110 | 0x30 |
| 13 | 0x05 | 0x000140 | 0x30 |
| 14 | 0x05 | 0x000020 | 0x30 |
| 15 | 0x05 | 0x000050 | 0x20 |
| 16 | 0x03 | 0x00012C | 0x30 |
| 17 | 0x03 | 0x0000FD | 0x30 |

Primary's comment (lines 1275-1280): "ElitechLog Win V8.0.5.0
DataFactory.GetListForParameter() exact, statically reverse-engineered
17-packet connect/read preamble … these are read operations; v16 reproduces
them before record download." python-elitech has no preamble and reads records
directly; whether the preamble is strictly required on 0x35 hardware is not
established in the source — primary introduced it in v16 while diagnosing
failed record reads and kept it.

---

## 4. Device info decoding (lines 578-706)

All values are read with GetParameter. Reads made by `read_device_info`:

| Address | Len | Field |
|---|---|---|
| 0x00 | 14 | identity |
| 0x94 | 2 | protocol |
| 0x20 | 1 | start-mode byte |
| 0x25 | 1 | device state |
| 0x26 | 2 | actual stop mode, battery |
| 0x28 | 7 | configuration time |
| 0x30 | 15 | start time, pad, stop time |
| 0x42 | 10 | capacity + record count |
| 0x4C | 2 | sampling interval |
| 0x88 | 7 | device time |

Decoding:

| Field | Address | Encoding | Lines |
|---|---|---|---|
| Model code | 0x00-0x01 | u16 BE | 596 |
| Serial number | 0x02-0x0D | 12 ASCII bytes, strip `\0`, trim | 598-603 |
| Protocol version | **0x95** (byte [1] of the 2-byte read at 0x94) | u8; tested device = 0x35 | 605; `pe:parameters.py:721` agrees (0x95) |
| Capacity (records) | 0x42-0x45 | u32 BE; see stop-state normalisation below | 426-505 |
| Record count | protocol >= 0x24: 0x46-0x49 u32 BE; else 0x48-0x49 u16 BE | | 474-478; `pe:parameters.py:708-709` (same rule, ≥0x24 variant commented out) |
| Sampling interval | 0x4C-0x4D | u16 BE, **units of 10 s**; 0 or FF FF = invalid | 508-528; `pe:parameters.py:712` (TimeSpanParameter ×10) |
| Start mode byte | 0x20 | bits 0-2 mode: 000 Immediate, 001 Manual, 010 Timer, 111 MAX; bit 3 = button stop enabled; bit 4 = software stop enabled | 543-551, 683-696; `pe:parameters.py:674-676` |
| Device state | 0x25 | raw u8 (undecoded; python-elitech only defines MAX=0x7F) | 613 |
| Actual stop mode | 0x26 bits 0-2 | 000 Manual, 011 Temporary, 111 none/MAX | 554-561 |
| Battery | 0x27 low nibble | 0..15; whole byte 0xFF = unknown | 618-622 |
| Configuration time | 0x28-0x2E | 7-byte datetime (§4.1) | 624 |
| Start time | 0x30-0x36 | 7-byte datetime | 625 |
| (pad) | 0x37 | ignored | `pe:parameters.py:703` |
| Stop time | 0x38-0x3E | 7-byte datetime | 626 |
| Device time | 0x88-0x8E | 7-byte datetime | 627 |

### 4.1 7-byte datetime (lines 379-423, 943-970)

```
[0] year - 2000   [1] month   [2] day-of-week   [3] day   [4] hour   [5] minute   [6] second
```
Byte 2 is .NET `DayOfWeek` (Sunday = 0 … Saturday = 6) when written by the
factory software; primary's `encode_official_elitech_datetime` writes it
(line 960: `(python_weekday + 1) % 7`). It is **ignored on decode**.
python-elitech writes 0x00 there (`pe:parameters.py:181`) — noted by primary as
a divergence from the factory program (lines 951-955).

Decode rules: all-zero → no value; all-0xFF → no value; any of bytes
0,1,3,4,5,6 == 0xFF → no value (stopped device fills runtime fields with 0xFF);
otherwise construct the date (invalid calendar date → no value).

### 4.2 Capacity/count sentinel normalisation (lines 426-505)

Observed 10-byte block at 0x42 while running: `00 00 7d 00 00 00 00 1b 00 00`
→ capacity 32000, count 27. Observed after a stop:
`ff ff 7d 00 00 00 00 1b ff ff`. Rules:

- capacity bytes 0x42-0x45 == `FF FF FF FF` → unknown.
- bytes 0x42-0x43 == `FF FF` and 0x44-0x45 != `FF FF` → capacity = u16 BE of
  0x44-0x45.
- otherwise capacity = u32 BE.
- record-count bytes (per protocol rule above) all 0xFF → unknown.
- count > capacity → treat both as unknown.

`read_record_count_from_fd` (lines 1186-1255) reads 0x94/2 once, then reads
0x42/10 up to 3 times (60 ms apart) until both values decode; otherwise it
fails rather than proceed with a destructive operation.

---

## 5. Config block layout

Addresses known from python-elitech's parameter table
(`pe:parameters.py:657-722`) plus primary. Types: bit fields are within the
byte at the given address, bit 0 = LSB. "W" = writable per python-elitech;
"I" = "immutable" (python-elitech zeroes or preserves it on write, see §7).

| Address | Size | Field | Encoding | W |
|---|---|---|---|---|
| 0x00 | 2 | model | u16 BE | – |
| 0x02 | 12 | serial-number | ASCII, NUL padded | – |
| 0x0E | 2 | ignored | `00 00` | |
| 0x10 | 13 | travel-number | ASCII, NUL padded | W |
| 0x1D | 1 | pdf-language | 0x00 en, 0x01 zh, 0x02 es, 0xFF MAX | W |
| 0x1E | bits 0-3 | product-properties | nibble | – |
| 0x1E | bit 4 | light-on | | W |
| 0x1E | bit 7 | allow-cycle (overwrite when full) | noted "does not work on RC-5+" | W |
| 0x1F | 1 | firmware-version | u8 | – I |
| 0x20 | bits 0-2 | start-mode | 000 Immediate, 001 Manual, 010 Timer, 111 MAX | W |
| 0x20 | bit 3 | button-stop | 1 = device can be stopped by button | W |
| 0x20 | bit 4 | software-stop | 1 = can be stopped by software | W |
| 0x20 | bit 5 | ignored | 0 | |
| 0x20 | bit 6 | repeat | new recording without reading previous | W |
| 0x20 | bit 7 | pause-allowed | | W |
| 0x21 | bit 0 | pdf-password-protected | | W |
| 0x21 | bit 1 | temperature-sensor-location | 0 Internal, 1 External | W |
| 0x21 | bit 2 | humidity-sensor-location | 0 Internal, 1 External | W |
| 0x21 | bit 3 | temperature-sensor-unit | 0 Celsius, 1 Fahrenheit | W |
| 0x21 | bit 4 | temperature-alarm-mode | marked "TODO two bits" (AlarmModes: 00 none, 01 single, 10 multiple) | W |
| 0x21 | bit 6 | humidity-alarm-mode | same caveat | W |
| 0x22 | bit 0 | high-temperature-alarm3-type | untested | W |
| 0x22 | bit 1 | high-temperature-alarm2-type | untested | W |
| 0x22 | bit 2 | high-temperature-alarm1-type | untested | W |
| 0x22 | bit 3 | low-temperature-alarm1-type | untested | W |
| 0x22 | bit 4 | low-temperature-alarm2-type | untested | W |
| 0x22 | bit 5 | low-temperature-alarm3-type | untested | W |
| 0x22 | bit 6 | high-humidity-alarm-type | untested | W |
| 0x22 | bit 7 | low-humidity-alarm-type | untested | W |
| 0x23 | bits 0-1 | exact-sensor-type | 00 none, 01 glycol bottle, 11 MAX | W |
| 0x23 | bits 4-7 | light-intensity | nibble | W |
| 0x24 | 12 (bytes 0x24 & 0x2F) | timezone | hours in 0x24 bits 0-4 (h > 12 → negative, -(24-h)); minutes in 0x2F | W |
| 0x25 | bits 0-6 | device-state | raw | – I |
| 0x26 | bits 0-2 | actual-stop-mode | 000 Manual, 011 Temporary, 111 MAX | – I |
| 0x26 | bit 3 | temporary-pdf | | W |
| 0x26 | bit 4 | display-time | "no effect" | W |
| 0x27 | bits 0-3 | battery-level | 0..15 | – I |
| 0x27 | bit 4 | csv | "does not work on RC-5+" | W |
| 0x28 | 7 | configuration-time | datetime §4.1 | W |
| 0x2F | 1 | timezone minutes | see 0x24 | W |
| 0x30 | 7 | start-time | datetime | – |
| 0x37 | 1 | ignored | | |
| 0x38 | 7 | stop-time | datetime | – |
| 0x3F | 1 | ignored | | |
| 0x40 | 2 | start-delay | u16 BE, delay before start in Timer mode (untested) | W |
| 0x42 | 4 | device-capacity | u32 BE records | – |
| 0x46 | 4 | record-number (protocol ≥ 0x24) | u32 BE | – |
| 0x48 | 2 | record-number (protocol < 0x24) | u16 BE | – |
| 0x4A | 2 | ignored | | |
| 0x4C | 2 | interval | u16 BE × 10 s | W |
| 0x80 | 6 | password (PDF) | ASCII | W |
| 0x88 | 7 | device-time | datetime | – I |
| 0x95 | 1 | protocol-version | u8 | – |

**Alarm threshold values are not located in either source.** python-elitech
defines a `FloatParameter` encoding (u16 BE; `0xFFFF` = NaN; `< 0x8000` →
value/10; else `-(v - 0x8000)/10`; `pe:parameters.py:460-490`) but no
parameter in its table uses it, so the alarm limit addresses are unknown.
Nothing about alarm limits, units C/F beyond the bit at 0x21.3, or the
0x98..0x12B region's contents is decoded in either source; primary reads and
rewrites those regions opaquely.

Primary's only decoded config fields are: 0x20 (mode + stop bits), 0x28
(config time), 0x4C (interval), 0x88 (device time), 0x42 block, 0x95
(`read_device_config`, lines 2112-2225). Interval fallback: if 0x4C/2 returns
`FF FF`, read 0x30/0x30 and take bytes `[0x1C:0x1E]` of it (same address via
a wider read; lines 2153-2172).

---

## 6. Records

### 6.1 Storage and addressing

- Record length: 8 bytes (line 1262; `pe:record.py:22`).
- GetRecord `offset` = **record index** (0-based), `N` = number of records
  (line 1461-1497: "offset = record ordinal (0, 6, 12, …); standard 8-byte
  records: count = 6; operation = 0x0001; byte[5] = 0 for normal record
  store").
- Records per packet: **6** (line 1263). python-elitech derives the same:
  `n = 51 // 8 = 6` (`pe:commands.py:525`).
- Response: header as §2.2 with op 0x01; record data starts at **byte 11**
  (line 1264), `6 × 8 = 48` bytes, so a full data report has `LEN = 0x3C`.
  Primary does not check the echoed offset/count bytes 7-10 of a data report
  (`validate_record_report_header`, lines 1530-1547, only checks header and
  op). python-elitech interprets bytes 7-10 of the response as the returned
  record offset/count and re-slices if they differ (`pe:frames.py:181-184`).
- Timestamps are **per record**, encoded inside each 8-byte record. Nothing
  is derived from start time + index × interval.

### 6.2 Record bit layout (lines 1549-1668; `pe:record.py:75-113`)

The 8 bytes form a little-endian 64-bit word `q` (byte 0 = bits 0-7).

| Byte | Bits | Field |
|---|---|---|
| 0 | 0-7 | flags (§6.3) |
| 1 | 0 | ignored (python-elitech warns if set) |
| 1 | 1 | temperature extension bit (protocol ≥ 0x23) — see note |
| 1 | 2-7 | second (0-59) |
| 2 | 0-6 | year − 2000 |
| 2 | 7 | month bit 0 |
| 3 | 0-2 | month bits 1-3 (month = 4 bits total) |
| 3 | 3-7 | day |
| 4 | 0-4 | hour |
| 4 | 5-7 | temperature bits 0-2 |
| 5 | 0-7 | temperature bits 3-10 |
| 6 | 0-5 | minute |
| 6 | 6-7 | humidity bits 0-1 (python-elitech only) |
| 7 | 0-7 | humidity bits 2-9 (python-elitech only) |

Primary's explicit expressions (lines 1583-1607):
```
second = (b1 >> 2) & 0x3F
year   = 2000 + (b2 & 0x7F)
month  = ((b3 & 0x07) << 1) | ((b2 >> 7) & 1)
day    = (b3 >> 3) & 0x1F
hour   = b4 & 0x1F
minute = b6 & 0x3F
temp_raw (protocol >= 0x23) = (((b1 >> 1) & 1) << 11) | (b5 << 3) | (b4 >> 5)
temp_raw (protocol <  0x23) =                            (b5 << 3) | (b4 >> 5)
temperature = temp_raw / 10.0, negated if flags & 0x08 (Sign1)
```
Units: tenths of a degree in whatever unit the device records; neither source
consults the C/F bit at 0x21.3 when decoding.

**Extension-bit divergence.** Primary places bit 1 of byte 1 at temperature
bit **11** (`<< 11`, line 1596). python-elitech ORs it into bit **10**
(`temperature |= ((q >> 9) & 0x01) << 10`, `pe:record.py:94`) — which
collides with bit 47 = byte 5 bit 7 already in the 11-bit field, so
python-elitech's version is self-inconsistent. Use primary's (12-bit) form.
Neither source shows a captured record with this bit set.

Humidity (python-elitech only, `pe:record.py:86, 103-106, 112`):
`hum_raw = (b6 >> 6) | (b7 << 2)` (10 bits); value = raw/10 %, negated if
flags & 0x40 (Sign2); raw 0 → no humidity. Primary ignores humidity on the
temperature-only RC-5 and treats bits 6-7 of byte 0 differently (§6.3).

Sentinel: a record of `FF FF FF FF FF FF FF FF` means "no data" (line 1571;
`pe:record.py:83`).

### 6.3 Flags (byte 0)

| Bit | Mask | Name | Meaning |
|---|---|---|---|
| 0 | 0x01 | Mark | user mark |
| 1 | 0x02 | Pause | record is a pause event |
| 2 | 0x04 | Stop | record is a stop event |
| 3 | 0x08 | Sign1 | temperature negative |
| 4 | 0x10 | Light | light event |
| 5 | 0x20 | Vibration | vibration event |
| 6 | 0x40 | Sign2 | protocol < 0x35: humidity negative (python-elitech) |
| 7 | 0x80 | Error | protocol < 0x35: error record |

Protocol ≥ 0x35 (primary, lines 1555-1567, 1577-1582): normal measurement
records have byte 0 == **0xC0**. Bits 6-7 are therefore treated as a
record-format marker and masked off (`flags = b0 & 0x3F`); only bits 0-5 are
interpreted. Interpreting 0xC0 with the old map would label every record
"Error".

Status decision table (lines 1626-1648):

| Pause (0x02) | Stop (0x04) | Error (0x80) and protocol < 0x35 | Status | Temperature reported |
|---|---|---|---|---|
| 1 | – | – | Pause | no |
| 0 | 1 | – | Stop | no |
| 0 | 0 | 1 | Error | no |
| 0 | 0 | 0 | Measurement | yes |

Mark/Light/Vibration are additive labels and do not change status.

### 6.4 Download algorithm (`read_device_records`, lines 1856-2108)

```
open fd (O_RDWR|O_NONBLOCK)
prime_parameter_session()                      # 0x00/14 then 0x94/2
protocol, capacity, count = read_record_count_from_fd()   # 0x94/2, 0x42/10 (≤3 tries)
if count == 0: done
wanted_start = 0 if limit == 0 else max(0, count - limit)      # "last N"
packet_offset = (wanted_start // 6) * 6
run_official_parameter_preamble()               # 17 frames, 80 ms apart
while packet_offset < count:
    report = get_record_data_report(packet_offset)         # §6.5
    valid = min(6, count - packet_offset)
    for i in 0..valid-1:
        raw = report[11 + 8*i : 19 + 8*i]
        parse; keep if packet_offset + i >= wanted_start
    packet_offset += 6
sleep 50 ms
prime_parameter_session(timeout 0.75)           # cleanup; failure is non-fatal
close fd
```
Records beyond `count` inside the last packet are ignored, not parsed.
Record numbering shown to users is 1-based (`record_index + 1`).

python-elitech instead reads open-ended and stops when an entire 6-record
packet is all 0xFF (`pe:commands.py:538`); it never consults the count field.
On 0x35 hardware the count field is authoritative in primary.

### 6.5 Per-packet exchange (`get_record_data_report`, lines 1729-1853)

1. Write the 64-byte GetRecord frame. **Do not drain first.**
2. Loop until 1.5 s elapsed, `select` in slices of ≤ 0.35 s:
   - Read 64 bytes. Skip (log) any report that is not 64 bytes or fails
     header/op check.
   - If the report is ACK-only (`33 CC 00 0C 01 00 00 00 00 00 00 0C` and all
     bytes 12..63 zero; lines 1499-1527) — note `saw_ack`, keep waiting, do
     **not** resend.
   - Otherwise count how many of the 6 slots at byte 11 parse to a valid
     non-sentinel record (lines 1671-1704). If ≥ 1, return this report.
     If 0, log and keep waiting.
3. Timeout → error (distinguishing "ACK but no data" from "no response").

Primary's comments (lines 1737-1748): on this hardware the device sends an
immediate ACK-only report and the data report a few ms later; draining before
the next request silently discarded that late report in v15, so v16 stopped
draining between record packets.

---

## 7. Configuration write (factory save flow, lines 2228-2660)

**Destructive:** the flow ends with FormatCommand (0x02C0), which erases the
record store. Primary refuses unless `record_count == 0` or the caller has
explicitly confirmed data loss and the confirmed count still matches the
device's count (lines 2413-2440). Also refuses if protocol < 0x16
(lines 2444-2448).

Steps (`apply_device_config`, lines 2320-2660):

1. Open fd; `prime_parameter_session()`; `read_record_count_from_fd()`.
2. Read the six blocks with GetParameter (lines 2241-2248, 2452-2457):

   | Block | Offset | Read length |
   |---|---|---|
   | 0 | 0x00 | 0x30 |
   | 1 | 0x30 | 0x30 |
   | 2 | 0x60 | 0x30 |
   | 3 | 0x98 | 0x34 |
   | 4 | 0xCC | 0x30 |
   | 5 | 0xFD | 0x2F |

   Not covered: 0x90-0x97 (protocol version lives here) and 0xFC.
3. Patch in memory (`_patch_compat_config_blocks`, lines 2251-2317):
   - `0x20 = (old & 0b11100000) | 0b001` (Manual start), `| 0x08` if button
     stop, `| 0x10` if software stop. Primary always writes Manual mode.
   - if sync-clock: `0x28..0x2E = now` in factory datetime form (§4.1, with
     day-of-week).
   - `0x4C..0x4D = u16 BE (interval_seconds / 10)`. Interval must be a whole
     number of minutes 1..1440 in primary's UI; the wire constraint is a
     multiple of 10 s, ≤ 0xFFFF × 10 s.
   - `0x88..0x8E = 00 00 00 00 00 00 00` (factory zeroes device-time in the
     0x60 packet; lines 2307-2313). Primary never writes a real value to
     0x88; "sync clock" means writing configuration-time only.
4. Send SetParameter frames **in this exact order** (lines 2496-2540), each
   followed by waiting for and validating the status response (§3.1):

   | # | Offset | Data bytes sent | Byte 10 (declared) | Byte 3 (LEN) |
   |---|---|---|---|---|
   | 1 | 0x00 | 48 | 0x30 | 0x3C |
   | 2 | 0x30 | 48 | 0x30 | 0x3C |
   | 3 | 0x60 | **47** (0x60..0x8E) | **0x30** | **0x3B** |
   | 4 | 0x98 | 52 | 0x34 | 0x40 |
   | 5 | 0xCC | 48 | 0x30 | 0x3C |
   | 6 | 0xFD | 47 | 0x2F | 0x3B |

   Packet 3 reproduces a factory off-by-one byte-for-byte: declared length
   0x30 but only 47 payload bytes, frame length 59, checksum at index 58
   (lines 807-873, 2511-2520). Address 0x8F is therefore never written.
5. Send FormatCommand `33 CC 00 0D C0 02 00 00 00 00 01 00 CF`, wait up to
   2 s for a response, ignore its contents (lines 1152-1166, 2542-2549).
6. Sleep 550 ms with the fd still open, then close (lines 2551-2559).
7. Sleep 1 s, reopen, verify by GetParameter: 0x20/1, 0x4C/2, 0x28/7, 0x88/7
   (lines 2564-2581). Checks: interval raw equal; `0x20 & 0x1F` equal;
   config time bytes equal (if sync-clock). Device time is logged only.

Primary's own note (README, "Adatbiztonság"): after this flow the device does
not start recording; on the tested RC-5 in Manual mode the user long-presses
the ▶ button. Earlier "plain SetParameter" attempts appeared to succeed but
reverted after a physical replug; the factory sequence (with FormatCommand and
the post-format hold) is what persisted.

### 7.1 python-elitech differences on write

- Same six compat ranges (`pe:commands.py:337-344`) but block 0x60 is written
  as a normal 48-byte frame, and **no FormatCommand** follows.
- Only writes to non-compat, minimal ranges by default; compat mode is opt-in.
- Sets configuration-time to now in compat mode (`pe:commands.py:388-392`).
- Zeroes "immutable" fields with byte/datetime types on write: firmware
  version 0x1F and device time 0x88..0x8E (`pe:commands.py:376-381` with
  `pe:parameters.py:157, 181`); enum/nibble immutables (0x25, 0x26, 0x27)
  keep their old bytes.
- Writes 0x00 as the datetime day-of-week byte.
- GetParameter quirk: a 1-byte read is turned into a 2-byte read at
  `offset - 1` (`pe:frames.py:189-192`). Primary issues 1-byte reads
  (0x20/1, 0x25/1) and they work on 0x35 hardware.
- SetParameter to python-elitech splits ranges to avoid rewriting
  configuration-time unless asked (`pe:commands.py:393-398`).

---

## 8. Quirks, ordering constraints, destructive operations

- **Destructive:** FormatCommand 0x02C0 erases records. SetParameter itself is
  not observed to erase, but primary always follows it with Format because
  that is the only sequence that persisted across replug on this firmware.
  StopCommand 0x03C0 stops recording (python-elitech only, effect on 0x35
  untested).
- Prime the parameter session (0x00/14, 0x94/2) at the start of every
  operation, and again after a record download; otherwise short reads may
  return all-0xFF blocks (lines 303-321, 2036-2058).
- Treat all-0xFF fields as "unknown", never as values (§4.1, §4.2). Stopped
  devices fill several runtime fields with 0xFF.
- Read 0x42/10 up to three times before trusting capacity/count; refuse
  destructive work if it never decodes.
- Drain stale reports before GetParameter/SetParameter/Format, but **never
  between GetRecord request and its data report**.
- Handle an ACK-only report before the GetRecord data report; keep waiting on
  the same request.
- 80 ms pause after each preamble packet; 550 ms open-hold after Format;
  1 s before reopening.
- The device may answer a GetParameter with a wider range than requested;
  slice by returned offset.
- Only one HID transaction at a time (primary serialises with a global lock,
  line 34).

---

## 9. Minimal sequences

### 9.1 Minimal read (info + records)

```
fd = open("/dev/hidrawN", O_RDWR|O_NONBLOCK)

# prime
get(0x000000, 14) -> model = u16be[0:2]; serial = ascii[2:14]
get(0x000094, 2)  -> protocol = [1]

# info
get(0x42, 10) -> capacity, count      (retry ≤3, 60 ms apart, sentinel rules)
get(0x4C, 2)  -> interval = u16be * 10 s
get(0x20, 1)  -> start byte
get(0x28, 7), get(0x30, 15), get(0x88, 7) -> datetimes

# records
if count > 0:
    drain
    for (op, off, n) in PREAMBLE: write read-frame(op, off, n); read 1 report (1 s); sleep 80 ms
    for off in 0, 6, 12, ... < count:
        write GetRecord(off, 6)               # no drain
        wait ≤1.5 s: skip ACK-only; accept first report with ≥1 plausible record
        for i in 0 .. min(6, count-off)-1: parse report[11+8i : 19+8i]
    sleep 50 ms
    get(0x000000, 14); get(0x000094, 2)      # re-prime
close(fd)
```
where `get(off, n)` = drain; write `33 CC 00 0C 03 00 00 mid lo hi n cksum`
(+pad); select ≤1 s; read 64; validate (§2.2); slice `[off-resp_off : +n]`.

### 9.2 Minimal configure (interval, stop bits, clock) — erases records

```
fd = open(...)
prime; protocol, capacity, count = read_record_count()
require count == 0 or explicit confirmation of count

blocks = { off: get(off, len) for (off,len) in [(0x00,0x30),(0x30,0x30),(0x60,0x30),(0x98,0x34),(0xCC,0x30),(0xFD,0x2F)] }
blocks[0x00][0x20] = (old & 0xE0) | 0x01 | (button?0x08:0) | (software?0x10:0)
blocks[0x00][0x28:0x2F] = [yy, mm, dow, dd, hh, mi, ss]      # if syncing
blocks[0x30][0x1C:0x1E] = u16be(interval_s / 10)
blocks[0x60][0x28:0x2F] = 00*7                                # 0x88..0x8E

set(0x00, blocks[0x00])                    # 48 B, LEN 0x3C
set(0x30, blocks[0x30])                    # 48 B, LEN 0x3C
set(0x60, blocks[0x60][:47], declared=0x30)# 47 B, LEN 0x3B
set(0x98, blocks[0x98])                    # 52 B, LEN 0x40
set(0xCC, blocks[0xCC])                    # 48 B, LEN 0x3C
set(0xFD, blocks[0xFD])                    # 47 B, LEN 0x3B
write 33 CC 00 0D C0 02 00 00 00 00 01 00 CF ; read (≤2 s, ignore)
sleep 550 ms; close(fd)
sleep 1 s; reopen; get(0x20,1), get(0x4C,2), get(0x28,7) and compare; close
```
`set(off, data)` = drain; write `33 CC 00 LEN 04 00 00 mid lo hi N data cksum`
(+pad); select ≤1 s; read 64; require `byte4==0x04`, echoed offset, `byte10==1`,
`byte11==1`.

---

## 10. Findings on protocol 0x54 (model 0x0134, temperature + humidity)

Observed on a logger reporting model `0x0134`, protocol `0x54`, serial
`EL26...`. Everything in §1-§5 and §7 behaved as described (identity,
capacity/count, interval, start byte, configure flow, verification).
Differences:

- **Records are 4 bytes**: `TT TT HH HH`, each a big-endian value in tenths
  (temperature °C, relative humidity %). `0xFFFF` = no reading; the sign
  convention is assumed to be python-elitech's `FloatParameter` (bit 15 =
  negative) and is unverified. A GetRecord reply carries 6 × 4 = 24 data
  bytes (`LEN 0x24`). Infer the width from `len(data) / N`.
- **No per-record timestamp.** Time = start time + index × interval. The
  parameter-space start time at 0x30 keeps the factory placeholder
  `2012-01-01 01:01:01` while recording; the real start time lives in the
  status space.
- **The op 0x05 "reads" are a status space**, not opaque. Offsets seen:

  | Offset | Bytes | Meaning |
  |---|---|---|
  | 0x00 | 1 | `01` while recording |
  | 0x08 | 7 | start time (datetime §4.1, day-of-week 0) |
  | 0x8A | 2 | record count, little-endian (verified < 256 only) |
  | 0x90 | 2 | max temperature (tenths) |
  | 0x98 | 2 | min temperature |
  | 0xA0 | 2 | mean temperature (tracks the running mean) |
  | 0xA2 | 2 | equal to 0xA0 so far; possibly mean kinetic temperature |
  | 0xB0 | 2 | max humidity |
  | 0xB8 | 2 | min humidity |
  | 0xC0 | 2 | mean humidity |
  | 0x109, 0x159 | 2 | `22 01`, unknown |

  There is no "current reading" field; the last record is the current value.
- The count in the parameter space (0x46) is live and matches 0x8A.
- **StopCommand 0x03C0 gets no reply from an idle logger** within 2 s.
- The logger does not record while on USB; recording starts with a 10 s
  button press after unplugging, even in Immediate start mode.
