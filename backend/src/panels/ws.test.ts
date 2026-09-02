import { describe, expect, it } from 'vitest';
import {
  acceptKey,
  binaryFrame,
  decodeTagged,
  encodeFrame,
  encodeMaskedFrame,
  encodeTagged,
  FrameDecoder,
  handshakeResponse,
  MAX_CLIENT_FRAME_BYTES,
  OPCODE,
  textFrame,
  WsProtocolError,
} from './ws';

describe('the WebSocket handshake', () => {
  it('computes the accept key from RFC 6455 §1.3', () => {
    // The RFC's own example. If this drifts, every browser refuses the socket with no useful error.
    expect(acceptKey('dGhlIHNhbXBsZSBub25jZQ==')).toBe('s3pPLMBiTxaQ9kYGzzhZRbK+xOo=');
    expect(handshakeResponse('dGhlIHNhbXBsZSBub25jZQ==')).toContain(
      'Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo='
    );
  });

  it('negotiates no extensions', () => {
    // permessage-deflate would put a compressor between a Machine's output and a tile, on bytes
    // that are mostly escape sequences.
    expect(handshakeResponse('x')).not.toMatch(/Sec-WebSocket-Extensions/i);
  });
});

describe('server frames', () => {
  it('encodes the three length forms, unmasked', () => {
    for (const len of [0, 1, 125, 126, 127, 65535, 65536, 70_000]) {
      const frame = encodeFrame(OPCODE.binary, Buffer.alloc(len, 7));
      expect(frame[0]).toBe(0x82); // FIN + binary
      expect((frame[1] as number) & 0x80).toBe(0); // never masked
      const decoded = new FrameDecoder(1_000_000).push(
        // Re-mask it as a client would so the decoder will accept it back.
        encodeMaskedFrame(OPCODE.binary, frame.subarray(frame.length - len))
      );
      expect(decoded[0]?.payload.length).toBe(len);
    }
  });
});

describe('the frame decoder', () => {
  it('unmasks a text frame split across TCP chunks', () => {
    const frame = encodeMaskedFrame(OPCODE.text, Buffer.from('{"t":"subscribe"}'));
    const d = new FrameDecoder();
    // A 40-byte message arriving in two reads is not exotic; a decoder that assumes otherwise fails
    // the first time a proxy is in the path.
    expect(d.push(frame.subarray(0, 3))).toHaveLength(0);
    const out = d.push(frame.subarray(3));
    expect(out).toHaveLength(1);
    expect(out[0]?.payload.toString()).toBe('{"t":"subscribe"}');
  });

  it('reassembles continuation frames', () => {
    const d = new FrameDecoder();
    const part = (op: number, fin: boolean, s: string): Buffer => {
      const f = encodeMaskedFrame(op, Buffer.from(s));
      if (!fin) f[0] = (f[0] as number) & 0x7f;
      return f;
    };
    expect(d.push(part(OPCODE.text, false, '{"t":'))).toHaveLength(0);
    const out = d.push(part(OPCODE.continuation, true, '"ping"}'));
    expect(out[0]?.payload.toString()).toBe('{"t":"ping"}');
    expect(out[0]?.opcode).toBe(OPCODE.text);
  });

  it('refuses an unmasked client frame', () => {
    // RFC 6455 §5.1 requires masking, and accepting unmasked frames is how a proxy gets talked
    // into cache poisoning.
    const d = new FrameDecoder();
    expect(() => d.push(textFrame('hello'))).toThrow(WsProtocolError);
  });

  it('refuses an oversized frame with the right close code', () => {
    const d = new FrameDecoder(1024);
    try {
      d.push(encodeMaskedFrame(OPCODE.text, Buffer.alloc(2048)));
      expect.unreachable('should have thrown');
    } catch (err) {
      expect(err).toBeInstanceOf(WsProtocolError);
      expect((err as WsProtocolError).code).toBe(1009);
    }
    expect(MAX_CLIENT_FRAME_BYTES).toBe(64 * 1024);
  });

  it('refuses a fragmented control frame', () => {
    const d = new FrameDecoder();
    const f = encodeMaskedFrame(OPCODE.ping, Buffer.alloc(0));
    f[0] = 0x09; // FIN cleared
    expect(() => d.push(f)).toThrow(/fragmented control frame/);
  });
});

describe('the Terminal-tagged binary framing', () => {
  it('is [idLen][id][payload]', () => {
    const id = 'fleet:kf-crawl-01/kontra-webcrawl/actor';
    const payload = Buffer.from('\x1b[H\x1b[2Jhello');
    const framed = encodeTagged(id, payload);

    expect(framed[0]).toBe(Buffer.byteLength(id));
    expect(framed.subarray(1, 1 + id.length).toString('utf8')).toBe(id);
    expect(framed.subarray(1 + id.length)).toEqual(payload);
    expect(decodeTagged(framed)).toEqual({ id, payload });
  });

  it('survives the framing a client actually receives', () => {
    const id = 'fleet:kf-crawl-01/kontra-webcrawl/handler';
    const wire = binaryFrame(encodeTagged(id, Buffer.from([0x1b, 0x5b, 0x48])));
    // Strip the WS header the way a browser's WebSocket does, then decode the tag.
    expect(decodeTagged(wire.subarray(2)).id).toBe(id);
  });

  it('refuses an id the one-byte length cannot hold', () => {
    expect(() => encodeTagged('x'.repeat(256), Buffer.alloc(0))).toThrow();
    expect(() => encodeTagged('', Buffer.alloc(0))).toThrow();
    expect(() => decodeTagged(Buffer.from([200, 1, 2]))).toThrow(/truncated/);
  });
});
