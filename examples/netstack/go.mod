// The netstack example has its own module so the core go-afxdp library stays
// dependency-minimal (ebpf, netlink, sys) and only this example pulls in
// gVisor's TCP/IP stack, gvisor.dev/gvisor/pkg/tcpip.
//
// gVisor publishes no semver tags; the importable tree is its "go" branch
// (go get gvisor.dev/gvisor@go). Its go directive is ahead of the root
// module's, so building this example needs a newer toolchain, which the go
// command fetches on its own when GOTOOLCHAIN is auto (the default).
//
// Build it from this directory:
//
//	cd examples/netstack && go mod tidy && go build .
module github.com/atoonk/go-afxdp/examples/netstack

go 1.26.3

require (
	github.com/atoonk/go-afxdp v0.0.0-00010101000000-000000000000
	github.com/cilium/ebpf v0.16.0
	github.com/vishvananda/netlink v1.3.1
	github.com/vishvananda/netns v0.0.5
	golang.org/x/sys v0.43.0
	gvisor.dev/gvisor v0.0.0-20260901214033-af1f3d3eb0f6
)

require (
	github.com/google/btree v1.1.2 // indirect
	golang.org/x/exp v0.0.0-20250711185948-6ae5c78190dc // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/atoonk/go-afxdp => ../..
