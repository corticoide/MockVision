package netctl

import (
	"bytes"
	"testing"
)

// The set's keys follow the registers the rule loads: the address, the
// protocol and the port, each in a zero-padded 32-bit register.
func TestAllowedKeys(t *testing.T) {
	got := allowedKey("10.96.0.20", "tcp", 9000)
	want := []byte{10, 96, 0, 20, 6, 0, 0, 0, 0x23, 0x28, 0, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("key % x, want % x", got, want)
	}
	els := allowedElements(&Firewall{DNS: []string{"10.0.0.53"}, Allow: []Destination{{IP: "10.0.0.20", Port: 5000, Proto: "udp"}}})
	if len(els) != 3 {
		t.Fatalf("%d elements, want DNS over UDP and TCP and one target", len(els))
	}
	if els[0].Key[4] != 17 || els[1].Key[4] != 6 || els[2].Key[4] != 17 {
		t.Fatalf("protocols %d %d %d", els[0].Key[4], els[1].Key[4], els[2].Key[4])
	}
	if allowedElements(nil) != nil {
		t.Fatal("no firewall has no elements")
	}
}
