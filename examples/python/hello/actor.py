"""hello — the seeded first actor. One Method, one Dataset row."""

from pydantic import BaseModel

from kontra import actor


class Target(BaseModel):
    """Nothing required. The starter workflow sends an empty unit."""


class Greeting(BaseModel):
    message: str


@actor.defn
class Hello:
    @actor.method(takes=Target, emits=Greeting)
    async def greet(self, batch, dataset) -> None:
        async for _unit in batch:
            await dataset.push(Greeting(message="hello world"))


if __name__ == "__main__":
    actor.serve()
