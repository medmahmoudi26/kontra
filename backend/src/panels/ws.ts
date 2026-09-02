/**
 * The WebSocket wire — RFC 6455, by hand, plus the Terminal-tagged binary framing (ADR 0020).
 *
 * WHY BY HAND. The streamer needs a server-side WebSocket, and the alternative was a new runtime
 * dependency in the process that also holds the cloud credential and the fleet SSH key. What is
 * actually needed is small and closed: an accept-key, a frame encoder, a frame decoder that
 * unmasks and bounds, and a close. Written here, the framing is a pure function — which is what
 * lets `[idLen][id][payload]` and the snapshot's clear-screen prefix be pinned by unit tests
 * instead of asserted in a comment.
 *
 * ONE SOCKET PER TAB, carrying every Terminal. Browsers cap ~6 concurrent HTTP/1.1 streams per
 * origin, so per-tile SSE or streaming GETs deadlock a wall of tiles; one socket also puts
 * backpressure accounting in one place, which is the place that decides what to elide.
 *
 * Client→server is text JSON; server→client is text JSON for control and binary for bytes. The
 * decoder bounds a client frame hard (64 KiB): the only thing a browser is allowed to send is a
 * short control message, and nothing in this design ever accepts bytes bound for a session.
 */

import { createHash, randomBytes } from 'node:crypto';

/** RFC 6455 §1.3. */
export const WS_GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';

export const OPCODE = {
  continuation: 0x0,
  text: 0x1,
  binary: 0x2,
  close: 0x8,
  ping: 0x9,
  pong: 0xa,
} as const;

/** A client control message is JSON, and small. Anything larger is a protocol error, not a big
 * message: there is no client→server path in this design that carries data. */
export const MAX_CLIENT_FRAME_BYTES = 64 * 1024;

export function acceptKey(key: string): string {
  return createHash('sha1')
    .update(key + WS_GUID)
    .digest('base64');
}

/** The 101 response, verbatim bytes. No extensions are negotiated — permessage-deflate would put
 * a compressor between a Machine's output and a tile for no gain on escape-heavy screens. */
export function handshakeResponse(key: string): string {
  return (
    'HTTP/1.1 101 Switching Protocols\r\n' +
    'Upgrade: websocket\r\n' +
    'Connection: Upgrade\r\n' +
    `Sec-WebSocket-Accept: ${acceptKey(key)}\r\n` +
    '\r\n'
  );
}

/** Encode one server frame. Server→client frames are never masked (RFC 6455 §5.1). */
export function encodeFrame(opcode: number, payload: Buffer = Buffer.alloc(0)): Buffer {
  const len = payload.length;
  let header: Buffer;
  if (len < 126) {
    header = Buffer.alloc(2);
    header[1] = len;
  } else if (len < 65536) {
    header = Buffer.alloc(4);
    header[1] = 126;
    header.writeUInt16BE(len, 2);
  } else {
    header = Buffer.alloc(10);
    header[1] = 127;
    // 64-bit length, high word zero: a frame this side of 4 GiB is already absurd.
    header.writeUInt32BE(0, 2);
    header.writeUInt32BE(len, 6);
  }
  header[0] = 0x80 | (opcode & 0x0f); // FIN
  return Buffer.concat([header, payload]);
}

export function textFrame(s: string): Buffer {
  return encodeFrame(OPCODE.text, Buffer.from(s, 'utf8'));
}

export function binaryFrame(payload: Buffer): Buffer {
  return encodeFrame(OPCODE.binary, payload);
}

/** A close frame with a status code and a short reason. */
export function closeFrame(code = 1000, reason = ''): Buffer {
  const body = Buffer.alloc(2 + Buffer.byteLength(reason));
  body.writeUInt16BE(code, 0);
  body.write(reason, 2, 'utf8');
  return encodeFrame(OPCODE.close, body);
}

export interface DecodedFrame {
  opcode: number;
  payload: Buffer;
}

/** A protocol violation. Carries the RFC close code the socket should die with. */
export class WsProtocolError extends Error {
  constructor(
    message: string,
    readonly code: number
  ) {
    super(message);
    this.name = 'WsProtocolError';
  }
}

/**
 * Incremental frame decoder.
 *
 * Feed it TCP chunks, take whole frames out. It unmasks (client frames must be masked), refuses
 * an oversized frame or an oversized reassembly, and reassembles continuations — a browser will
 * not fragment a 40-byte JSON message, but a decoder that assumes so is a decoder that breaks on
 * the first proxy that does.
 */
export class FrameDecoder {
  private buf: Buffer = Buffer.alloc(0);
  private fragments: Buffer[] = [];
  private fragmentOpcode = 0;
  private fragmentBytes = 0;

  /**
   * `requireMask` is true for the server's read path, where RFC 6455 §5.1 makes masking mandatory.
   * It is false only for reading the SERVER's own frames — which is what a test client does, and the
   * reason it is a parameter rather than a constant.
   */
  constructor(
    private readonly maxFrameBytes = MAX_CLIENT_FRAME_BYTES,
    private readonly requireMask = true
  ) {}

  push(chunk: Buffer): DecodedFrame[] {
    this.buf = this.buf.length ? Buffer.concat([this.buf, chunk]) : chunk;
    const out: DecodedFrame[] = [];
    for (;;) {
      const frame = this.shift();
      if (!frame) return out;
      if (frame.opcode >= 0x8) {
        // Control frames are never fragmented and never queued behind a reassembly.
        out.push(frame);
        continue;
      }
      if (!frame.fin || frame.opcode === OPCODE.continuation) {
        if (frame.opcode !== OPCODE.continuation) {
          this.fragmentOpcode = frame.opcode;
          this.fragments = [];
          this.fragmentBytes = 0;
        } else if (!this.fragments.length && !this.fragmentOpcode) {
          throw new WsProtocolError('continuation frame with nothing to continue', 1002);
        }
        this.fragments.push(frame.payload);
        this.fragmentBytes += frame.payload.length;
        if (this.fragmentBytes > this.maxFrameBytes) {
          throw new WsProtocolError('fragmented message too large', 1009);
        }
        if (frame.fin) {
          out.push({ opcode: this.fragmentOpcode, payload: Buffer.concat(this.fragments) });
          this.fragments = [];
          this.fragmentOpcode = 0;
          this.fragmentBytes = 0;
        }
        continue;
      }
      out.push({ opcode: frame.opcode, payload: frame.payload });
    }
  }

  /** Pull one complete frame out of the buffer, or return null when more bytes are needed. */
  private shift(): (DecodedFrame & { fin: boolean }) | null {
    const b = this.buf;
    if (b.length < 2) return null;
    const first = b[0] as number;
    const second = b[1] as number;
    const fin = (first & 0x80) !== 0;
    const opcode = first & 0x0f;
    const masked = (second & 0x80) !== 0;
    let len = second & 0x7f;
    let offset = 2;

    if (opcode >= 0x8) {
      if (!fin) throw new WsProtocolError('fragmented control frame', 1002);
      if (len > 125) throw new WsProtocolError('control frame too long', 1002);
    }

    if (len === 126) {
      if (b.length < offset + 2) return null;
      len = b.readUInt16BE(offset);
      offset += 2;
    } else if (len === 127) {
      if (b.length < offset + 8) return null;
      const high = b.readUInt32BE(offset);
      const low = b.readUInt32BE(offset + 4);
      if (high !== 0) throw new WsProtocolError('frame too large', 1009);
      len = low;
      offset += 8;
    }
    if (len > this.maxFrameBytes) throw new WsProtocolError('frame too large', 1009);
    // A client that does not mask is either broken or not a browser; RFC 6455 §5.1 requires it,
    // and accepting unmasked frames is how a proxy gets talked into cache poisoning.
    if (!masked && this.requireMask) throw new WsProtocolError('client frame is not masked', 1002);
    const maskLen = masked ? 4 : 0;
    if (b.length < offset + maskLen + len) return null;

    const mask = masked ? b.subarray(offset, offset + 4) : null;
    offset += maskLen;
    const payload = Buffer.allocUnsafe(len);
    for (let i = 0; i < len; i += 1) {
      const byte = b[offset + i] as number;
      payload[i] = mask ? byte ^ (mask[i & 3] as number) : byte;
    }
    this.buf = b.subarray(offset + len);
    return { opcode, payload, fin };
  }
}

/** Longest id a tagged frame can carry — the length prefix is one byte. Terminal ids run ~45
 * bytes; the cap is checked so a future provider cannot silently truncate one. */
export const MAX_TAGGED_ID_BYTES = 255;

/**
 * `[1 byte idLen][idLen bytes utf8 id][payload]`.
 *
 * The whole reason the wire is binary: xterm.js `write()` takes bytes, and a Terminal's bytes are
 * escape sequences, not text. Tagging in-band is what makes one socket carry a whole wall.
 */
export function encodeTagged(id: string, payload: Buffer): Buffer {
  const idBytes = Buffer.from(id, 'utf8');
  if (!idBytes.length || idBytes.length > MAX_TAGGED_ID_BYTES) {
    throw new Error(`panels: Terminal id of ${idBytes.length} bytes cannot be framed`);
  }
  const out = Buffer.allocUnsafe(1 + idBytes.length + payload.length);
  out[0] = idBytes.length;
  idBytes.copy(out, 1);
  payload.copy(out, 1 + idBytes.length);
  return out;
}

/** The decode half — used by tests and by the browser client's mirror of this function. */
export function decodeTagged(buf: Buffer): { id: string; payload: Buffer } {
  if (!buf.length) throw new Error('panels: empty tagged frame');
  const idLen = buf[0] as number;
  if (!idLen || buf.length < 1 + idLen) throw new Error('panels: truncated tagged frame');
  return {
    id: buf.subarray(1, 1 + idLen).toString('utf8'),
    payload: buf.subarray(1 + idLen),
  };
}

/** A client-side handshake key, for tests that speak the protocol against the real server. */
export function newClientKey(): string {
  return randomBytes(16).toString('base64');
}

/** Mask and encode a frame the way a browser does — test-side only, but it lives beside the
 * decoder it is the inverse of. */
export function encodeMaskedFrame(opcode: number, payload: Buffer): Buffer {
  const mask = randomBytes(4);
  const masked = Buffer.allocUnsafe(payload.length);
  for (let i = 0; i < payload.length; i += 1) {
    masked[i] = (payload[i] as number) ^ (mask[i & 3] as number);
  }
  const len = payload.length;
  let header: Buffer;
  if (len < 126) {
    header = Buffer.alloc(2);
    header[1] = 0x80 | len;
  } else if (len < 65536) {
    header = Buffer.alloc(4);
    header[1] = 0x80 | 126;
    header.writeUInt16BE(len, 2);
  } else {
    header = Buffer.alloc(10);
    header[1] = 0x80 | 127;
    header.writeUInt32BE(0, 2);
    header.writeUInt32BE(len, 6);
  }
  header[0] = 0x80 | (opcode & 0x0f);
  return Buffer.concat([header, mask, masked]);
}
