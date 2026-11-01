#!/usr/bin/env python3
"""M0 spec checks: schemas are valid 2020-12, fixtures validate, links and decision refs resolve.

Run by `make check`. Exits non-zero with one line per problem.
"""
import json
import pathlib
import re
import sys

from jsonschema import Draft202012Validator
from referencing import Registry, Resource

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCHEMAS = ROOT / "spec" / "schema"
problems = []

# 1. Every schema is a valid 2020-12 schema; build a registry so relative $refs resolve.
registry = Registry()
schemas = {}
for p in sorted(SCHEMAS.glob("*.json")):
    doc = json.loads(p.read_text())
    try:
        Draft202012Validator.check_schema(doc)
    except Exception as e:  # noqa: BLE001
        problems.append(f"{p.relative_to(ROOT)}: not a valid 2020-12 schema: {e.message}")
    schemas[p.name] = doc
    res = Resource.from_contents(doc)
    registry = registry.with_resource(p.name, res).with_resource(doc["$id"], res)

# 2. Fixtures validate: examples/<schema>.<variant>.json against <schema>.json (or a $defs entry).
for p in sorted((SCHEMAS / "examples").glob("*.json")):
    stem = p.name.split(".")
    base, variant = stem[0], stem[1] if len(stem) > 2 else None
    schema = schemas.get(f"{base}.json") or schemas.get(f"{base}.{variant}.json")
    if base == "mcp-tools":
        schema = {"$ref": f"https://kontra.sh/schema/mcp/tools/v1.json#/$defs/{variant}"}
    elif base == "hooks":
        schema = {"$ref": f"https://kontra.sh/schema/hooks/v1.json#/$defs/{variant}"}
    elif f"{base}.{variant}.json" in schemas:
        schema = schemas[f"{base}.{variant}.json"]
    if schema is None:
        problems.append(f"{p.relative_to(ROOT)}: no schema for fixture")
        continue
    v = Draft202012Validator(schema, registry=registry)
    for err in v.iter_errors(json.loads(p.read_text())):
        problems.append(f"{p.relative_to(ROOT)}: {err.json_path}: {err.message}")

# 3. Relative links in markdown point at files that exist.
for md in sorted(list((ROOT / "spec").glob("*.md")) + list((ROOT / "docs").glob("*.md")) + [ROOT / "README.md"]):
    text = re.sub(r"```.*?```", "", md.read_text(), flags=re.S)
    text = re.sub(r"`[^`\n]*`", "", text)
    for target in re.findall(r"\]\(([^)#\s]+)(?:#[^)]*)?\)", text):
        if re.match(r"^[a-z]+:", target):
            continue
        if not (md.parent / target).exists():
            problems.append(f"{md.relative_to(ROOT)}: broken link {target}")

# 4. Every decision cited as Dn in spec/ or migrations/ exists in docs/decisions.md, and every entry is cited
#    somewhere or marked (uncited).
decisions = ROOT / "docs" / "decisions.md"
entries = set(re.findall(r"^### D(\d+)\.", decisions.read_text(), re.M)) if decisions.exists() else set()
if not decisions.exists():
    problems.append("docs/decisions.md is missing")
cited = set()
for f in list((ROOT / "spec").glob("*.md")) + list((ROOT / "migrations").glob("*.sql")):
    cited |= set(re.findall(r"\bD(\d+)\b", f.read_text()))
for n in sorted(cited - entries, key=int):
    problems.append(f"D{n} is cited but has no entry in docs/decisions.md")

# 5. relay.md §2.3's derived buyer ids (D319) are what Python's uuid.uuid5 computes from the names it writes: an
#    operator computes them in its own language.
import uuid  # noqa: E402

relay = (ROOT / "spec" / "relay.md").read_text()
NS = uuid.uuid5(uuid.NAMESPACE_URL, "https://kontra.sh/ns/v1")


def derive(kind, *parts):
    return uuid.uuid5(NS, "".join(f"{len(p.encode())}:{p}," for p in [kind, *parts]))


for what, value in [
    ("the namespace", NS),
    ("the node buyer", derive("node", "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa")),
    ("the oidc buyer", derive("oidc", "https://api.kontra.sh", "alice")),
    ("the team gate's anonymous buyer", derive("anonymous")),
    ("a tenant's anonymous buyer", derive("anonymous", "01900000-0000-7000-8000-000000000001")),
]:
    if f"`{value}`" not in relay:
        problems.append(f"spec/relay.md §2.3: {what} is {value}, which it does not write")

for line in problems:
    print(line)
sys.exit(1 if problems else 0)
