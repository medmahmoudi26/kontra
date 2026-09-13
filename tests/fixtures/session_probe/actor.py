"""A test actor that answers ONE question: which process, and which instance, served this call?

Used by tests/test_session_pinning_live.py, which runs two of these against a live cluster. The
pid proves the Session is pinned to a process; `calls` proves both calls met the same LOADED
instance, which is the part `self.*` depends on and the part a re-activation would break
silently — a fresh instance answers 1, 1 rather than 1, 2.
"""

import os

from kontra import actor


@actor.defn
class SessionProbe:
    @actor.load
    async def open(self):
        self.calls = 0

    @actor.method
    async def whoami(self, batch, dataset):
        async for unit in batch:
            self.calls += 1
            await dataset.push({
                "pid": os.getpid(),
                "actor": self.run_id,
                "instance": id(self),
                "calls": self.calls,
                "unit": unit.value,
            })


if __name__ == "__main__":
    actor.serve()
