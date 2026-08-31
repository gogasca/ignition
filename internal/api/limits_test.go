package api

import (
	"strings"
	"testing"

	"ignition.dev/ignition/internal/store"
)

func allowList(cidrs ...string) store.EgressSpec {
	return store.EgressSpec{Mode: "ALLOW_LIST", AllowedCIDRs: cidrs}
}

func TestCheckAllowListAcceptsPublicCIDRs(t *testing.T) {
	for _, cidr := range []string{"8.8.8.8/32", "1.1.1.0/24", "203.0.114.0/24", "2606:4700::/32"} {
		if err := checkAllowList(allowList(cidr)); err != nil {
			t.Errorf("checkAllowList(%q) = %v, want nil", cidr, err)
		}
	}
}

func TestCheckAllowListRejectsPrivateAndSpecialCIDRs(t *testing.T) {
	cases := []string{
		"10.0.0.0/8",                 // RFC 1918
		"172.16.5.0/24",              // RFC 1918
		"192.168.1.0/24",             // RFC 1918
		"127.0.0.0/8",                // loopback
		"169.254.169.254/32",         // link-local / cloud metadata
		"100.64.0.0/10",              // carrier-grade NAT
		"198.18.0.0/15",              // benchmarking
		"192.0.2.0/24",               // TEST-NET-1
		"240.0.0.0/4",                // reserved
		"224.0.0.1/32",               // multicast
		"0.0.0.0/32",                 // "this host"
		"fc00::/7",                   // IPv6 ULA
		"fe80::/10",                  // IPv6 link-local
		"::1/128",                    // IPv6 loopback
		"::ffff:169.254.169.254/128", // IPv4-mapped metadata endpoint
		"64:ff9b::a00:1/128",         // NAT64-embedded 10.0.0.1
	}
	for _, cidr := range cases {
		if err := checkAllowList(allowList(cidr)); err == nil {
			t.Errorf("checkAllowList(%q) = nil, want rejection", cidr)
		}
	}
}

// A short prefix must not slip private space past the guard by parsing to a
// public-looking host address.
func TestCheckAllowListRejectsShortPrefixCoveringPrivate(t *testing.T) {
	for _, cidr := range []string{"8.8.8.8/1", "128.0.0.0/1", "0.0.0.0/0", "192.0.0.0/2"} {
		err := checkAllowList(allowList(cidr))
		if err == nil || !strings.Contains(err.Error(), "overlaps blocked network") {
			t.Errorf("checkAllowList(%q) = %v, want overlap rejection", cidr, err)
		}
	}
}

func TestCheckAllowListRejectsMalformedCIDR(t *testing.T) {
	for _, cidr := range []string{"8.8.8.8", "not-a-cidr", "10.0.0.0/33"} {
		if err := checkAllowList(allowList(cidr)); err == nil {
			t.Errorf("checkAllowList(%q) = nil, want invalid-CIDR error", cidr)
		}
	}
}

func TestCheckAllowListRejectsTLSDomains(t *testing.T) {
	e := store.EgressSpec{Mode: "ALLOW_LIST", AllowedTLSDomains: []string{"example.com"}}
	if err := checkAllowList(e); err == nil {
		t.Fatal("checkAllowList with allowedTlsDomains = nil, want rejection")
	}
}

func TestCheckAllowListRequiresAtLeastOneRule(t *testing.T) {
	if err := checkAllowList(store.EgressSpec{Mode: "ALLOW_LIST"}); err == nil {
		t.Fatal("checkAllowList with no rules = nil, want rejection")
	}
}
