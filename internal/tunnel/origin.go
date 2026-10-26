package tunnel

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// defaultPorts are elided from an origin.
var defaultPorts = map[string]int{"http": 80, "https": 443}

// plainHosts are the only hosts --gate may name over http:// (§3: contributors' gates).
var plainHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// CanonicalOrigin is scheme://host[:port] of u, lowercase, the scheme's default port elided (§4 hello
// "origin"): what the node dialed, and what the gate compares with the origin of its public_url. The
// scheme is http or https; userinfo, path, query and fragment are not part of an origin. An IPv6 host
// keeps its brackets; a port is written in decimal without leading zeros.
func CanonicalOrigin(u *url.URL) (string, error) {
	if u == nil {
		return "", errors.New("origin: no URL")
	}
	scheme := strings.ToLower(u.Scheme)
	def, ok := defaultPorts[scheme]
	if !ok {
		return "", fmt.Errorf("origin: scheme %q: want http or https", u.Scheme)
	}
	if u.Opaque != "" {
		return "", errors.New("origin: not a hierarchical URL")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("origin: no host")
	}
	if strings.ContainsAny(host, "/?#@% \t\r\n") { // '%': no IPv6 zone, no escapes
		return "", fmt.Errorf("origin: host %q", host)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	origin := scheme + "://" + host
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("origin: port %q", p)
		}
		if n != def {
			origin += ":" + strconv.Itoa(n)
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("origin: empty port")
	}
	return origin, nil
}

// CheckGateURL parses --gate (§3): https://, or http:// only on localhost, 127.0.0.1 and [::1]
// (contributors' gates); no userinfo, query or fragment.
func CheckGateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("--gate: %v", err)
	}
	switch {
	case u.User != nil || strings.Contains(u.Opaque, "@"): // first, so that no message below quotes them
		return nil, errors.New("--gate: no credentials in the URL (they go in the handshake, §3)")
	case u.Scheme != "https" && u.Scheme != "http":
		return nil, fmt.Errorf("--gate %q: want an https:// URL", raw)
	case u.Opaque != "" || u.Host == "":
		return nil, fmt.Errorf("--gate %q: no host", raw)
	case u.RawQuery != "" || u.ForceQuery:
		return nil, fmt.Errorf("--gate %q: no query", raw)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, fmt.Errorf("--gate %q: no fragment", raw)
	}
	if _, err := CanonicalOrigin(u); err != nil {
		return nil, fmt.Errorf("--gate %q: %v", raw, err)
	}
	if u.Scheme == "http" && !plainHosts[strings.ToLower(u.Hostname())] {
		return nil, fmt.Errorf("--gate %q: http:// only for localhost, 127.0.0.1 and [::1]; a gate elsewhere is https://", raw)
	}
	return u, nil
}
