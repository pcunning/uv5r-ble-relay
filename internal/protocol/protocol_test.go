package protocol

import "testing"

func TestTotalMemBytes(t *testing.T) {
	if TotalMemBytes != 0x8240 {
		t.Fatalf("TotalMemBytes = 0x%X want 0x8240", TotalMemBytes)
	}
	var sum uint32
	for _, s := range MemSizes {
		sum += uint32(s)
	}
	if sum != TotalMemBytes {
		t.Fatalf("sum(MemSizes)=0x%X != TotalMemBytes=0x%X", sum, TotalMemBytes)
	}
}

func TestMemMapShape(t *testing.T) {
	if len(MemStarts) != len(MemSizes) {
		t.Fatalf("MemStarts/MemSizes length mismatch")
	}
	if len(MemStarts) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(MemStarts))
	}
}

func TestFDescriptorLen(t *testing.T) {
	if len(FDescriptor) != FDescriptorLen {
		t.Fatalf("FDescriptor len=%d want %d", len(FDescriptor), FDescriptorLen)
	}
}

func TestMModelLen(t *testing.T) {
	if len(MModel) != MModelLen {
		t.Fatalf("MModel len=%d want %d", len(MModel), MModelLen)
	}
}
