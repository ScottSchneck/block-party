package serverlist

import (
	"net/netip"
	"testing"
)

// Consoles outside the house are told the home's internet address for
// NetherNet: PUBLIC_IP, or the one Friends outside the house found.
func TestAdvertisedIP(t *testing.T) {
	found := netip.MustParseAddr("198.51.100.7")
	for _, c := range []struct {
		public  string
		outside Outside
		want    string
	}{
		{"", Outside{}, ""},
		{"", Outside{Enabled: true, IP: found}, "198.51.100.7"},
		{"", Outside{Enabled: false, IP: found}, ""},
		{"203.0.113.9", Outside{Enabled: true, IP: found}, "203.0.113.9"},
		{"", Outside{Enabled: true, IP: netip.MustParseAddr("10.0.1.1")}, ""},
	} {
		cfg := &Config{}
		if c.public != "" {
			cfg.PublicIP = netip.MustParseAddr(c.public)
		}
		o := c.outside
		cfg.OutsideInfo = func() Outside { return o }
		got := advertisedIP(cfg)
		if (c.want == "" && got.IsValid()) || (c.want != "" && got.String() != c.want) {
			t.Errorf("public %q outside %+v: %v, want %q", c.public, c.outside, got, c.want)
		}
	}
}
