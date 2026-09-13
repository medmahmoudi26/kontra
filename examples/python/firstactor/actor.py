"""firstactor — the one to copy. Every control the form can draw, on one Method.

WHAT IT DOES. Takes a host and expands it into candidate subdomain labels, one Dataset row per
candidate. No network, no dependency beyond the SDK: it is a template, and a template that needs
an API key is a template nobody runs on the day they install.

WHY IT LOOKS LIKE THIS. `testdata/fixtureactor/` is deliberately boring because a fixture must not
teach — it exists so the parity gate has something to dispatch to. This one is the opposite: every
field on `Target` is here because it makes the console and the VS Code pane draw a DIFFERENT
control, and seeing all five at once is the fastest way to learn what a declared type buys you.

    kontra serve --actor examples/python/firstactor --watch

── THE FORM IS DERIVED, NEVER AUTHORED ─────────────────────────────────────────────────────────

Nothing about the UI is configured anywhere. The console reads the JSON Schema pydantic derives
from `Target` and picks a control per field:

    host: str                     a box
    mode: Literal["quick",…]      a DROPDOWN — the wrong value is not on the screen to pick
    follow_redirects: bool        a TOGGLE, with `not set` distinct from `false`
    wordlist: File | None         a DROP ZONE; the bytes go to the orchestrator, the field
                                  carries {name, sha256, size}
    corpus: Folder | None         the same, for a whole directory

The two blob types are the only ones that need a marker — `x-kontra-input` — because `File` is a
model with three string properties and nothing else would stop the console asking somebody to type
a SHA-256 by hand. Everything else is read off the schema JSON Schema already has a word for.

── AND THE FILE NEVER TRAVELS AS AN ARGUMENT ───────────────────────────────────────────────────

A Method's input is a workflow argument, and a workflow argument is kept in workflow history for
the namespace's whole retention and replayed on every worker that picks the run up. A 40 MB
wordlist there is either refused outright or offloaded by the claim-check codec on the way past.
So the offload happens ONCE, before the run starts: the console PUTs the bytes, the orchestrator
content-addresses them, and a hundred bytes of ref ride in the argument. `await
unit.value.wordlist.read()` below is what turns the ref back into bytes, integrity-checked against
the sha it names.
"""

from typing import Literal, Optional

from pydantic import BaseModel

from kontra import File, Folder, actor


class Target(BaseModel):
    """One Unit of work — and one row of the form.

    EVERY FIELD IS A DIFFERENT CONTROL. That is the point of this class; see the module docstring.
    The optional ones are optional so the form is usable with nothing dropped on it, which is how
    somebody tries this for the first time.
    """

    #: The apex to expand. A plain box.
    host: str

    #: A CLOSED SET, so the console draws a dropdown and a typo is not expressible. `Literal` and
    #: not `str` is the whole difference between a value that is checked at the form and one that
    #: fails inside your Method with an argument you have to go and read the history to see.
    mode: Literal["quick", "deep"] = "quick"

    #: A TOGGLE. `bool` typed into a box is a value that can be misspelled — and was, which is why
    #: the console grew a three-state control where "not set" is distinct from "false".
    follow_redirects: bool = False

    #: A DROP ZONE. Optional, so the form is runnable empty; when something IS dropped, this is a
    #: ref and `.read()` fetches the bytes.
    wordlist: Optional[File] = None

    #: A DROP ZONE FOR A DIRECTORY. `Folder.files` is a list of `File`, each carrying the `path` it
    #: had inside the directory — which is the one thing a per-file drop cannot tell you.
    corpus: Optional[Folder] = None


class Candidate(BaseModel):
    """One row out. What a Dataset query will see, so the names are the column names."""

    host: str
    candidate: str
    #: Where the label came from — `builtin`, the wordlist's filename, or a path inside the folder.
    source: str
    mode: str


#: The labels used when nothing is dropped on the form. Small on purpose: a template that emits ten
#: thousand rows on its first run teaches you about your terminal, not about kontra.
BUILTIN = ("www", "api", "dev", "staging", "admin")

#: What `mode: "deep"` adds. Two modes that differed by a number nobody could see would make the
#: dropdown decorative — the value has to change what comes out or it is not worth collecting.
DEEP = ("internal", "vpn", "git", "jenkins", "grafana", "s3", "cdn", "mail")


@actor.defn
class FirstActor:
    @actor.load
    async def open(self) -> None:
        """Run ONCE per worker, before the first Unit.

        This is where an expensive thing belongs — an HTTP session, a compiled regex, a model. It
        is NOT per Unit and NOT per Batch, which is the distinction that makes an actor cheap to
        fan out: pay for the setup on a Machine once and dispatch to it a thousand times.
        """
        self.builtin = list(BUILTIN)

    @actor.method(takes=Target, emits=Candidate)
    async def expand(self, batch, dataset) -> None:
        """One row per candidate label, per Unit.

        THE LOOP IS YOURS (ADR 0028 §2). kontra hands you a Batch and you decide what a Unit costs
        — sequential here, but nothing stops you gathering them. What kontra owns is that the Batch
        arrives, that `dataset.push` is durable, and that a Unit that raised is reported as one.
        """
        async for unit in batch:
            target = unit.value
            labels = [(w, "builtin") for w in self.builtin]
            if target.mode == "deep":
                labels += [(w, "builtin") for w in DEEP]

            # THE DROPPED FILE, READ HERE AND NOT BEFORE. `.read()` re-hashes what it fetches
            # against the sha in the ref, so a blob replaced under the same address is a loud
            # failure rather than a Method quietly running on the wrong bytes.
            if target.wordlist is not None:
                for line in (await target.wordlist.read()).decode("utf-8", "replace").splitlines():
                    if line.strip():
                        labels.append((line.strip(), target.wordlist.name))

            # AND THE DROPPED DIRECTORY. `f.path` is the name it had inside the folder, which is
            # what lets a row say which file in a corpus it came from.
            if target.corpus is not None:
                for f in target.corpus.files:
                    for line in (await f.read()).decode("utf-8", "replace").splitlines():
                        if line.strip():
                            labels.append((line.strip(), f.path or f.name))

            seen = set()
            for label, source in labels:
                candidate = f"{label}.{target.host}"
                # DEDUPED HERE, because a wordlist and the builtins overlap and a Dataset with the
                # same row twice is a count nobody can trust.
                if candidate in seen:
                    continue
                seen.add(candidate)
                await dataset.push(
                    Candidate(
                        host=target.host,
                        candidate=candidate,
                        source=source,
                        mode=target.mode,
                    )
                )

    @actor.close
    async def close(self) -> None:
        """Run once on the way out. Release what `@actor.load` took."""
        self.builtin = []


if __name__ == "__main__":
    # `kontra serve` runs this file. Nothing here knows about Temporal, a queue or a Machine —
    # that is the runtime's, and it is why the same file runs locally and on a Fleet unchanged.
    actor.serve()
