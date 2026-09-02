"""Single source of the Kontra contract version token.

The codec claim-check marker and the actor manifest schemaVersion both REFERENCE
this token so there is one place to bump the version. The PRODUCED wire strings
must stay byte-identical to today:
  - codec marker:    b"binary/claim-check-v1"   (separator '-v')
  - manifest schema:  "kontra.actor.v1"         (separator '.v')
Only the bare token below is shared; each call site keeps its own prefix+separator.

NOT shared with the buf-owned proto package `kontra.v1` (governed by buf), nor with
the deliberately-independent copies in conformance/codec/build_fixtures.py,
backend/src/codec/claimCheck.ts, and the orchestrator TS schemaVersion sites —
those are the cross-language/cross-tool checks and must NOT import this constant.
"""

CONTRACT_VERSION = "v1"
