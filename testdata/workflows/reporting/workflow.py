"""reporting — the documented example of a workflow that writes a report (ADR 0055).

THE WORKFLOW COMPUTES EVERYTHING; THE REPORT ONLY PRESENTS IT. There is no SDK verb for a report and
no way for a template to ask a question: a workflow puts something in its report by RETURNING it.
That is the whole contract, and it is why this file has no kontra import at all.

    kontra workflow serve reporting            # lints report.md before anything runs
    kontra workflow start reporting --wait
    kontra report preview <run-id>             # render an edited template, store nothing

A TYPED RETURN, because `serve` can check a template's `result.*` paths against one when the type is
derivable. It also documents the report's data for whoever edits `report.md` next.

SMALL VALUES AND REFERENCES, which is the size rule. Big text and bytes go to object storage through
the claim-check codec and the result carries the ref; the renderer resolves it by reading the store
directly, never through an actor — see ADR 0055 §4 for the live run that taught us the difference.
"""
from dataclasses import dataclass, field

from temporalio import workflow


@dataclass
class PriceChange:
    sku: str
    old: str
    new: str


@dataclass
class EnrichResult:
    """What the report is made of. Every field here is read by `report.md` and nothing else is."""

    summary: str
    products: int
    missing_image: int
    isolated: int
    price_changes: list[PriceChange] = field(default_factory=list)
    #: `{"b64": "..."}` for exact bytes, `{"ref": "<sha256>"}` for a claim check, or a plain string.
    #: `{% code "http", result.sample_request %}` renders whichever it is, and redacts it either way.
    sample_request: dict | None = None


@workflow.defn
class Reporting:
    @workflow.run
    async def run(self, catalog: str = "catalog-eu") -> EnrichResult:
        # A fixture: no dispatch, no Fleet, no target. The numbers are constants so the rendered
        # report is byte-identical on every run, which is what lets a test assert on it.
        return EnrichResult(
            summary=(
                f"{12480:,} products enriched from {catalog}; 214 had no matching image and 6 were "
                "isolated after repeated timeouts."
            ),
            products=12480,
            missing_image=214,
            isolated=6,
            price_changes=[
                PriceChange(sku="EU-44871", old="24.90", new="31.50"),
                PriceChange(sku="EU-10233", old="129.00", new="109.00"),
            ],
            # Deliberately carries a credential: the `http` block redacts it before storage, and the
            # original is reachable only through the audited reveal route. An export can never
            # contain it, which is the property worth having a fixture for.
            sample_request={
                "b64": (
                    "UE9TVCAvbG9naW4gSFRUUC8xLjENCkhvc3Q6IGNhdGFsb2ctZXUuZXhhbXBsZQ0KQXV0aG9yaXph"
                    "dGlvbjogQmVhcmVyIGV5SmhiR2NpT2lKSVV6STFOaUo5DQpYLVRyYWNlOglifDNmMQ0KDQp7InVz"
                    "ZXIiOiJzdmMtZW5yaWNoIiwicGFzc3dvcmQiOiJodW50ZXIyIn0="
                )
            },
        )
