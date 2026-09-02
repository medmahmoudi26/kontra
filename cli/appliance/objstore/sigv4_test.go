package objstore

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPresignMatchesAWSDocumentedVector pins the signer to AWS's own published example rather
// than to our own output.
//
// A signature test that signs with our code and checks with our code proves only that the code is
// deterministic. This vector is the one in the SigV4 documentation's "Example: GET Object
// (presigned URL)" — a known key, a known secret, a frozen clock and the signature AWS says comes
// out. If the canonical request, the scope, the signing-key chain or the query encoding drifts by
// one byte, this fails, which is the only way to find out before a client does.
func TestPresignMatchesAWSDocumentedVector(t *testing.T) {
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	q := url.Values{
		"X-Amz-Algorithm":     {sigV4Algorithm},
		"X-Amz-Credential":    {"AKIAIOSFODNN7EXAMPLE/" + credentialScope(at, "us-east-1")},
		"X-Amz-Date":          {at.Format(iso8601)},
		"X-Amz-Expires":       {"86400"},
		"X-Amz-SignedHeaders": {"host"},
	}
	// Virtual-host addressing, because that is the form the documented example uses. The store
	// itself only ever signs path-style; the algorithm underneath is the same one.
	got := signV4(sigV4Input{
		Method:      http.MethodGet,
		Path:        "/test.txt",
		Query:       q,
		Host:        "examplebucket.s3.amazonaws.com",
		PayloadHash: unsignedPayload,
		At:          at,
		Region:      "us-east-1",
		SecretKey:   "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	})
	const want = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got != want {
		t.Fatalf("presign signature drifted from the AWS documented vector:\n  got  %s\n  want %s", got, want)
	}
}

func TestPresignBuildsAPathStyleURL(t *testing.T) {
	u, err := Presign(PresignOptions{
		Endpoint: "http://10.124.0.2:8333", Bucket: "kontra",
		Key:    "datasets/actor=crawl/dt=2026-08-26/part-0.parquet",
		Region: "us-east-1", AccessKey: "kontra", SecretKey: "kontra",
		Expires: 15 * time.Minute, Now: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "http://10.124.0.2:8333/kontra/datasets/actor%3Dcrawl/") {
		t.Fatalf("not a path-style URL signed for the given endpoint: %s", u)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature"} {
		if parsed.Query().Get(k) == "" {
			t.Errorf("presigned URL is missing %s", k)
		}
	}
	// The path is signed with '=' percent-encoded, so it must ARRIVE that way. A URL whose path
	// is re-encoded on the way out is a URL whose signature no longer covers it — which is
	// invisible until the day signatures are enforced.
	if strings.Contains(parsed.EscapedPath(), "actor=crawl") {
		t.Errorf("the '=' in a hive partition was not encoded as it was signed: %s", parsed.EscapedPath())
	}
}

func TestPresignRefusesWhatItCannotGuess(t *testing.T) {
	for _, tc := range []struct {
		why  string
		opts PresignOptions
		want string
	}{
		{"no endpoint", PresignOptions{Bucket: "b", Key: "k", AccessKey: "a", SecretKey: "s"}, "endpoint"},
		{"endpoint with no scheme", PresignOptions{Endpoint: "10.0.0.1:8333", Bucket: "b", Key: "k", AccessKey: "a", SecretKey: "s"}, "scheme"},
		{"no credentials", PresignOptions{Endpoint: "http://h:1", Bucket: "b", Key: "k"}, "credentials"},
		{"no key", PresignOptions{Endpoint: "http://h:1", Bucket: "b", AccessKey: "a", SecretKey: "s"}, "bucket and key"},
	} {
		_, err := Presign(tc.opts)
		if err == nil {
			t.Errorf("%s: presigned anyway", tc.why)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error does not name the missing thing: %v", tc.why, err)
		}
	}
}

func TestParseSigV4ReadsBothCarriers(t *testing.T) {
	t.Run("authorization header", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodPut, "http://s/kontra/x", nil)
		r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKID/20260826/eu-west-1/s3/aws4_request, "+
			"SignedHeaders=host;x-amz-date, Signature=abc123")
		got, err := parseSigV4(r)
		if err != nil {
			t.Fatal(err)
		}
		if got.AccessKey != "AKID" || got.Region != "eu-west-1" || got.Signature != "abc123" || got.Presigned {
			t.Fatalf("parsed wrong: %+v", got)
		}
	})

	t.Run("presigned query", func(t *testing.T) {
		at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
		u, err := Presign(PresignOptions{
			Endpoint: "http://h:8333", Bucket: "kontra", Key: "a/b", Region: "us-east-1",
			AccessKey: "kontra", SecretKey: "secret", Expires: time.Hour, Now: at,
		})
		if err != nil {
			t.Fatal(err)
		}
		r, _ := http.NewRequest(http.MethodGet, u, nil)
		got, err := parseSigV4(r)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Presigned || got.AccessKey != "kontra" {
			t.Fatalf("parsed wrong: %+v", got)
		}
		if !got.Expires.Equal(at.Add(time.Hour)) {
			t.Fatalf("expiry %v, want %v", got.Expires, at.Add(time.Hour))
		}
	})

	// An unsigned request is NOT an error. Signatures are parsed and not enforced (s3.go), so a
	// caller with no credential at all has to parse to "nothing presented" rather than to a
	// refusal — DuckDB's httpfs and the browser's presigned reads both depend on the distinction.
	t.Run("anonymous", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "http://s/kontra/datasets/x.parquet", nil)
		got, err := parseSigV4(r)
		if err != nil || got != nil {
			t.Fatalf("anonymous request should parse to (nil, nil), got (%+v, %v)", got, err)
		}
	})

	t.Run("a scheme we do not speak is named", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "http://s/kontra/x", nil)
		r.Header.Set("Authorization", "AWS AKID:signature")
		if _, err := parseSigV4(r); err == nil || !strings.Contains(err.Error(), "AWS4-HMAC-SHA256") {
			t.Fatalf("a SigV2 header should be refused by name, got %v", err)
		}
	})
}
