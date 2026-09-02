/**
 * DataConverter wiring for the claim-check codec — the TS equivalent of Python's
 * single `payload_codec`. `{ payloadCodecs: [new ClaimCheckCodec()] }` is passed
 * as the `dataConverter` to both the Temporal Client and Worker (later units).
 *
 * The codec reads `KONTRA_S3_*` from the environment, so this default instance is
 * a passthrough unless `KONTRA_S3_ENDPOINT` is set.
 */

import type { PayloadCodec } from '@temporalio/common';

import { ClaimCheckCodec } from './claimCheck';

/**
 * The subset of Temporal's `DataConverter` we populate here. Declared structurally
 * (not via the imported `DataConverter` type) so this file compiles regardless of
 * minor SDK type-name drift; the object is still assignable to the `dataConverter`
 * option accepted by both `Client` and `Worker.create`.
 */
export interface CodecDataConverter {
  payloadCodecs: PayloadCodec[];
}

/** Shared default DataConverter — one ClaimCheckCodec instance for Client + Worker. */
export const dataConverter: CodecDataConverter = {
  payloadCodecs: [new ClaimCheckCodec()],
};
