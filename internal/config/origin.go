package config

import (
	"crypto/subtle"
	"net/url"
	"strings"
)

// IsLoopbackHost reports whether host is a loopback bind address or name.
func IsLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	switch h {
	case "127.0.0.1", "::1", "::ffff:127.0.0.1", "localhost":
		return true
	}
	return false
}

var defaultPorts = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443", "ftp": "21"}

// NormalizeOrigin reduces a URL to its scheme://host[:port] origin, the way
// browsers send the Origin header. Non-web schemes (chrome-extension://...)
// are kept verbatim minus trailing slashes.
func NormalizeOrigin(origin string) string {
	value := strings.TrimRight(strings.TrimSpace(origin), "/")
	if value == "" {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return value
	}
	scheme := strings.ToLower(u.Scheme)
	def, special := defaultPorts[scheme]
	if !special {
		return value
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" && p != def {
		host += ":" + p
	}
	return scheme + "://" + host
}

// IsBrowserOriginAllowed allows non-browser clients (no Origin), loopback
// web origins, and exact entries from the configured allowlist.
func IsBrowserOriginAllowed(origin string, allowed []string) bool {
	if origin == "" {
		return true
	}
	n := NormalizeOrigin(origin)
	for _, a := range allowed {
		if a == n {
			return true
		}
	}
	u, err := url.Parse(n)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && IsLoopbackHost(u.Hostname())
}

// IsProxyAuthorized checks "Bearer <key>" in constant time. An empty
// expected key disables authentication.
func IsProxyAuthorized(authorization, expected string) bool {
	if expected == "" {
		return true
	}
	supplied, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) == 1
}
