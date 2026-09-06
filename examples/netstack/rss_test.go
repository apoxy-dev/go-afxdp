package main

import "testing"

// The Toeplitz test vectors from Microsoft's RSS specification (the
// "Verifying the RSS Hash Calculation" example), IPv4 without ports and
// with ports.
func TestToeplitzVectors(t *testing.T) {
	key := []byte{0x6d, 0x5a, 0x56, 0xda, 0x25, 0x5b, 0x0e, 0xc2, 0x41, 0x67, 0x25, 0x3d, 0x43, 0xa3, 0x8f, 0xb0,
		0xd0, 0xca, 0x2b, 0xcb, 0xae, 0x7b, 0x30, 0xb4, 0x77, 0xcb, 0x2d, 0xa3, 0x80, 0x30, 0xf2, 0x0c,
		0x6a, 0x42, 0xb7, 0x3b, 0xbe, 0xac, 0x01, 0xfa}
	r := &rss{key: key, indir: []int{0}}
	cases := []struct {
		data []byte
		want uint32
	}{
		// 66.9.149.187:2794 -> 161.142.100.80:1766
		{[]byte{66, 9, 149, 187, 161, 142, 100, 80, 2794 >> 8, 2794 & 0xff, 1766 >> 8, 1766 & 0xff}, 0x51ccc178},
		{[]byte{66, 9, 149, 187, 161, 142, 100, 80}, 0x323e8fc2},
		// 199.92.111.2:14230 -> 65.69.140.83:4739
		{[]byte{199, 92, 111, 2, 65, 69, 140, 83, 14230 >> 8, 14230 & 0xff, 4739 >> 8, 4739 & 0xff}, 0xc626b0ea},
		{[]byte{199, 92, 111, 2, 65, 69, 140, 83}, 0xd718262a},
	}
	for _, c := range cases {
		if got := r.toeplitz(c.data); got != c.want {
			t.Errorf("toeplitz(%v) = %#x, want %#x", c.data, got, c.want)
		}
	}
}
