// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package agent

import (
	"encoding/binary"
	"testing"
)

// /proc/net/tcp prints each 32-bit address word in host byte order; the
// vectors below are the little-endian form every supported host uses.
func TestListenerLoopbackCoversWholeLoopbackRange(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("vectors are little-endian /proc encodings")
	}
	cases := []struct {
		name, raw, family string
		want              bool
	}{
		{"127.0.0.1", "0100007F", "ipv4", true},
		{"127.0.0.53 systemd-resolved", "3500007F", "ipv4", true},
		{"127.0.1.1", "0101007F", "ipv4", true},
		{"10.0.0.1", "0100000A", "ipv4", false},
		{"0.0.0.0", "00000000", "ipv4", false},
		{"::1", "00000000000000000000000001000000", "ipv6", true},
		{"::ffff:127.0.0.1", "0000000000000000FFFF00000100007F", "ipv6", true},
		{"::ffff:127.0.0.53", "0000000000000000FFFF00003500007F", "ipv6", true},
		{"::", "00000000000000000000000000000000", "ipv6", false},
		{"::ffff:10.0.0.1", "0000000000000000FFFF00000100000A", "ipv6", false},
		{"not hex", "zz", "ipv4", false},
		{"ipv4 word in ipv6 table", "0100007F", "ipv6", false},
	}
	for _, tc := range cases {
		if got := listenerLoopback(tc.raw, tc.family); got != tc.want {
			t.Errorf("%s: listenerLoopback(%q, %q) = %v, want %v", tc.name, tc.raw, tc.family, got, tc.want)
		}
	}
}
