# uv5r-ble-relay

A Go program that bridges [CHIRP](https://chirp.danplanet.com/) (mainline, kk7ds/chirp) to a **Baofeng UV-5R Mini** radio over Bluetooth Low Energy, without modifying CHIRP at all.

> **Disclaimer:** All code in this project was written by AI (GitHub Copilot / Claude Opus 4.7 & Sonnet 4.6) and tested by a human. Use at your own risk - it could brick your radio. Always back up your radio's configuration before writing to it.

---

## How it was created

The UV-5R Mini uses a BLE-to-UART bridge chip (HM-10 / TI CC2541) to expose a "transparent serial" link over GATT. CHIRP can already program the radio over USB, but has no BLE support in its mainline build.

The approach:

1. **Research** — The BLE protocol was identified via [zayator's BT_COMMUNICATION_ANALYSIS.md](https://github.com/zayator/chirp/blob/9d4a1f798e4142bbbc445afa2def24cfe30b68a3/BT_COMMUNICATION_ANALYSIS.md), which documents the GATT service (`0xFFE0`) and characteristic (`0xFFE1`), write type (Write Without Response), the 20-byte BLE chunk limit, and the full programming handshake (`PROGRAMCOLORPROU` → `F` → `M` → `SEND!`).

2. **Protocol gap** — CHIRP's mainline `UV5RMini` driver sends 64-byte upload blocks (`0x57 addr 0x40 + 64 bytes`), but the radio over BLE only acknowledges 128-byte uploads. Downloads (reads) use 64-byte blocks on both sides.

3. **Design** — Rather than patching CHIRP, the relay:
   - Creates a **PTY** (pseudo-terminal) pair. CHIRP opens the slave end (`/dev/ttysNNN` or `/tmp/ttyBLE`) as a normal serial port.
   - Connects to the radio as a **BLE central** using `tinygo.org/x/bluetooth` (CoreBluetooth on macOS, BlueZ on Linux).
   - Inserts a **translation layer** that buffers pairs of 64-byte CHIRP writes into single 128-byte BLE writes, synthesising an `0x06` ACK for the first half so CHIRP stays in sync.
   - All other traffic (ident handshake, downloads, ACKs) passes through unchanged.

4. **Implementation** — Written in Go 1.22. Fully unit-tested with an in-memory fake radio that emulates the exact wire protocol, so no hardware is needed to run `go test`.

See [SPEC.md](SPEC.md) for the full protocol reference, package layout, and test plan.

---

## How it works

```
┌────────┐  PTY (/tmp/ttyBLE)  ┌──────────────────────────────┐  BLE GATT  ┌──────────┐
│ CHIRP  │ ──────────────────► │  uv5r-relay                  │ ─────────► │ UV-5R Mn │
│        │ ◄────────────────── │    [translator] [BLE client] │ ◄───────── │  (HM-10) │
└────────┘   transparent bytes └──────────────────────────────┘  notify    └──────────┘
```

### On startup
1. Opens a PTY master/slave pair; creates a symlink at `--link` (default `/tmp/ttyBLE`) pointing to the slave device path.
2. Scans for a BLE peripheral whose advertised name contains `--name` (default `5R`) or whose advertising data includes service `0xFFE0`.
3. Connects, discovers service `FFE0` and characteristic `FFE1`, enables notifications.
4. Waits for CHIRP to open the PTY.

### During a CHIRP operation
- **Identification handshake** (`PROGRAMCOLORPROU` → `F` → `M` → `SEND!`): forwarded byte-for-byte each time CHIRP initiates an operation.
- **Download (Radio → CHIRP)**: CHIRP sends `0x52 addr 0x40`; relay forwards to BLE; radio replies with a 4-byte header + 64 bytes; relay forwards back to CHIRP.
- **Upload (CHIRP → Radio)**:
  - Even-addressed 64-byte blocks: cached; a synthetic `0x06` ACK is returned to CHIRP immediately.
  - Odd-addressed (second-half) 64-byte blocks: combined with the cached first half → one 128-byte BLE write; radio's real `0x06` ACK forwarded to CHIRP.
  - Final/partial blocks at segment boundaries: padded to 128 bytes with `0xFF` before sending.
- **Chunking**: each BLE write is split into ≤20-byte chunks (or `--mtu - 3` if MTU negotiation succeeds). Notifications are reassembled transparently.

### On shutdown
Press **Ctrl+C** once — graceful shutdown: context cancelled, both sides closed, BLE disconnected, PTY symlink removed. Press **Ctrl+C** a second time to force-quit immediately.

---

## Requirements

|           |                                                                          |
|-----------|--------------------------------------------------------------------------|
| **Radio** | Baofeng UV-5R Mini (also branded `5R Mini`, `GM-5R Mini`)                |
| **CHIRP** | [kk7ds/chirp](https://github.com/kk7ds/chirp) mainline, any recent build |
| **Go**    | 1.22 or newer                                                            |
| **macOS** | 12+ (Monterey). Bluetooth permission granted to your terminal app.       |
| **Linux** | BlueZ ≥ 5.50, `bluetoothd` running. No root required.                    |

---

## Installation

```sh
git clone <this repo>
cd uv5r-ble-relay
go build -o uv5r-relay ./cmd/uv5r-relay
```

---

## Usage

### 1. Start the relay

```sh
./uv5r-relay --link /tmp/ttyBLE
```

Expected output:
```
[relay] PTY slave: /dev/ttys008
[relay] symlink:   /tmp/ttyBLE -> /dev/ttys008
[relay] scanning for "5R" with service 0xFFE0…
time=… level=INFO msg="ble: device seen" addr=… name="5R Mini" rssi=-54 has_svc=true
[relay] mainline-CHIRP translator enabled (use --passthrough to disable)
[relay] waiting for CHIRP to open the port…
```

### 2. Open CHIRP

- **Radio → Download From Radio** (or Upload To Radio)
- **Port**: `/tmp/ttyBLE`
- **Vendor**: `Baofeng`
- **Model**: `UV-5R Mini`
- Click OK / Clone

### 3. Stop the relay

Press `Ctrl+C`. Press it again to force-quit if it hangs.

---

## Flags

| Flag             | Default       | Purpose                                                                    |
|------------------|---------------|----------------------------------------------------------------------------|
| `--link`         | `/tmp/ttyBLE` | Symlink path for the PTY slave. Use this in CHIRP.                         |
| `--name`         | `5R`          | Substring matched against the radio's advertised BLE name                  |
| `--address`      | _(scan)_      | Skip scan; connect directly to this BLE address / CoreBluetooth UUID       |
| `--service`      | `FFE0`        | GATT service UUID                                                          |
| `--char`         | `FFE1`        | GATT characteristic UUID                                                   |
| `--mtu`          | `247`         | Requested ATT MTU; write chunk size = MTU − 3                              |
| `--pace`         | `0`           | Delay between BLE write chunks (e.g. `5ms` for slow radios)                |
| `--scan-timeout` | `15s`         | How long to scan before giving up                                          |
| `--scan-list`    | off           | List all nearby BLE devices and exit (useful for finding the radio's name) |
| `--passthrough`  | off           | Disable the 64→128 translation layer (raw mode)                            |
| `--verbose`      | off           | Enable debug logging                                                       |
| `--trace`        | off           | Hex-dump every byte in both directions                                     |
| `--fake`         | off           | Use the in-memory fake radio instead of BLE (no hardware needed)           |
| `--version`      | —             | Print version and exit                                                     |

---

## Troubleshooting

### Radio not found during scan

```sh
./uv5r-relay --scan-list --scan-timeout 20s
```

This lists every BLE device seen. Find your radio's name and use `--name` to match it:

```sh
./uv5r-relay --name "GM-5R" --link /tmp/ttyBLE
```

Or connect directly by address (useful on macOS where CoreBluetooth assigns a UUID):

```sh
./uv5r-relay --address "12345678-ABCD-…" --link /tmp/ttyBLE
```

### CHIRP reports "Radio did not respond"

Run with `--verbose` to see exactly where the protocol stalls:

```sh
./uv5r-relay --verbose --link /tmp/ttyBLE
```

Look for:
- `translate: waiting for more bytes` — partial command received; CHIRP may be sending slowly.
- `translate: unexpected ACK byte from radio` — radio replied with something other than `0x06`.
- `translate: inner read failed` — BLE read timed out or disconnected.

If the protocol seems fine but CHIRP still fails, try adding a small inter-chunk delay:

```sh
./uv5r-relay --pace 10ms --link /tmp/ttyBLE
```

### Smoke-test without a radio

```sh
./uv5r-relay --fake --link /tmp/ttyBLE
```

The in-memory fake radio responds to the full ident handshake and all read/write commands, letting you verify CHIRP connectivity without physical hardware.

---

## Development

```sh
go test ./...          # all unit tests (no BLE hardware needed)
go test -race ./...    # race detector
go vet ./...
```

The BLE hardware path (`internal/bleconn/ble.go`) is isolated behind the `Transport` interface and not touched by unit tests. Everything else — the PTY layer, translator state machine, relay pump, and protocol constants — is fully covered.

See [SPEC.md](SPEC.md) for the complete technical specification.
