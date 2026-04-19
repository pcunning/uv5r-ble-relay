package bleconn

import "io"

// Transport is the byte-level link to a UV-5R radio. The relay is written
// against this interface so it can be exercised with an in-memory fake or
// with a real BLE device.
type Transport interface {
	io.ReadWriteCloser
}
