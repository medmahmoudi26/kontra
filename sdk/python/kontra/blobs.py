"""Take a file — or a folder of them — as a Method's declared input.

    from kontra import actor, File

    @actor.method(takes=File, emits=Finding)
    async def scan(self, batch, dataset):
        async for unit in batch:                       # the loop is yours (ADR 0028 §2)
            for line in (await unit.value.read()).decode().splitlines():
                await dataset.push(Finding(host=line))

An operator drops the file on the form; the console uploads it, the field carries the REF, and the
actor reads the bytes here. Nothing large ever travels as a workflow argument.

── WHY A REF AND NOT THE BYTES ────────────────────────────────────────────────────────────────

A Method's input is a workflow argument, and a workflow argument goes into workflow history — where
it is kept for the namespace's whole retention and replayed on every worker that picks the run up.
A 40 MB corpus inlined there is 40 MB Temporal will refuse outright, or that the claim-check codec
(ADR 0007) offloads on the way past anyway. So the offload happens ONCE, deliberately, before the
run starts: the console PUTs the bytes to the orchestrator, which content-addresses them into the
same store the codec uses, and what rides in the argument is a hundred bytes of `{name, sha256,
size}`.

That is not a new plane. `cas/<sha[:2]>/<sha>` is where the codec already writes an offloaded
payload and exactly where every language's blob fetch already looks — which is why reading one needs
no new configuration on either side.

── THE FETCH LIVES IN THE RUNTIME, NOT HERE ───────────────────────────────────────────────────

This module declares the TYPES and holds a reader that is `None` until a worker installs one
(`set_blob_reader`, called by `internals/temporal/host.py`). The arrow is runtime → sdk and never
the reverse: an author surface that linked an S3 client could not be imported by a caller who has
neither, and `import kontra` is required to cost nothing. `tests/test_sdk_arrow.py` enforces both
halves — statically, and by importing every module here with `internals` made unimportable — and it
is what caught `File.read()` reaching for `internals.casstore` directly.

── CONTENT-ADDRESSED, WHICH IS WHAT MAKES A RE-RUN EXACT ──────────────────────────────────────

The sha IS the address. Re-uploading the same corpus writes nothing and answers the same ref; a run
repeated a week later names the same sha and gets the same bytes, or a loud miss — never a file
somebody replaced under the same name. `name` is carried for YOU to read (log it, key an output on
it) and is never used to find anything.

── WHAT THE CONSOLE DOES WITH THE DECLARATION ─────────────────────────────────────────────────

`json_schema_extra={"x-kontra-input": …}` is the whole contract with the UI. pydantic copies unknown
keywords straight through into the derived schema, JSON Schema says unknown keywords are ignored, so
the marker reaches the catalog untouched and every validator in the chain steps over it. The console
reads it and draws a drop zone instead of walking the model's three properties and asking somebody
to type a SHA-256 by hand.

An SDK that has not grown these types yet simply never sets the marker, and its fields stay boxes.
"""

from __future__ import annotations

from typing import Any, Awaitable, Callable, Optional

from pydantic import BaseModel, Field

#: How this process fetches a blob by its sha — installed by the RUNTIME, never by this module.
#:
#: THE ARROW IS RUNTIME → SDK AND NEVER THE REVERSE, and `tests/test_sdk_arrow.py` enforces it both
#: statically and by importing every module here with `internals` made unimportable. An author
#: surface that linked an S3 client could not be imported by a caller who has neither, and
#: `import kontra` is required to cost nothing — which is also why `File`/`Folder` are resolved
#: lazily from `kontra/__init__.py`: `blobs` imports pydantic.
#:
#: `None` is the honest state almost everywhere. A caller workflow, the CLI and a plain script all
#: have no reader, and :meth:`File.read` says so by name rather than failing on a missing
#: environment variable three frames down.
_reader: Optional[Callable[[str], Awaitable[bytes]]] = None


def set_blob_reader(fn: Optional[Callable[[str], Awaitable[bytes]]]) -> None:
    """Install the fetch a served actor uses — called once by the worker host at start.

    It takes a hex sha256 and returns the bytes. It does NOT need to verify them:
    :meth:`File.read` re-hashes what it gets, because integrity is a property of the type rather
    than of whoever happened to fetch.
    """
    global _reader
    _reader = fn


class File(BaseModel):
    """One uploaded file: what it was called, where its bytes are, and how many.

    Declare it with ``takes=File`` — or as a field on your own input model, which is the usual
    case, because a Method that takes a file almost always takes something else with it::

        class Scan(BaseModel):
            corpus: File
            strict: bool = False
    """

    model_config = {"json_schema_extra": {"x-kontra-input": "file"}}

    #: The name on the operator's filesystem. FOR YOU TO READ, never to address anything — it is
    #: not unique, it is not ours to trust, and the orchestrator has already reduced it to a single
    #: path segment so joining it onto a directory cannot escape one.
    name: str
    #: Lower-hex sha256 of the bytes. THIS is the address.
    sha256: str
    size: int
    #: What the browser said it was. A hint, never a guarantee — `text/csv` from a drag is the OS
    #: guessing from an extension.
    content_type: str = Field(default="", alias="contentType")
    #: The path inside the dropped directory, for a file that came from a :class:`Folder`. `None`
    #: for a file chosen on its own, which has no directory to be relative to.
    path: Optional[str] = None

    async def read(self) -> bytes:
        """The bytes, integrity-checked against :attr:`sha256`.

        THE RUNTIME SUPPLIES THE FETCH; THIS MODULE ONLY DECLARES THE TYPE. That inversion is not
        style — `tests/test_sdk_arrow.py` enforces it, and it caught this method importing
        `internals.casstore` directly. **The arrow is runtime → sdk and never the reverse**: an
        author surface that links an S3 client cannot be imported by a caller who has neither, and
        `import kontra` is required to cost nothing. A deferred import would still be an edge; the
        suite tolerates exactly two, and both are entry-point handoffs where an author has already
        given the process away.

        So the worker REGISTERS a reader at start (`internals/temporal/host.py` calls
        :func:`set_blob_reader`), and this calls whatever is there. Same store, same
        `cas/<sha[:2]>/<sha>` derivation, same prefix — because it is literally the runtime's own
        `casstore`, reached from the side that is allowed to have it.

        THE HASH IS RE-CHECKED HERE rather than in the reader, because it is a property of this
        type and not of whoever fetched. A content address whose object was replaced is the one
        failure that would otherwise be silent: the Method runs happily on the wrong bytes and
        writes a wrong Dataset.

        NOT CACHED. A Method that reads the same file for every Unit in a Batch should read it once
        in `@actor.load` and keep it; this cannot know whether you want that, and a cache on the
        model would hold a large object for the life of the worker.
        """
        import hashlib

        if _reader is None:
            raise RuntimeError(
                f"cannot read {self.name}: no blob reader is registered in this process. "
                "A File's bytes are fetched by the worker runtime — this is an actor Method's "
                "to call, inside a served actor, not a caller's or a script's."
            )
        data = await _reader(self.sha256)
        got = hashlib.sha256(data).hexdigest()
        if got != self.sha256:
            raise RuntimeError(
                f"{self.name}: integrity check failed — the object at {self.sha256[:12]} "
                f"hashes to {got[:12]}"
            )
        return data


class Folder(BaseModel):
    """A directory of uploaded files, in the order the browser walked it.

    The operator drops a folder; every file under it is uploaded and listed here with its
    :attr:`File.path` relative to the directory, so structure survives::

        @actor.method(takes=Folder)
        async def ingest(self, unit):
            for f in unit.value.files:
                unit.emit(Doc(path=f.path, body=(await f.read()).decode()))

    A FOLDER IS N FILES AND NOT AN ARCHIVE. That is what a browser can give and what a store can
    content-address; inventing a tar on the way in would mean unpacking one on the way out, inside
    somebody's Method, for no gain — and would lose per-file dedup, which is most of the value when
    a corpus is re-uploaded with three files changed.
    """

    model_config = {"json_schema_extra": {"x-kontra-input": "folder"}}

    #: The directory's own name, as the operator had it.
    name: str
    files: list[File] = Field(default_factory=list)

    def __len__(self) -> int:
        return len(self.files)

    def __iter__(self) -> Any:  # type: ignore[override]
        return iter(self.files)
