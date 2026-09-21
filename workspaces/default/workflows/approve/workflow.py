"""approve — the smallest workflow that asks a human something.

It dispatches nothing, provisions nothing and writes nothing. Like `ping`, its whole job is to
exercise one piece of wiring — here the human-in-the-loop turn: park on a question, render it as a
form in the transcript, and carry the answer back as a turn.

    kontra workflow serve .kontra/workflows/approve --tmux
    kontra workflow start .kontra/workflows/approve --input '{"n": 12}'

The run then sits PARKED until somebody answers it in the console, which is the point: a stalled
run and a run waiting on a person are different facts, and only one of them is anybody's
fault.

IT IS ALSO THE SMALLEST PLACE TO SEE THE DIFFERENCE. `note` and `ask` are the two things a workflow
says out loud, and this file uses both in the order an author reaches for them: say where the run is,
ask the question, say what the answer was.

THEY ARE NOT A PAIR ANY MORE, AND ADR 0050 §2 IS WHY. `note` is a LOG LINE — it costs no history, is
not capped, and reaches the log store where it can be queried across runs. `ask` costs history AND
STOPS THE RUN until a person moves it, which is the one thing a log line can never do and the reason
it survived the removal of `speak`.
"""
from dataclasses import dataclass
from datetime import timedelta

from temporalio import workflow

from actorkit import ask, catalog, note

@dataclass
class ApprovalRequest:
    """What the operator hands to the workflow. `n` is the number of hosts to approve."""
    test_input: str
    proceed: bool


@dataclass
class Approval:
    """What the operator hands back. `note` is optional; `approved` is not."""

    approved: bool
    note: str = ""


@workflow.defn
class Approve:
    @workflow.run
    async def run(self, req: ApprovalRequest) -> dict:
        answer = await ask(
            f"Approve this input: {req.test_input}?",
            takes=Approval,
            context={"test_input": req.test_input},
            deadline=timedelta(hours=4),
        )

        approved = bool(getattr(answer, "approved", False))
        note(f"{'approved' if approved else 'declined'}; {req.test_input}")

        return {
            "approved": approved,
            "note": str(getattr(answer, "note", "")),
            "test_input": req.test_input,
        }


if __name__ == "__main__":
    catalog.serve([Approve])
