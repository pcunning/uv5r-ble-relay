//go:build !nobl

package bleconn

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"

	"github.com/pcunning/uv5r-ble-relay/internal/protocol"
)

// BLEConfig configures a BLE connection attempt.
type BLEConfig struct {
	Address     string        // optional explicit BLE address / CoreBluetooth UUID
	NameFilter  string        // substring matched against advertised LocalName
	ServiceUUID string        // hex (e.g. "FFE0")
	CharUUID    string        // hex (e.g. "FFE1")
	MTU         int           // requested MTU; chunkSize = MTU-3
	Pace        time.Duration // optional inter-chunk delay
	ScanTimeout time.Duration // scan window
	Logger      *slog.Logger
}

// ScannedDevice is a single BLE advertisement result.
type ScannedDevice struct {
	Address string
	Name    string
	RSSI    int16
	HasFfE0 bool
}

// BLETransport is a Transport backed by tinygo.org/x/bluetooth.
type BLETransport struct {
	cfg       BLEConfig
	adapter   *bluetooth.Adapter
	device    bluetooth.Device
	char      bluetooth.DeviceCharacteristic
	notify    *NotifyQueue
	chunkSize int
	closed    bool
	mu        sync.Mutex
}

// Connect performs the full enable/scan/connect/discover/notify dance.
func Connect(cfg BLEConfig) (*BLETransport, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MTU <= 0 {
		cfg.MTU = 247
	}
	if cfg.ScanTimeout <= 0 {
		cfg.ScanTimeout = 15 * time.Second
	}
	if cfg.ServiceUUID == "" {
		cfg.ServiceUUID = protocol.ServiceShort
	}
	if cfg.CharUUID == "" {
		cfg.CharUUID = protocol.CharShort
	}

	svcUUID, err := bluetooth.ParseUUID(normaliseUUID(cfg.ServiceUUID))
	if err != nil {
		return nil, fmt.Errorf("parse service UUID: %w", err)
	}
	chrUUID, err := bluetooth.ParseUUID(normaliseUUID(cfg.CharUUID))
	if err != nil {
		return nil, fmt.Errorf("parse char UUID: %w", err)
	}

	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return nil, fmt.Errorf("enable adapter: %w", err)
	}

	var addr bluetooth.Address
	if cfg.Address != "" {
		auid, err := bluetooth.ParseUUID(cfg.Address)
		if err != nil {
			addr.Set(cfg.Address)
		} else {
			addr.UUID = auid
		}
		cfg.Logger.Info("ble: skipping scan, using configured address", "address", cfg.Address)
	} else {
		found, err := scan(adapter, cfg.NameFilter, svcUUID, cfg.ScanTimeout, cfg.Logger)
		if err != nil {
			return nil, err
		}
		addr = found.Address
		cfg.Logger.Info("ble: found device",
			"name", found.LocalName(), "address", addr.String(), "rssi", found.RSSI)
	}

	dev, err := adapter.Connect(addr, bluetooth.ConnectionParams{})
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	services, err := dev.DiscoverServices([]bluetooth.UUID{svcUUID})
	if err != nil {
		_ = dev.Disconnect()
		return nil, fmt.Errorf("discover service: %w", err)
	}
	if len(services) == 0 {
		_ = dev.Disconnect()
		return nil, fmt.Errorf("service %s not found", cfg.ServiceUUID)
	}

	chars, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{chrUUID})
	if err != nil {
		_ = dev.Disconnect()
		return nil, fmt.Errorf("discover characteristic: %w", err)
	}
	if len(chars) == 0 {
		_ = dev.Disconnect()
		return nil, fmt.Errorf("characteristic %s not found", cfg.CharUUID)
	}
	char := chars[0]

	mtu, err := char.GetMTU()
	if err != nil || mtu == 0 {
		mtu = 23
	}
	chunkSize := int(mtu) - 3
	if chunkSize <= 0 || chunkSize > 244 {
		chunkSize = protocol.DefaultChunkSize
	}
	cfg.Logger.Info("ble: connected", "mtu", mtu, "chunk", chunkSize)

	t := &BLETransport{
		cfg:       cfg,
		adapter:   adapter,
		device:    dev,
		char:      char,
		notify:    NewNotifyQueue(),
		chunkSize: chunkSize,
	}

	if err := char.EnableNotifications(func(buf []byte) {
		t.notify.Push(buf)
	}); err != nil {
		_ = dev.Disconnect()
		return nil, fmt.Errorf("enable notifications: %w", err)
	}

	return t, nil
}

// Read drains buffered notifications.
func (t *BLETransport) Read(p []byte) (int, error) { return t.notify.Read(p) }

// Write chunks data and sends each segment via WriteWithoutResponse.
func (t *BLETransport) Write(p []byte) (int, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return 0, errors.New("write on closed BLETransport")
	}
	t.mu.Unlock()
	if err := WriteChunked(t.char.WriteWithoutResponse, p, t.chunkSize, t.cfg.Pace); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close disconnects the device and releases the notification queue.
func (t *BLETransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	_ = t.notify.Close()
	if err := t.device.Disconnect(); err != nil {
		return fmt.Errorf("disconnect: %w", err)
	}
	return nil
}

// ScanAll scans for all BLE devices for the given duration and returns every
// unique device seen. Useful for diagnosing which name/address the radio
// is advertising under (run with --scan-list).
func ScanAll(timeout time.Duration, logger *slog.Logger) ([]ScannedDevice, error) {
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return nil, fmt.Errorf("enable adapter: %w", err)
	}
	svcUUID, _ := bluetooth.ParseUUID(normaliseUUID("FFE0"))

	var mu sync.Mutex
	seen := map[string]ScannedDevice{}
	done := make(chan struct{})

	timer := time.AfterFunc(timeout, func() { _ = adapter.StopScan() })
	defer timer.Stop()

	go func() {
		_ = adapter.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
			addr := r.Address.String()
			name := r.LocalName()
			mu.Lock()
			prev, exists := seen[addr]
			// update if name just arrived or RSSI improved
			if !exists || (prev.Name == "" && name != "") {
				d := ScannedDevice{
					Address: addr,
					Name:    name,
					RSSI:    r.RSSI,
					HasFfE0: r.HasServiceUUID(svcUUID),
				}
				seen[addr] = d
				if logger != nil {
					logger.Info("ble: seen", "addr", addr, "name", name, "rssi", r.RSSI, "has_ffe0", d.HasFfE0)
				}
			}
			mu.Unlock()
		})
		close(done)
	}()
	<-done

	mu.Lock()
	defer mu.Unlock()
	out := make([]ScannedDevice, 0, len(seen))
	for _, d := range seen {
		out = append(out, d)
	}
	return out, nil
}

// scan watches for advertisements until the first match passes name and
// service filters, then stops scanning and returns it.
//
// Matching rules (HM-10/CC2541 modules often do NOT include service UUIDs
// in their advertising packets — name matching alone is the reliable path):
//
//   - If nameFilter is set:  accept if name contains nameFilter (case-insensitive)
//     OR if the service UUID appears in the advertising data.
//   - If nameFilter is empty: accept any device that advertises the service UUID.
//
// Every unique device seen during the window is logged at INFO level.
func scan(adapter *bluetooth.Adapter, nameFilter string, svc bluetooth.UUID,
	timeout time.Duration, logger *slog.Logger) (*bluetooth.ScanResult, error) {

	logger.Info("ble: scanning", "name_filter", nameFilter, "service", svc.String(), "timeout", timeout)

	var (
		result bluetooth.ScanResult
		gotMu  sync.Mutex
		got    bool
	)
	seen := map[string]string{} // addr → last logged name
	done := make(chan struct{})

	timer := time.AfterFunc(timeout, func() {
		_ = adapter.StopScan()
	})
	defer timer.Stop()

	go func() {
		_ = adapter.Scan(func(a *bluetooth.Adapter, r bluetooth.ScanResult) {
			addr := r.Address.String()
			name := r.LocalName()
			matchesService := r.HasServiceUUID(svc)
			matchesName := nameFilter == "" || strings.Contains(strings.ToUpper(name), strings.ToUpper(nameFilter))

			gotMu.Lock()
			defer gotMu.Unlock()

			// Log each unique device (or when name arrives for the first time).
			if prev, ok := seen[addr]; !ok || (prev == "" && name != "") {
				seen[addr] = name
				logger.Info("ble: device seen",
					"addr", addr, "name", name, "rssi", r.RSSI, "has_svc", matchesService)
			}

			if got {
				return
			}

			// Accept: name matches filter, OR service UUID in advertising data.
			// When nameFilter is empty, require service UUID.
			var accept bool
			if nameFilter != "" {
				accept = matchesName || matchesService
			} else {
				accept = matchesService
			}
			if !accept {
				return
			}

			result = r
			got = true
			_ = a.StopScan()
		})
		close(done)
	}()

	<-done
	gotMu.Lock()
	defer gotMu.Unlock()
	if !got {
		// Summarise what WAS seen to help the user pick the right --name.
		var hint string
		if len(seen) > 0 {
			hint = fmt.Sprintf(" (%d device(s) seen during scan — rerun with --scan-list to inspect)", len(seen))
		}
		return nil, fmt.Errorf("no matching BLE device found within scan window%s", hint)
	}
	return &result, nil
}

// normaliseUUID expands short forms ("FFE0") to their 128-bit canonical form
// since older bluetooth.ParseUUID implementations only accepted full strings.
func normaliseUUID(u string) string {
	s := strings.TrimSpace(u)
	if len(s) == 4 {
		return "0000" + strings.ToLower(s) + "-0000-1000-8000-00805f9b34fb"
	}
	if len(s) == 8 {
		return strings.ToLower(s) + "-0000-1000-8000-00805f9b34fb"
	}
	return s
}
