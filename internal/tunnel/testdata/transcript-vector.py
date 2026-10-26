# Independent test vector for spec/tunnel.md §2.2 (key id) and §4.1 (transcript info, HPKE export proof).
# Python only (hashlib, hmac, base64, cryptography's raw X25519); RFC 9180 is written out by hand, not taken
# from the Go code under test. Regenerate: python3 transcript-vector.py > transcript-vector.json
import base64, hashlib, hmac, json, struct, sys
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey, X25519PublicKey

CHARSET = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
GEN = [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3]

def polymod(values):
    chk = 1
    for v in values:
        b = chk >> 25
        chk = ((chk & 0x1ffffff) << 5) ^ v
        for i in range(5):
            chk ^= GEN[i] if ((b >> i) & 1) else 0
    return chk

def hrp_expand(hrp):
    return [ord(x) >> 5 for x in hrp] + [0] + [ord(x) & 31 for x in hrp]

def convertbits(data, frombits, tobits):
    acc = 0; bits = 0; ret = []; maxv = (1 << tobits) - 1
    for value in data:
        acc = (acc << frombits) | value; bits += frombits
        while bits >= tobits:
            bits -= tobits; ret.append((acc >> bits) & maxv)
    if bits:
        ret.append((acc << (tobits - bits)) & maxv)
    return ret

def bech32_encode(hrp, data8):
    data = convertbits(data8, 8, 5)
    lhrp = hrp.lower()
    values = hrp_expand(lhrp) + data
    mod = polymod(values + [0] * 6) ^ 1
    chk = [(mod >> 5 * (5 - i)) & 31 for i in range(6)]
    s = lhrp + "1" + "".join(CHARSET[d] for d in data + chk)
    return s.upper() if hrp.isupper() else s

def key_id(recipient):
    d = hashlib.sha256(recipient.encode()).digest()
    return "k1_" + base64.b32encode(d).decode().lower().rstrip("=")[:26]

def lp(x):
    assert len(x) < 65536
    return struct.pack(">H", len(x)) + x

def info(origin, cb, cb_value, hello, session, gate_nonce):
    m = b"kontra.sh/tunnel/v1" + b"\x00"
    m += lp(origin.encode()) + lp(cb.encode()) + lp(cb_value)
    m += lp(hashlib.sha256(hello).digest()) + lp(session.encode()) + lp(gate_nonce)
    return hashlib.sha256(m).digest()

# RFC 9180, base mode, DHKEM(X25519, HKDF-SHA256) 0x0020, HKDF-SHA256 0x0001, export-only 0xFFFF.
def extract(salt, ikm):
    return hmac.new(salt if salt else b"\x00" * 32, ikm, hashlib.sha256).digest()

def expand(prk, info_, L):
    out = b""; t = b""; i = 1
    while len(out) < L:
        t = hmac.new(prk, t + info_ + bytes([i]), hashlib.sha256).digest()
        out += t; i += 1
    return out[:L]

def labeled_extract(suite, salt, label, ikm):
    return extract(salt, b"HPKE-v1" + suite + label + ikm)

def labeled_expand(suite, prk, label, info_, L):
    return expand(prk, struct.pack(">H", L) + b"HPKE-v1" + suite + label + info_, L)

KEM_SUITE = b"KEM" + struct.pack(">H", 0x0020)
HPKE_SUITE = b"HPKE" + struct.pack(">HHH", 0x0020, 0x0001, 0xFFFF)

def pub(sk):
    return X25519PrivateKey.from_private_bytes(sk).public_key().public_bytes_raw()

def export(sk_r, enc, info_, label, L=32):
    dh = X25519PrivateKey.from_private_bytes(sk_r).exchange(X25519PublicKey.from_public_bytes(enc))
    kem_context = enc + pub(sk_r)
    eae_prk = labeled_extract(KEM_SUITE, b"", b"eae_prk", dh)
    shared = labeled_expand(KEM_SUITE, eae_prk, b"shared_secret", kem_context, 32)
    psk_id_hash = labeled_extract(HPKE_SUITE, b"", b"psk_id_hash", b"")
    info_hash = labeled_extract(HPKE_SUITE, b"", b"info_hash", info_)
    ksc = b"\x00" + psk_id_hash + info_hash
    secret = labeled_extract(HPKE_SUITE, shared, b"secret", b"")
    exporter_secret = labeled_expand(HPKE_SUITE, secret, b"exp", ksc, 32)
    return labeled_expand(HPKE_SUITE, exporter_secret, b"sec", label.encode(), L)

def seed(name):
    return hashlib.sha256(b"kontra.sh/tunnel test vector: " + name.encode()).digest()

node_sk, old_sk = seed("node key"), seed("old node key")
eph, eph_old = seed("gate ephemeral"), seed("gate ephemeral old")
recipient = bech32_encode("age", pub(node_sk))
old_recipient = bech32_encode("age", pub(old_sk))
kid, old_kid = key_id(recipient), key_id(old_recipient)
hello = json.dumps({"t": "hello", "v": 1, "origin": "https://gate.example", "kid": kid, "recipient": recipient,
                    "name": "build-box-3", "nonce": base64.urlsafe_b64encode(seed("node nonce")).decode().rstrip("="),
                    "services": [{"kind": "a2a", "name": "hello"}],
                    "rotate_from": {"kid": old_kid, "recipient": old_recipient},
                    "agent": "kontra/4.0.0 (linux/amd64)"}, separators=(",", ":")).encode()
session = "0192f0b4-0000-7000-8000-000000000001"
gate_nonce = seed("gate nonce")
cb_value = seed("tls exporter")
enc, enc_old = pub(eph), pub(eph_old)

cases = []
for cb, cbv in (("tls-exporter", cb_value), ("none", b"")):
    i = info("https://gate.example", cb, cbv, hello, session, gate_nonce)
    cases.append({
        "cb": cb, "cb_value_hex": cbv.hex(), "info_hex": i.hex(),
        "proof_hex": export(node_sk, enc, i, "kontra.sh/tunnel/pop/v1").hex(),
        "proof_old_hex": export(old_sk, enc_old, i, "kontra.sh/tunnel/pop-old/v1").hex(),
    })

out = {
    "about": "Computed independently of the Go code (Python: hashlib, hmac, cryptography's raw X25519, RFC 9180 written out) by transcript-vector.py in this directory (its RFC 9180 code reproduces RFC 9180 A.7 exports); spec/tunnel.md §2.2, §4.1.",
    "node_scalar_hex": node_sk.hex(),
    "node_secret_key": bech32_encode("AGE-SECRET-KEY-", node_sk),
    "recipient": recipient, "kid": kid,
    "old_scalar_hex": old_sk.hex(), "old_recipient": old_recipient, "old_kid": old_kid,
    "origin": "https://gate.example",
    "hello": hello.decode(),
    "session": session, "gate_nonce_hex": gate_nonce.hex(),
    "enc_hex": enc.hex(), "enc_old_hex": enc_old.hex(),
    "cases": cases,
}
json.dump(out, sys.stdout, indent=2)
print()
