// Package protocol contains the constants describing the Baofeng UV-5R Mini
// BLE wire protocol. The relay does not interpret these values — they are
// shared with tests and any future tooling.
package protocol

// BLE GATT identifiers (HM-10 transparent UART profile).
const (
	ServiceUUID        = "0000FFE0-0000-1000-8000-00805F9B34FB"
	CharacteristicUUID = "0000FFE1-0000-1000-8000-00805F9B34FB"
	ServiceShort       = "FFE0"
	CharShort          = "FFE1"
)

// Identification handshake magic strings/bytes.
const (
	IdentRequest = "PROGRAMCOLORPROU"
	IdentAck     = byte(0x06)
	CmdF         = byte('F')
	CmdM         = byte('M')
	SendPrefix   = "SEND!"
)

// Read/write opcodes.
const (
	OpRead  = byte(0x52) // 'R'
	OpWrite = byte(0x57) // 'W'

	ReadBlockSize  = 0x40 // 64 bytes per BLE read
	WriteBlockSize = 0x80 // 128 bytes per BLE write
)

// FDescriptorLen is the fixed length of the reply to "F".
const FDescriptorLen = 16

// MModelLen is the fixed length of the reply to "M".
const MModelLen = 15

// MModel is the model identifier returned by the radio.
const MModel = "5RMINI  +L00000"

// FDescriptor is a fixed sample firmware descriptor used by the fake radio.
var FDescriptor = []byte{
	0x01, 0x36, 0x01, 0x74, 0x04, 0x00, 0x05, 0x20,
	0x02, 0x20, 0x02, 0x60, 0x01, 0x03, 0x50, 0x03,
}

// Memory map (per SPEC §3.4).
var (
	MemStarts = []uint16{0x0000, 0x9000, 0xA000}
	MemSizes  = []uint16{0x8040, 0x0040, 0x01C0}
)

// Segment describes one contiguous memory region: [Start, Start+Size).
type Segment struct {
	Start uint16
	Size  uint16
}

// MemSegments is the structured equivalent of MemStarts/MemSizes.
var MemSegments = []Segment{
	{Start: 0x0000, Size: 0x8040},
	{Start: 0x9000, Size: 0x0040},
	{Start: 0xA000, Size: 0x01C0},
}

// SegmentOf returns the segment containing addr (and true) or zero/false if
// addr is not within any defined segment.
func SegmentOf(addr uint16) (Segment, bool) {
	for _, s := range MemSegments {
		// uint32 widening avoids overflow for the very last segment.
		end := uint32(s.Start) + uint32(s.Size)
		if uint32(addr) >= uint32(s.Start) && uint32(addr) < end {
			return s, true
		}
	}
	return Segment{}, false
}

// IsLastBlockInSegment reports whether the block [addr, addr+blockLen) is the
// final block in its segment (i.e. addr+blockLen == segment end).
func IsLastBlockInSegment(addr uint16, blockLen uint16) bool {
	s, ok := SegmentOf(addr)
	if !ok {
		return false
	}
	return uint32(addr)+uint32(blockLen) == uint32(s.Start)+uint32(s.Size)
}

// TotalMemBytes is the sum of MemSizes — full image size.
const TotalMemBytes = 0x8040 + 0x0040 + 0x01C0 // 0x8240 = 33344

// DefaultChunkSize is the conservative BLE write chunk default
// (negotiated MTU 23 → 20 byte ATT payload).
const DefaultChunkSize = 20
