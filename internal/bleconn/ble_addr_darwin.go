//go:build !nobl && darwin

package bleconn

import "tinygo.org/x/bluetooth"

// setAddressUUID assigns a CoreBluetooth peripheral UUID to an Address.
// On macOS, CoreBluetooth identifies peripherals by UUID rather than MAC address.
func setAddressUUID(addr *bluetooth.Address, uuid bluetooth.UUID) {
	addr.UUID = uuid
}
