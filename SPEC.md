# uv5r-ble-relay — Project Specification

A Go program that bridges [CHIRP](https://chirp.danplanet.com/) to a Baofeng UV-5R Mini radio over Bluetooth Low Energy (BLE) by exposing a local pseudo-terminal that CHIRP opens as a regular serial port.

```
┌────────┐   PTY (e.g. /dev/ttysNNN)   ┌──────────────┐   GATT FFE1 (BLE)   ┌──────────┐
│ CHIRP  │ ─────────────────────────── │  uv5r-relay  │ ─────────────────── │ UV-5R Mn │
└────────┘   raw bytes (transparent)   └──────────────┘   ≤20-byte writes   └──────────┘
```

The relay is **byte-oriented** but not entirely transparent: by default it inserts a small
**translation layer** so that mainline CHIRP's `UV5RMini` driver (which uploads in 64-byte
blocks) can talk to a radio that only accepts 128-byte writes over BLE. Identification and
downloads are passed through unchanged. The relay performs no encryption and no MTU-level
framing beyond chunking large writes:

1. Opens a PTY pair, prints/symlinks the slave path so CHIRP can connect.
2. Connects to the radio over BLE GATT.
3. Re-pairs CHIRP's 64-byte upload blocks into the 128-byte blocks the radio expects (see §3.6).
4. Forwards each resulting block to the radio (chunked to BLE MTU).
5. Forwards notifications from the radio back into the PTY.

`--passthrough` disables the translator for advanced users running a fork of CHIRP that
already emits 128-byte uploads.

---

## 1. Target

| Item               | Value                                                                                                                                                     |
|--------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|
| Radio              | Baofeng UV-5R Mini (also branded `5R Mini`, `GM-5R Mini`)                                                                                                 |
| Protocol reference | [zayator/chirp BT_COMMUNICATION_ANALYSIS.md](https://github.com/zayator/chirp/blob/9d4a1f798e4142bbbc445afa2def24cfe30b68a3/BT_COMMUNICATION_ANALYSIS.md) |
| CHIRP target       | **mainline** https://github.com/kk7ds/chirp (`UV5RMini` driver in `baofeng_uv17Pro.py`)                                                                   |
| OS                 | macOS 12+, Linux with BlueZ ≥ 5.50                                                                                                                        |
| Languages          | Go 1.22+                                                                                                                                                  |

---

## 2. BLE Profile

| Item                                | Value                                                                            |
|-------------------------------------|----------------------------------------------------------------------------------|
| Module type                         | HM-10 / TI CC2541 transparent UART                                               |
| Service UUID                        | `0000FFE0-0000-1000-8000-00805F9B34FB`                                           |
| Characteristic UUID (TX **and** RX) | `0000FFE1-0000-1000-8000-00805F9B34FB`                                           |
| Properties                          | Write Without Response + Notify (single characteristic)                          |
| Notify CCCD                         | `00002902-...` written to `0x0001` (handled by the BLE library)                  |
| Pairing                             | None                                                                             |
| Negotiated MTU                      | Default 23 (20-byte ATT payload). Relay requests MTU 247; falls back gracefully. |
| Write chunk size                    | `min(negotiatedMTU - 3, 244)`; default 20 bytes                                  |
| Advertising name                    | Contains `5R Mini`, `5RMINI`, or `UV-5R` (filter by service UUID is preferred)   |

### Notes
- CoreBluetooth (macOS) hides MAC; match by service UUID + name. Linux exposes MAC.
- Notifications arrive in arbitrary chunk sizes ≤MTU and must be concatenated **as-is** into the PTY read buffer; the CHIRP driver reads exact byte counts and tolerates fragmentation via its `timeout` attribute.

---

## 3. Wire Protocol (Transparent — for reference only)

The relay does **not** interpret these bytes. They are documented so tests can be authored.

### 3.1 Identification (issued before each session)
```
TX  "PROGRAMCOLORPROU"        (16 bytes)
RX  0x06                      (1 byte ACK / fingerprint)
TX  "F"                       (1 byte)
RX  16 bytes                  (firmware/config descriptor, e.g. 01 36 01 74 04 00 05 20 02 20 02 60 01 03 50 03)
TX  "M"                       (1 byte)
RX  15 bytes                  ("5RMINI  +L00000")
TX  "SEND!" + 20 bytes        (e.g. 53 45 4E 44 21 05 0D 01 01 01 04 11 08 05 0D 0D 01 11 0F 09 12 09 10 04 00)
RX  0x06                      (ACK)
```

### 3.2 Read block (download — 64-byte blocks)
```
TX  0x52  ADDR_HI  ADDR_LO  0x40
RX  0x52  ADDR_HI  ADDR_LO  0x40  <0x40 bytes of encrypted payload>
```

### 3.3 Write block (upload — 128-byte blocks via BLE)
```
TX  0x57  ADDR_HI  ADDR_LO  0x80  <0x80 bytes of encrypted payload>   (132 bytes total)
RX  0x06                                                              (ACK)
```

### 3.4 Memory map
| Segment   | Start    | Size                        |
|-----------|----------|-----------------------------|
| 1         | `0x0000` | `0x8040`                    |
| 2         | `0x9000` | `0x0040`                    |
| 3         | `0xA000` | `0x01C0`                    |
| **Total** |          | **`0x8240` (33 344 bytes)** |

### 3.5 Timeouts (set by CHIRP, not the relay)
- Identification: 5 s (BLE path)
- Per-block read: 1.5 s default
- Per-block ACK: 1.5 s default

The relay must not impose any timeout on the PTY side beyond the negotiated read deadline. It only buffers received notification bytes until CHIRP reads them.

### 3.6 Translation Layer (mainline-CHIRP compatibility)

Mainline CHIRP's `baofeng_uv17Pro.py:UV5RMini` driver uploads memory in 64-byte blocks
(`0x57 hi lo 0x40 + 64 bytes`) because Python's `struct.pack("b", length)` cannot encode
`0x80` as a signed byte. The radio over BLE only ACKs 128-byte uploads, so the relay
splices CHIRP's stream into 128-byte uploads as follows:

- **Identification** (`PROGRAMCOLORPROU`, `F`, `M`, `SEND!`+20): pass-through. Each command
  and its known reply length are forwarded verbatim. After `SEND!`'s `0x06` ACK the
  translator switches to **command phase**.
- **Reads** (`0x52 hi lo 0x40`): pass-through. The 4-byte echo + 64-byte payload returned
  by the radio is forwarded to CHIRP unchanged.
- **Writes** (`0x57 hi lo 0x40 + 64 bytes`): re-paired according to the segment table:
  - If the address is 128-byte aligned **and** a full pair fits inside the segment, the
    block is the **first half** of a pair: cache it and synthesize an `0x06` ACK to CHIRP
    immediately. Do not touch the inner transport.
  - If the address is the **second half** of a previously cached pair, combine the two
    halves into one 128-byte block and emit `0x57 hi lo 0x80 + 128 bytes` to the radio at
    the cached address. Forward the radio's `0x06` ACK back to CHIRP.
  - If the address is the **final / odd** block of its segment (a full 128-byte pair would
    overrun the segment end — e.g. the single block at `0x9000` or the last block at
    `0x8000` of segment 1), pad the trailing 64 bytes with `0xFF`, emit one 128-byte
    upload, and forward the ACK.

Memory segments (per §3.4) drive these decisions; helpers live in
[internal/protocol/protocol.go](uv5r-ble-relay/internal/protocol/protocol.go) and the state
machine itself in [internal/translate/translate.go](uv5r-ble-relay/internal/translate/translate.go).

For a full upload, mainline CHIRP sends `513 + 1 + 7 = 521` block writes and receives 521
ACKs from the relay; the radio sees `257 + 1 + 4 = 262` paired 128-byte uploads.

---

## 4. Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                       cmd/uv5r-relay                        │
│                          (main.go)                          │
└─────────────────┬───────────────────────────┬───────────────┘
                  │                           │
        ┌─────────▼─────────┐       ┌─────────▼─────────┐
        │  internal/ptylink │◀─────▶│  internal/relay   │◀─────▶┌──────────────────┐
        │  PTY master loop  │ chans │  goroutine pump   │ chans │ internal/bleconn │
        └───────────────────┘       └───────────────────┘       │  GATT client     │
                                                                │  + chunker       │
                                                                └────────┬─────────┘
                                                                         │
                                                          tinygo.org/x/bluetooth
```

### 4.1 Packages

| Package             | Responsibility                                                                                                           |
|---------------------|--------------------------------------------------------------------------------------------------------------------------|
| `cmd/uv5r-relay`    | CLI: flags, logging, lifecycle                                                                                           |
| `internal/ptylink`  | Open PTY, expose master `io.ReadWriteCloser`, return slave path & symlink                                                |
| `internal/bleconn`  | `Transport` interface; `BLETransport` implementation using `tinygo.org/x/bluetooth`; chunked writes; notification fan-in |
| `internal/relay`    | Bidirectional pump: PTY⇄Transport with backpressure, optional inter-write pacing, optional hex tracing                   |
| `internal/protocol` | Constants only (UUIDs, magic strings, segment table) — used by tests                                                     |

### 4.2 The `Transport` interface
```go
type Transport interface {
    io.ReadWriteCloser   // Read drains buffered notifications; Write sends bytes to radio
}
```
The relay code is written against this interface so it can be tested with an in-memory fake (`memTransport`) that emulates the real radio.

### 4.3 Goroutines (one process, no globals)
| Goroutine      | Loop                                                         |
|----------------|--------------------------------------------------------------|
| `pty→ble`      | `for { n,_ := pty.Read(buf); transport.Write(buf[:n]) }`     |
| `ble→pty`      | `for chunk := range notifyCh { pty.Write(chunk) }`           |
| signal handler | SIGINT/SIGTERM → cancel context → close PTY + disconnect BLE |

`ble.Adapter.SetNotificationHandler` (TinyGo) runs callbacks on the BLE thread; we copy bytes into a buffered channel to avoid blocking BLE I/O.

### 4.4 Chunking
On each `Write(data)`:
```
for len(data) > 0:
    n = min(len(data), chunkSize)
    char.WriteWithoutResponse(data[:n])
    data = data[n:]
    if pacing > 0: time.Sleep(pacing)
```
`chunkSize` defaults to 20; configurable via `--mtu` (sets `chunkSize = mtu - 3`). Inter-chunk pacing defaults to 0; `--pace 5ms` is exposed for sluggish radios.

### 4.5 Receive buffering
A `bytes.Buffer` guarded by a `sync.Mutex` plus a `sync.Cond` (or simply a buffered byte channel of size 64 KiB). `Read(p)` returns immediately if any data is buffered; otherwise blocks (PTY writer side blocks the goroutine, not the user — CHIRP just sees zero bytes returned and retries within its own timeout, which matches `pyserial` semantics on a PTS).

---

## 5. CLI

```
uv5r-relay [flags]

  --address string     BLE peer address or CoreBluetooth UUID (skip scan)
  --name string        Substring match on advertised name (default "5R")
  --service string     Service UUID hex (default "FFE0")
  --char string        Characteristic UUID hex (default "FFE1")
  --mtu int            Requested MTU (default 247; chunkSize = mtu-3)
  --pace duration      Inter-chunk delay (default 0)
  --scan-timeout dur   Scan window (default 15s)
  --link string        Symlink to create for the PTY slave (default "/tmp/ttyBLE")
  --trace              Hex-dump every byte both directions to stderr
  --fake               Use in-memory fake radio instead of BLE (for smoke testing)
  --version
```

On startup:
```
$ uv5r-relay
[relay] PTY slave: /dev/ttys005
[relay] symlink:   /tmp/ttyBLE -> /dev/ttys005
[relay] scanning for "5R" with service 0xFFE0…
[relay] found "5R Mini" at 6F2B…  RSSI -54
[relay] connected, MTU=185, chunk=182
[relay] waiting for CHIRP to open the port…
```

In CHIRP: **Radio → Download/Upload**, port = `/tmp/ttyBLE` (or the printed `/dev/ttysNNN`), model = "Baofeng UV-5R Mini".

Exit codes: `0` clean, `1` runtime error, `2` BLE not available, `3` device not found, `64` bad CLI args.

---

## 6. Dependencies

| Module                   | Purpose                    |
|--------------------------|----------------------------|
| `tinygo.org/x/bluetooth` | Cross-platform BLE central |
| `github.com/creack/pty`  | PTY pair                   |
| `github.com/spf13/pflag` | CLI parsing                |

Standard library only otherwise. No CGO beyond what TinyGo's bluetooth package brings (CoreBluetooth on macOS, BlueZ D-Bus on Linux — no cgo on Linux, cgo on macOS).

---

## 7. Test Plan (`internal/...` packages and `cmd/uv5r-relay/main_test.go`)

The BLE transport itself cannot be unit-tested without a radio, so the design isolates transport behind an interface. Tests cover:

### 7.1 `internal/bleconn/chunker_test.go`
- `TestChunker_SplitsIntoMTU` — given 132-byte payload and chunkSize=20, expect 7 writes of [20,20,20,20,20,20,12].
- `TestChunker_ExactBoundary` — 60 bytes / chunk 20 → 3 writes of 20.
- `TestChunker_Single` — 1 byte → 1 write of 1.
- `TestChunker_Zero` — 0 bytes → 0 writes.

### 7.2 `internal/bleconn/notifyqueue_test.go`
- `TestNotifyQueue_ReadAfterWrite` — push `[A,B,C]`; `Read(make([]byte,3))` → `[A,B,C]`.
- `TestNotifyQueue_ReadShorter` — push `[A,B,C,D]`; `Read(2)` → `[A,B]`; subsequent `Read(2)` → `[C,D]`.
- `TestNotifyQueue_ReadBlocksUntilData` — start `Read` in goroutine; assert it doesn't return; push; assert it does.
- `TestNotifyQueue_CloseUnblocksRead` — `Read` blocking; `Close()` → `Read` returns `(0, io.EOF)`.
- `TestNotifyQueue_FragmentedAccrual` — push three chunks of 20 bytes; one `Read(60)` returns all 60.

### 7.3 `internal/relay/relay_test.go` (uses the in-memory `memTransport`)
The `memTransport` fake implements the real radio's behavior:
- On the bytes `"PROGRAMCOLORPROU"`, replies `0x06`.
- On `"F"`, replies a fixed 16-byte descriptor.
- On `"M"`, replies `"5RMINI  +L00000"`.
- On `"SEND!"+20 bytes`, replies `0x06`.
- On `0x52 hi lo 0x40`, replies `0x52 hi lo 0x40 + 0x40` zero bytes.
- On `0x57 hi lo 0x80 + 0x80 bytes`, replies `0x06`.

Tests:
- `TestRelay_IdentificationHandshake` — relay between PTY and `memTransport`; from PTY side perform the full ident handshake; assert all replies arrive correctly.
- `TestRelay_FullUploadSequence` — drive the full upload (`MEM_STARTS`/`MEM_SIZES`, 128-byte blocks); count ACKs — must be exactly 261.
- `TestRelay_FullDownloadSequence` — drive a download with 64-byte reads; count bytes received — must be exactly `0x8240 + 4*521`.
- `TestRelay_FragmentedNotifications` — `memTransport` configured to deliver `0x52`-response in 20-byte slices; assert reader reassembles.
- `TestRelay_PtyClosePropagates` — close PTY master; relay shuts down and closes transport.
- `TestRelay_Shutdown` — cancel context; both goroutines exit within 1 s.

### 7.4 `internal/ptylink/ptylink_test.go`
- `TestOpen_ReturnsSlavePath` — `Open()`; assert `slave` exists and is a char device.
- `TestOpen_Roundtrip` — write to master, read same bytes from slave fd opened by test; and vice versa.
- `TestSymlink_CreatedAndOverwritten` — create symlink twice; second call overwrites.

### 7.5 `cmd/uv5r-relay` smoke test
- `TestMain_FakeMode` — run `main()` with `--fake --link /tmp/ttyBLE-test`; in-process open the symlink as a serial port; perform the ident handshake; assert success; SIGINT; assert clean exit.

### 7.6 Run
```
go test ./...
```
Expected: all green on macOS and Linux, no BLE hardware required (BLE-touching code is exercised only via the `Transport` interface).

---

## 8. Security & safety

- No network exposure, no listeners. PTY symlink is created with mode 0600 in `/tmp/`.
- BLE Write Without Response cannot brick the radio — the protocol expects exactly the frames CHIRP sends. The relay never injects bytes.
- On error (BLE disconnect, characteristic gone) the relay closes the PTY so CHIRP receives EOF and reports the failure rather than hanging.

---

## 9. Out of scope (v1)

- Windows support (BLE library supports it, but CHIRP's BLE detection on Windows uses COM names; needs separate work).
- BLE OTA firmware update.
- Multiple radios on one relay.
- Discovery UI.
