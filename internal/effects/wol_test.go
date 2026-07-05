package effects

import (
	"bytes"
	"testing"
)

func TestMagicPacket(t *testing.T) {
	pkt, err := magicPacket("98:b7:85:20:77:6b")
	if err != nil {
		t.Fatalf("magicPacket: %v", err)
	}
	if len(pkt) != 102 {
		t.Fatalf("len = %d, want 102", len(pkt))
	}
	if !bytes.Equal(pkt[:6], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Errorf("header = %x, want six 0xFF", pkt[:6])
	}
	mac := []byte{0x98, 0xb7, 0x85, 0x20, 0x77, 0x6b}
	for i := range 16 {
		off := 6 + i*6
		if !bytes.Equal(pkt[off:off+6], mac) {
			t.Fatalf("repetition %d = %x, want %x", i, pkt[off:off+6], mac)
		}
	}
}

func TestMagicPacketBadMAC(t *testing.T) {
	if _, err := magicPacket("not-a-mac"); err == nil {
		t.Error("expected error for bad MAC")
	}
}
