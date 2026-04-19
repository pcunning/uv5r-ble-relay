//go:build !nobl && !darwin

package bleconn

import "tinygo.org/x/bluetooth"

// setAddressUUID is a no-op on non-darwin platforms.
// BlueZ identifies peripherals by MAC address, not CoreBluetooth UUID.
func setAddressUUID(_ *bluetooth.Address, _ bluetooth.UUID) {}
