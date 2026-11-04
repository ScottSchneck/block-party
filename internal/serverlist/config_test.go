package serverlist

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAllowsNetherNet(t *testing.T) {
	all := &Config{NetherNetNames: map[string]bool{}}
	if !all.allowsNetherNet("geo.hivebedrock.network:19132") {
		t.Fatal("an empty NETHERNET_NAMES should allow every name")
	}
	one := &Config{NetherNetNames: map[string]bool{"play.galaxite.net": true}}
	cases := map[string]bool{
		"play.galaxite.net":             true,
		"PLAY.Galaxite.NET:19132":       true,
		"play.galaxite.net.":            true,
		"geo.hivebedrock.network:19132": false,
		"":                              false,
	}
	for host, want := range cases {
		if got := one.allowsNetherNet(host); got != want {
			t.Errorf("allowsNetherNet(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestFilterNamesTurnsAwayOtherNames(t *testing.T) {
	cfg := &Config{NetherNetNames: map[string]bool{"play.galaxite.net": true}}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := filterNames(cfg, inner)

	for host, want := range map[string]int{
		"play.galaxite.net:19132":       http.StatusOK,
		"geo.hivebedrock.network:19132": http.StatusNotFound,
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/v1/join", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %s: status %d, want %d", host, rec.Code, want)
		}
	}
}

func TestConnectionSettings(t *testing.T) {
	for _, tc := range []struct {
		env                map[string]string
		wantRak, wantNeth  bool
		wantConnection     string
		wantNetherNetNames int
	}{
		{map[string]string{}, true, true, "both", 0},
		{map[string]string{"CONNECTION": "nethernet"}, false, true, "nethernet", 0},
		{map[string]string{"CONNECTION": "both", "NETHERNET_NAMES": "play.galaxite.net, mco.lbsg.net"}, true, true, "both", 2},
		{map[string]string{"CONNECTION": "raknet", "NETHERNET_NAMES": "play.galaxite.net"}, true, false, "raknet", 0},
		// Settings from the first build keep working.
		{map[string]string{"SIGNALING": "off"}, true, false, "raknet", 0},
		{map[string]string{"SIGNALING": "auto"}, true, true, "both", 0},
		{map[string]string{"SIGNALING": "http"}, true, true, "both", 0},
		{map[string]string{"RAKNET": "false"}, false, true, "nethernet", 0},
	} {
		t.Setenv("LIST_IP", "10.0.1.117")
		for _, k := range []string{"CONNECTION", "NETHERNET_NAMES", "SIGNALING", "RAKNET"} {
			t.Setenv(k, "")
		}
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		c, err := LoadConfig()
		if err != nil {
			t.Fatalf("%v: %v", tc.env, err)
		}
		if c.RakNet != tc.wantRak || c.NetherNet != tc.wantNeth || c.Connection != tc.wantConnection ||
			len(c.NetherNetNames) != tc.wantNetherNetNames {
			t.Errorf("%v: got connection=%s raknet=%v nethernet=%v names=%d", tc.env, c.Connection, c.RakNet,
				c.NetherNet, len(c.NetherNetNames))
		}
		// Plain HTTP joins unless asked: consoles refuse the list's own
		// certificate (Terracotta) but fall back from a refused HTTPS join.
		if want := map[string]string{"auto": "auto"}[tc.env["SIGNALING"]]; want == "" && c.Signaling != "http" || want != "" && c.Signaling != want {
			t.Errorf("%v: signaling %q", tc.env, c.Signaling)
		}
	}
}

func TestExtraSignalingPorts(t *testing.T) {
	for value, want := range map[string]int{"": 2, "none": 0, "off": 0, "8080": 1} {
		t.Setenv("LIST_IP", "10.0.1.117")
		t.Setenv("EXTRA_SIGNALING_PORTS", value)
		c, err := LoadConfig()
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if got := len(c.ExtraSignalingPorts); got != want {
			t.Errorf("EXTRA_SIGNALING_PORTS=%q gave %d ports, want %d", value, got, want)
		}
	}
}

func TestListIPFromInterface(t *testing.T) {
	old := interfaceAddrs
	defer func() { interfaceAddrs = old }()
	addrs := func(cidrs ...string) func() ([]net.Addr, error) {
		return func() ([]net.Addr, error) {
			var out []net.Addr
			for _, c := range cidrs {
				ip, n, _ := net.ParseCIDR(c)
				n.IP = ip
				out = append(out, n)
			}
			return out, nil
		}
	}
	t.Setenv("LIST_IP", "")
	interfaceAddrs = addrs("127.0.0.1/8", "::1/128", "10.0.1.117/24", "fe80::1/64", "169.254.3.4/16")
	c, err := LoadConfig()
	if err != nil || c.ListIP.String() != "10.0.1.117" {
		t.Fatalf("one address: %v %v", c, err)
	}
	interfaceAddrs = addrs("127.0.0.1/8", "10.0.1.117/24", "192.168.9.2/24")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "several") {
		t.Errorf("two addresses: %v", err)
	}
	interfaceAddrs = addrs("127.0.0.1/8")
	if _, err := LoadConfig(); err == nil {
		t.Error("no address")
	}
	// LIST_IP still wins.
	t.Setenv("LIST_IP", "10.0.1.50")
	if c, err := LoadConfig(); err != nil || c.ListIP.String() != "10.0.1.50" {
		t.Errorf("LIST_IP: %v %v", c, err)
	}
}
