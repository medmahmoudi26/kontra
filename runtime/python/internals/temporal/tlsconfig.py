"""The TLS half of a Temporal client's options, for both Python hosts.

WHAT WAS WRONG. Nothing in this repository could connect to a secured Temporal. Both Python hosts,
the orchestrator's eight TypeScript sites and six Go ones all spelled the address and stopped, so a
self-hoster who put Temporal behind mTLS — the ordinary thing to do with a server holding every
Run's history — had no way to point kontra at it. That is a gap in the OPEN product, which is why it
is closed here and not in anything commercial.

THE CONTRACT IS SHARED WITH TWO OTHER LANGUAGES and lives in `shared/conformance/temporal_tls.json`,
which `tests/test_temporal_tls.py` executes against this module. Sixteen client connections is the whole
difficulty: a change reaching twelve of them does not fail loudly, it produces a deployment that
mostly works and has one process talking plaintext to a server that accepts both — the worst of the
three outcomes, because it looks like the good one.

    KONTRA_TEMPORAL_TLS              1|true|yes|on — TLS with the system trust store
    KONTRA_TEMPORAL_TLS_CA           PEM path: the server's root CA, for a private CA
    KONTRA_TEMPORAL_TLS_CERT         PEM path: this client's certificate   ] both, or neither
    KONTRA_TEMPORAL_TLS_KEY          PEM path: this client's private key   ]
    KONTRA_TEMPORAL_TLS_SERVER_NAME  SNI override, for a proxy in front of the server

PLAINTEXT REMAINS THE DEFAULT. With none of these set, :func:`connect_tls` returns ``False`` — which
is what ``Client.connect`` means by "no TLS" and what both hosts effectively passed before this
module existed. An existing deployment cannot notice this file arrived.

ANY ONE OF THEM TURNS TLS ON, and the switch is not a master disable. Setting a CA and forgetting
the switch would otherwise read a certificate, build nothing from it, and connect in the clear: a
silent downgrade produced by configuration that looks complete.

A MISCONFIGURATION IS A REFUSAL. It never falls back to plaintext — a security setting that degrades
to off when it cannot be satisfied is the failure ``watchdog.sh`` shipped for years here, and the
shape ADR 0039 refuses for cosign. The error names the VARIABLE and the PATH, because an operator
needs to know which setting pointed where and a path is a filesystem location. Key MATERIAL never
reaches the message: this reads bytes and hands them to the SDK.

IT IS IN `internals`, NOT IN `actorkit`. An actor author never configures how the host reaches
Temporal; the host does. ADR 0035 §2 keeps the arrow running runtime -> sdk, and a TLS knob in the
author-facing package would be surface nobody imports.
"""

from __future__ import annotations

import os
from typing import Callable, Mapping, Optional, Union

from temporalio.service import TLSConfig

#: Every variable this module reads. Exported so a test can assert nothing else consults one — one
#: function, one reading, or there are two policies that agree today and drift later.
TLS_VARS = (
    "KONTRA_TEMPORAL_TLS",
    "KONTRA_TEMPORAL_TLS_CA",
    "KONTRA_TEMPORAL_TLS_CERT",
    "KONTRA_TEMPORAL_TLS_KEY",
    "KONTRA_TEMPORAL_TLS_SERVER_NAME",
)

#: `1`, `true`, `yes`, `on` — anything else is off. Case-insensitive, trimmed. The same four words
#: the Go and TypeScript arms accept, pinned by the corpus's `truthy`/`falsy` lists.
_TRUTHY = {"1", "true", "yes", "on"}

Env = Union[Mapping[str, str], Callable[[str], str], None]


def _get(env: Env, name: str) -> str:
    if env is None:
        return os.environ.get(name, "")
    if callable(env):
        return env(name) or ""
    return env.get(name, "") or ""


def _truthy(value: str) -> bool:
    return value.strip().lower() in _TRUTHY


def tls_requested(env: Env = None) -> bool:
    """Is any TLS setting present? ANY ONE turns TLS on — see the module docstring."""
    if _truthy(_get(env, "KONTRA_TEMPORAL_TLS")):
        return True
    return any(_get(env, name) for name in TLS_VARS[1:])


def _read_pem(env: Env, variable: str) -> bytes:
    """Read a PEM, or refuse.

    The variable AND the path are named; the CONTENTS never are, and nothing here decrypts, so no
    passphrase can reach a message through this function.
    """
    path = _get(env, variable)
    if not path:
        raise ValueError(f"{variable} is empty — set it to a PEM path or unset it entirely")
    try:
        with open(path, "rb") as fh:
            return fh.read()
    except OSError as exc:
        raise ValueError(
            f"{variable}={path} could not be read ({exc.strerror or exc}). Temporal TLS is "
            f"configured, so this is a refusal rather than a fall back to an unencrypted connection"
        ) from None  # `from None`: the OSError's own repr can carry the path twice and adds nothing


def connect_tls(env: Env = None) -> Union[bool, TLSConfig]:
    """What to pass as ``Client.connect(..., tls=...)``.

    ``False`` when nothing is configured, which is what the SDK means by "no TLS" and what both
    hosts effectively passed before this module existed. Otherwise a :class:`TLSConfig`.
    """
    if not tls_requested(env):
        return False

    kwargs: dict[str, object] = {}

    server_name = _get(env, "KONTRA_TEMPORAL_TLS_SERVER_NAME")
    if server_name:
        kwargs["domain"] = server_name

    if _get(env, "KONTRA_TEMPORAL_TLS_CA"):
        kwargs["server_root_ca_cert"] = _read_pem(env, "KONTRA_TEMPORAL_TLS_CA")

    # BOTH OR NEITHER. A certificate without a key is not a partial configuration that could be
    # completed at connect time — it is mTLS that will not authenticate, and the server's rejection
    # arrives as a handshake failure naming neither variable.
    has_cert = bool(_get(env, "KONTRA_TEMPORAL_TLS_CERT"))
    has_key = bool(_get(env, "KONTRA_TEMPORAL_TLS_KEY"))
    if has_cert != has_key:
        missing = "KONTRA_TEMPORAL_TLS_KEY" if has_cert else "KONTRA_TEMPORAL_TLS_CERT"
        raise ValueError(
            f"KONTRA_TEMPORAL_TLS_CERT and KONTRA_TEMPORAL_TLS_KEY must be set together — "
            f"{missing} is missing. A client certificate without its key cannot authenticate, and "
            f"the server would refuse the handshake without naming either"
        )
    if has_cert and has_key:
        kwargs["client_cert"] = _read_pem(env, "KONTRA_TEMPORAL_TLS_CERT")
        kwargs["client_private_key"] = _read_pem(env, "KONTRA_TEMPORAL_TLS_KEY")

    return TLSConfig(**kwargs)  # type: ignore[arg-type]


def describe(address: str, tls: Union[bool, TLSConfig]) -> str:
    """One line for a boot log: which Temporal, encrypted or not, with a client certificate or not.

    Names the MODE and never the material, which is the whole of what is safe to print and exactly
    what an operator needs when a worker is not polling.
    """
    if not tls or isinstance(tls, bool):
        return f"{address} (plaintext)"
    parts = ["TLS"]
    if getattr(tls, "server_root_ca_cert", None):
        parts.append("private CA")
    if getattr(tls, "client_cert", None):
        parts.append("client certificate")
    domain: Optional[str] = getattr(tls, "domain", None)
    if domain:
        parts.append(f"SNI {domain}")
    return f"{address} ({', '.join(parts)})"
