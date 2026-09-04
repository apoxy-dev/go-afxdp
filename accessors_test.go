package afxdp

import "testing"

// The capability accessors answer from what a socket is, not from what the
// caller asked for; a program that wraps sockets reports them upward.

func TestMultiBufferAccessor(t *testing.T) {
	plain := &Socket{options: newConfig().opts}
	if plain.MultiBuffer() {
		t.Fatal("MultiBuffer is true on a socket bound without XDP_USE_SG")
	}
	sg := &Socket{options: newConfig(WithMultiBuffer()).opts}
	if !sg.MultiBuffer() {
		t.Fatal("MultiBuffer is false after WithMultiBuffer")
	}
}

func TestMaxPacket(t *testing.T) {
	for _, fs := range []int{2048, 4096} {
		xsk := &Socket{options: newConfig(WithFrameSize(fs)).opts}
		if got, want := xsk.MaxPacket(), xsk.FrameSize()-256; got != want {
			t.Fatalf("frame %d: MaxPacket %d, want FrameSize-XDP_PACKET_HEADROOM = %d", fs, got, want)
		}
	}
}
