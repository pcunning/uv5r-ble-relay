//go:build nobl

package bleconn

import (
	"errors"
	"log/slog"
	"time"
)

// BLEConfig is a placeholder when the package is built without BLE support.
type BLEConfig struct {
	Address     string
	NameFilter  string
	ServiceUUID string
	CharUUID    string
	MTU         int
	Pace        time.Duration
	ScanTimeout time.Duration
	Logger      *slog.Logger
}

// BLETransport is a no-op stub when built with `-tags nobl`.
type BLETransport struct{}

// Connect always returns an error in nobl builds.
func Connect(BLEConfig) (*BLETransport, error) {
	return nil, errors.New("BLE support not compiled in (built with -tags nobl)")
}

// Read is a no-op stub.
func (*BLETransport) Read([]byte) (int, error) { return 0, errors.New("nobl") }

// Write is a no-op stub.
func (*BLETransport) Write([]byte) (int, error) { return 0, errors.New("nobl") }

// Close is a no-op stub.
func (*BLETransport) Close() error { return nil }
