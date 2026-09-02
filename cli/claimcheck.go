// claimcheck.go — the CLI's side of the claim-check codec, which is the HANDLER's codec.
//
// WHAT USED TO BE HERE. `kontra workflow start --wait` needs to read a result Temporal stored as a
// ref, so this file carried a fifth implementation of the wire format: its own copy of the
// `binary/claim-check-v1` marker, its own `{sha256,size,meta}` struct, its own base64 metadata
// round-trip, its own sha256 verification, and its own `cas/<sha[:2]>/<sha>` key built with a
// Sprintf. Its header cited shared/conformance/codec/fixtures.json as the thing that pins all of it. No
// test in this package had ever opened that file.
//
// It was also WRONG, in the one place a copy of an address is always eventually wrong. The key was
// `fmt.Sprintf("%s/%s/%scas/%s/%s", endpoint, bucket, prefix, sha[:2], sha)` — the prefix
// concatenated with no separator — so with KONTRA_S3_PREFIX=`runs` this CLI fetched
// `runscas/ab/…` where the key is `runs/cas/ab/…`. It shows up only under a non-default prefix,
// only on a payload over 128 KiB, and only at `--wait` time.
//
// WHICH SIDE IS RIGHT, MEASURED AND NOT ASSUMED, because the obvious sentence here ("every other
// implementation joins with a slash") is false. `runtime/handler/internal/objectstore.Key` and
// `control/orchestrator/src/codec/objectStore.ts:key` drop the prefix in as a SEGMENT;
// `runtime/python/internals/casstore.py` does `self._prefix + key`, which is this same bug on the
// Python worker's WRITE path. Executed against the live control plane with KONTRA_S3_PREFIX=slice11:
// the Python-served workflow wrote `slice11cas/df/df5b…` and this CLI asked for
// `slice11/cas/df/df5b…`. So the address had two writers spelling it each way, no row in
// shared/conformance/codec/fixtures.json carries a prefix to catch it, and this change puts the CLI on the
// side objectstore.Key defines. The Python one is a separate bug and is not fixed here.
//
// SINCE FIXED, AND THE SPLIT WAS WORSE THAN THIS PARAGRAPH SAYS. `runtime/go/codec/s3.go`
// concatenated too, under a comment reading "raw; every SDK concatenates it verbatim" — so it was
// three joiners (handler, orchestrator, this CLI) against two concatenators (both actor SDKs), not
// three against one. Both SDKs now join, and `shared/conformance/codec/fixtures.json` grew the
// `prefixCases` rows that hold all six arms to it; see `wireFormat.prefixTrailingSlash` there for
// what a slash-terminated prefix means and why the joiners changed with them.
//
// WHAT IS LEFT IS A TRANSPORT, and that part is genuinely this binary's. The CLI carries no S3 SDK
// and reaches the store unsigned over plain HTTP — the same assumption `kontra runs --query` makes
// with DuckDB — so it brings a `claimcheck.Backing` and nothing else. The marker, the ref shape,
// the metadata decoding, the integrity check and the key layout come from
// runtime/handler/claimcheck, reached the way casstore and hydratestore already are.
//
// DECODE-ONLY, ON PURPOSE. A workflow's result comes back offloaded whenever it exceeds 128 KiB,
// and without the codec the SDK fails on `Unknown payload encoding binary/claim-check-v1` — so
// `--wait` would work on small results and break on real ones, the exact failure this codec exists
// to prevent everywhere else. Encoding is the opposite case: this CLI is a caller, its argument is
// a flag on a command line, and a flag big enough to need offloading is a flag that wants to be a
// dataset. So Encode is overridden to a passthrough rather than the store being made unwritable —
// the refusal is a property of this command, stated once, and not an accident of a backing that
// happens to 405.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/config"
	"github.com/medmahmoudi26/kontra/runtime/handler/claimcheck"
)

// casOverHTTP is a claimcheck.Backing that GETs an object out of the store over unsigned HTTP,
// path-style: <endpoint>/<bucket>/<key>. The KEY is handed to it already built — prefix and
// `cas/<sha[:2]>/<sha>` are objectstore's, not this file's — so the only thing here is the URL
// join and the error an operator can act on.
type casOverHTTP struct {
	endpoint string // http://host:port
	bucket   string
}

// Get fetches one object. A MISSING OBJECT IS AN ERROR HERE, not a `found=false`, and the reason
// is the surface: this backing is read-only and every read is a payload a human is waiting on, so
// there is no caller that can do anything with "absent" except fail — and failing without the URL
// sends an operator to look at the store rather than at the endpoint they pointed this at.
func (c casOverHTTP) Get(ctx context.Context, key string) ([]byte, bool, error) {
	url := c.endpoint + "/" + c.bucket + "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("claim-check fetch %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("claim-check fetch %s: %w (is KONTRA_S3_ENDPOINT right?)", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		switch resp.StatusCode {
		case http.StatusForbidden, http.StatusUnauthorized:
			hint = " — the object store wants credentials, which this command does not send"
		case http.StatusNotFound:
			hint = " — nothing is stored under that address (are KONTRA_S3_BUCKET and KONTRA_S3_PREFIX the ones the writers used?)"
		}
		return nil, false, fmt.Errorf("claim-check fetch %s: HTTP %d%s", url, resp.StatusCode, hint)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("claim-check fetch %s: %w", url, err)
	}
	return data, true, nil
}

// errCASReadOnly is what the three write methods answer. They are unreachable through
// decodeOnlyCodec, and they say why rather than panicking: a future caller that wires this backing
// into an encoding path gets a sentence instead of a stack trace.
var errCASReadOnly = errors.New(
	"the CLI's claim-check store is read-only: it fetches results over unsigned HTTP and " +
		"carries no credentials to write with")

func (casOverHTTP) Put(context.Context, string, []byte) error    { return errCASReadOnly }
func (casOverHTTP) Exists(context.Context, string) (bool, error) { return false, errCASReadOnly }
func (casOverHTTP) Delete(context.Context, string) error         { return errCASReadOnly }

// decodeOnlyCodec is the handler's codec with Encode disarmed. It EMBEDS rather than reimplements,
// so Decode — the marker check, the ref parse, the metadata base64, the digest verification — is
// the one in runtime/handler/internal/codec and cannot drift from it.
type decodeOnlyCodec struct{ *claimcheck.Codec }

func (decodeOnlyCodec) Encode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	return payloads, nil
}

var _ converter.PayloadCodec = decodeOnlyCodec{}

// cliClaimCheckCodec builds the codec this binary reads results with, from the same KONTRA_S3_*
// environment every other implementation reads.
func cliClaimCheckCodec() converter.PayloadCodec {
	return decodeOnlyCodec{claimcheck.New(
		casOverHTTP{endpoint: "http://" + s3HostPort(), bucket: s3Bucket()},
		cliutil.EnvOr("KONTRA_S3_PREFIX", ""),
		claimcheck.ThresholdFromEnv(),
	)}
}

// dialWithClaimCheck dials Temporal with the codec above installed.
func dialWithClaimCheck() (client.Client, error) {
	return client.Dial(client.Options{
		HostPort:      config.TemporalAddress(),
		Namespace:     config.TemporalNamespace(),
		DataConverter: converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), cliClaimCheckCodec()),
	})
}
