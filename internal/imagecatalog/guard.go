package imagecatalog

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// This file is the v0 answer to the SSRF exposure called out in
// docs/design/ignition-image-delivery.md "Security status": the resolver used
// to `remote.Get` against whatever host a client-supplied reference parsed to,
// with no allowlist, no block on link-local / private / metadata destinations,
// and no timeout independent of the caller. Registry identity / signature /
// provenance verification (admission step 3) is still not implemented — this
// only closes the network-reachability hole.

// Markers used to recognise a guard rejection after it has been wrapped by
// go-containerregistry's transport (which flattens dial errors to strings).
const (
	dialRefusedMarker = "refusing to dial non-public address"
	dialNoIPMarker    = "did not resolve to an IP"
)

// checkRegistryHost rejects a registry host that is not on the allowlist. An
// empty allowlist imposes no host restriction (the address guard below still
// applies); a non-empty allowlist is an exact, case-insensitive host match
// (any ":port" is ignored). Allowlist entries are registry hosts as
// go-containerregistry reports them — e.g. "us-central1-docker.pkg.dev", and
// "index.docker.io" for Docker Hub.
func checkRegistryHost(host string, allowlist []string) error {
	if len(allowlist) == 0 {
		return nil
	}
	h := hostOnly(host)
	for _, a := range allowlist {
		if h == hostOnly(a) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrSourceNotAllowed, host)
}

func hostOnly(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

// blockedIP reports whether ip is one an image resolver must never dial:
// loopback, unspecified, multicast, link-local (which includes the
// 169.254.169.254 GCE metadata address), RFC1918 / RFC4193 private space, or
// the 100.64.0.0/10 shared CGNAT range GKE uses.
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if ip.IsPrivate() { // 10/8, 172.16/12, 192.168/16, fc00::/7
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true // 100.64.0.0/10
	}
	return false
}

// guardedControl is a net.Dialer Control hook. It runs after name resolution
// on the concrete address about to be dialed, so a hostname that resolves — or
// rebinds — to a blocked range is refused here, not just at parse time.
func guardedControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("cannot parse dial address %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("dial address %q %s", host, dialNoIPMarker)
	}
	if blockedIP(ip) {
		return fmt.Errorf("%s %s", dialRefusedMarker, ip)
	}
	return nil
}

// guardedTransport clones the default transport and installs guardedControl on
// its dialer. Every connection remote.Get makes — the manifest request and any
// redirect to blob storage — goes through it.
func guardedTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: guardedControl}
	t.DialContext = d.DialContext
	return t
}
