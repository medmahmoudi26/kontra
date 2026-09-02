// sigv4.go — AWS Signature Version 4, the half of it the appliance's object store needs.
//
// WHY THIS EXISTS AT ALL, given that the store does not enforce signatures (see s3.go's header
// and ADR 0031 §1a): because `kontra explore` hands an operator a URL and calls it the
// credential. That surface is not "an S3 client with credentials"; it is curl, a browser, or
// DuckDB's httpfs opening a URL that already carries its own authorisation in the query string.
// Something has to MINT those URLs, and minting one means computing a real signature — the
// signing half of SigV4 is load-bearing even where the checking half is deliberately absent.
//
// The two halves are written against each other on purpose. Presign() builds the canonical
// request; parseSigV4() takes one apart. The day signature enforcement stops being an open
// question (ADR 0031 lists it as Open), the verifier is Presign's canonical request recomputed
// from the incoming one and compared — not a second implementation of the algorithm.
//
// Correctness here is not self-evident, so it is pinned to AWS's own published example rather
// than to our own output: see TestPresignMatchesAWSDocumentedVector.
package objstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	sigV4Service   = "s3"
	// unsignedPayload is what a presigned URL declares instead of a body hash: the signature is
	// computed before the body exists, and a GET has none anyway.
	unsignedPayload = "UNSIGNED-PAYLOAD"

	iso8601     = "20060102T150405Z"
	iso8601Date = "20060102"
)

// signedRequest is what an inbound request's SigV4 material parses into. It is DATA, not a
// verdict: the store records who a caller claims to be and serves the request either way, which
// is exactly what SeaweedFS-with-no-identities does today. Keeping the parse means the claim is
// available to log, and means enforcement is a policy decision rather than a rewrite.
type signedRequest struct {
	AccessKey string    // the claimed identity
	Region    string    // from the credential scope
	Signature string    // hex, as presented
	Presigned bool      // true when the material rode in the query string, not a header
	Expires   time.Time // zero unless Presigned; the moment the URL stops claiming validity
}

// parseSigV4 pulls the signature material out of a request, from either place it can ride: the
// `Authorization` header (a signed API call) or the `X-Amz-*` query parameters (a presigned URL).
//
// A request with NEITHER is not an error. Signatures are parsed and not enforced here (see s3.go),
// so "unsigned" is a supported case rather than a rejected one and callers get (nil, nil) for it.
//
// The example that used to be given here was a fleet Machine's cloud-init fetching its Bundle with
// a bare `curl`; that reader is gone (ADR 0036 makes a Bundle an OCI artifact) and the property is
// not, because turning enforcement on is its own decision with its own surfaces to check.
func parseSigV4(r *http.Request) (*signedRequest, error) {
	q := r.URL.Query()
	if cred := q.Get("X-Amz-Credential"); cred != "" {
		key, region, err := splitCredential(cred)
		if err != nil {
			return nil, err
		}
		out := &signedRequest{AccessKey: key, Region: region, Signature: q.Get("X-Amz-Signature"), Presigned: true}
		if d := q.Get("X-Amz-Date"); d != "" {
			t, err := time.Parse(iso8601, d)
			if err != nil {
				return nil, fmt.Errorf("X-Amz-Date %q is not an ISO8601 basic timestamp: %w", d, err)
			}
			secs, err := strconv.Atoi(q.Get("X-Amz-Expires"))
			if err != nil {
				return nil, fmt.Errorf("X-Amz-Expires %q is not a number of seconds: %w", q.Get("X-Amz-Expires"), err)
			}
			out.Expires = t.Add(time.Duration(secs) * time.Second)
		}
		return out, nil
	}

	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, nil
	}
	if !strings.HasPrefix(auth, sigV4Algorithm+" ") {
		// SigV2 and the pre-signed-header forms are not supported, and saying so beats letting
		// the request through as "unsigned" — an operator whose client fell back to V2 would
		// otherwise see writes land under no identity at all.
		return nil, fmt.Errorf("unsupported Authorization scheme %q (this store speaks %s only)",
			strings.SplitN(auth, " ", 2)[0], sigV4Algorithm)
	}
	out := &signedRequest{}
	for _, part := range strings.Split(strings.TrimPrefix(auth, sigV4Algorithm+" "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "Credential":
			key, region, err := splitCredential(v)
			if err != nil {
				return nil, err
			}
			out.AccessKey, out.Region = key, region
		case "Signature":
			out.Signature = v
		}
	}
	return out, nil
}

// splitCredential takes `AKID/20260826/us-east-1/s3/aws4_request` apart. The access key may
// itself contain slashes in no sane deployment, so the scope is read from the RIGHT.
func splitCredential(cred string) (accessKey, region string, err error) {
	parts := strings.Split(cred, "/")
	if len(parts) < 5 {
		return "", "", fmt.Errorf("credential %q is not <key>/<date>/<region>/<service>/aws4_request", cred)
	}
	scope := parts[len(parts)-4:]
	return strings.Join(parts[:len(parts)-4], "/"), scope[1], nil
}

// PresignOptions is one presigned GET. Every field is required except Now, because a presigned
// URL with a defaulted anything is a URL that works on one machine.
type PresignOptions struct {
	// Endpoint is the base URL the RECIPIENT will call — scheme, host and port, e.g.
	// "http://10.124.0.2:8333". It is part of the signature (Host is a signed header), so it is
	// not cosmetic: a URL signed for one host and fetched at another has an invalid signature.
	// This is the split-horizon problem KONTRA_S3_PUBLIC_ENDPOINT exists for.
	Endpoint string
	Bucket   string
	Key      string
	Region   string

	AccessKey string
	SecretKey string

	// Expires is how long the URL stays valid. S3 caps this at 7 days for SigV4.
	Expires time.Duration

	// Now is the signing instant; zero means time.Now().UTC(). Present so a test can pin a
	// signature to a published vector.
	Now time.Time
}

// Presign returns a presigned GET URL for one object, path-style.
//
// PATH-STYLE, ALWAYS, and that is not a shortcut. Virtual-host addressing puts the bucket in the
// hostname, which needs DNS that resolves `kontra.<controller>` — the appliance is an IP and a
// port. Every kontra client is already configured path-style (`forcePathStyle`, `UsePathStyle`,
// `s3_url_style='path'`), so this matches what the rest of the system speaks.
func Presign(o PresignOptions) (string, error) {
	if o.Endpoint == "" {
		return "", errors.New("presign: no endpoint — a presigned URL is signed FOR a host, so there is no default")
	}
	if o.Bucket == "" || o.Key == "" {
		return "", errors.New("presign: bucket and key are both required")
	}
	if o.AccessKey == "" || o.SecretKey == "" {
		return "", errors.New("presign: no credentials to sign with")
	}
	base, err := url.Parse(o.Endpoint)
	if err != nil || base.Host == "" || base.Scheme == "" {
		return "", fmt.Errorf("presign: endpoint %q needs a scheme and a host (e.g. http://10.124.0.2:8333)", o.Endpoint)
	}
	region := o.Region
	if region == "" {
		region = DefaultS3Region
	}
	expires := o.Expires
	if expires <= 0 {
		expires = time.Hour
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	path := "/" + o.Bucket + "/" + strings.TrimPrefix(o.Key, "/")
	q := url.Values{
		"X-Amz-Algorithm":     {sigV4Algorithm},
		"X-Amz-Credential":    {o.AccessKey + "/" + credentialScope(now, region)},
		"X-Amz-Date":          {now.Format(iso8601)},
		"X-Amz-Expires":       {strconv.Itoa(int(expires / time.Second))},
		"X-Amz-SignedHeaders": {"host"},
	}
	sig := signV4(sigV4Input{
		Method:      http.MethodGet,
		Path:        path,
		Query:       q,
		Host:        base.Host,
		PayloadHash: unsignedPayload,
		At:          now,
		Region:      region,
		SecretKey:   o.SecretKey,
	})
	q.Set("X-Amz-Signature", sig)

	// Built by hand rather than through url.URL.String(): the path must reach the recipient
	// encoded EXACTLY as it was signed, and url.URL re-encodes on its own rules.
	return base.Scheme + "://" + base.Host + uriEncodePath(path) + "?" + canonicalQuery(q), nil
}

type sigV4Input struct {
	Method      string
	Path        string // decoded object path, e.g. /kontra/cas/ab/abcd…
	Query       url.Values
	Host        string
	PayloadHash string
	At          time.Time
	Region      string
	SecretKey   string
}

// signV4 computes the hex signature. It is the ONE place the algorithm lives; Presign calls it to
// mint, and a future verifier calls it to check.
func signV4(in sigV4Input) string {
	canonical := strings.Join([]string{
		in.Method,
		uriEncodePath(in.Path),
		canonicalQuery(in.Query),
		"host:" + in.Host + "\n",
		"host",
		in.PayloadHash,
	}, "\n")
	scope := credentialScope(in.At, in.Region)
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		in.At.Format(iso8601),
		scope,
		hex.EncodeToString(sha256sum([]byte(canonical))),
	}, "\n")
	return hex.EncodeToString(hmacSHA256(signingKey(in.SecretKey, in.At, in.Region), []byte(stringToSign)))
}

func credentialScope(at time.Time, region string) string {
	return at.UTC().Format(iso8601Date) + "/" + region + "/" + sigV4Service + "/aws4_request"
}

func signingKey(secret string, at time.Time, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(at.UTC().Format(iso8601Date)))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(sigV4Service))
	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func sha256sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// canonicalQuery is SigV4's query encoding: every parameter percent-encoded with the strict
// unreserved set, sorted by encoded name then encoded value, joined with '&'. url.Values.Encode()
// is NOT this — it leaves '/' alone in values, and that single difference is a wrong signature.
func canonicalQuery(q url.Values) string {
	pairs := make([]string, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, uriEncode(k)+"="+uriEncode(v))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// uriEncodePath encodes each path segment, leaving the separators alone — S3's rule for the
// canonical URI of a path-style request.
func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

// uriEncode is RFC 3986 unreserved-set encoding, uppercase hex. Notably '/' IS encoded (to
// %2F) — callers that must keep separators split first.
func uriEncode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteString("%")
			const hexdigits = "0123456789ABCDEF"
			b.WriteByte(hexdigits[c>>4])
			b.WriteByte(hexdigits[c&0xf])
		}
	}
	return b.String()
}
