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
from referencing.jsonschema import DRAFT202012

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

# 2b. Refused fixtures: examples/invalid/<schema>.<variant>.json must not validate against <schema>.json (the CLI's own
#     checks are held to the same files: internal/publish's tests, D323; the gate's loader to config.*, D321). A rule
#     JSON Schema cannot state is checked outside it: its fixture is listed here with the rule, and must validate, so
#     that it tests that rule alone (the CLI and the control plane refuse it).
BEYOND_SCHEMA = {}  # none since listings lost their MCP tools policy (D350)
for p in sorted((SCHEMAS / "examples" / "invalid").glob("*.json")):
    base = p.name.split(".")[0]
    schema = schemas.get(f"{base}.json")
    if schema is None:
        problems.append(f"{p.relative_to(ROOT)}: no schema for refused fixture")
        continue
    valid = Draft202012Validator(schema, registry=registry).is_valid(json.loads(p.read_text()))
    if p.name in BEYOND_SCHEMA and not valid:
        problems.append(f"{p.relative_to(ROOT)}: the schema refuses it, but it tests a rule outside the schema "
                        f"({BEYOND_SCHEMA[p.name]})")
    elif p.name not in BEYOND_SCHEMA and valid:
        problems.append(f"{p.relative_to(ROOT)}: validates, but is a refused fixture")
for name in sorted(set(BEYOND_SCHEMA) - {p.name for p in (SCHEMAS / "examples" / "invalid").glob("*.json")}):
    problems.append(f"spec/schema/examples/invalid/{name}: listed as a rule outside the schema, but missing")

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

# 6. The A2UI example's fixtures (examples/sellers/a2ui/testdata) against the A2UI v0.9.1 schemas vendored beside them,
#    each file pinned by SHA256SUMS: the agent's form and a renderer's action, a client's capabilities and the card's
#    A2UI params. A file under invalid/ must not validate. The A2UI schemas name the catalog `catalog.json` relative to
#    their $ids: the basic catalog is served under that name too, as A2UI's own test runner does. Formats are asserted,
#    as A2UI's own runner does (ajv-formats): an action's `timestamp` is an RFC 3339 date-time, which jsonschema checks
#    only with rfc3339-validator installed (`make check-specs` installs it; without it, this fails).
import hashlib  # noqa: E402

A2UI = ROOT / "examples" / "sellers" / "a2ui" / "testdata"
VENDORED = A2UI / "a2ui-v0.9.1"
for line in (VENDORED / "SHA256SUMS").read_text().splitlines():
    digest, name = line.split(None, 1)
    f = VENDORED / name.strip()
    if not f.exists() or hashlib.sha256(f.read_bytes()).hexdigest() != digest:
        problems.append(f"{f.relative_to(ROOT)}: not the vendored file SHA256SUMS pins")
a2ui = registry
A2UI_BASE = "https://a2ui.org/specification/v0_9/"
for f in sorted((VENDORED / "json").glob("*.json")) + [VENDORED / "catalogs" / "basic" / "catalog.json"]:
    doc = json.loads(f.read_text())
    res = Resource.from_contents(doc, default_specification=DRAFT202012)
    a2ui = a2ui.with_resource(doc.get("$id", A2UI_BASE + f.name), res)
    if f.name == "catalog.json":
        a2ui = a2ui.with_resource(A2UI_BASE + "catalog.json", res)
A2UI_SCHEMAS = {
    "surface": {"$ref": A2UI_BASE + "server_to_client_list.json"},
    "delete": {"$ref": A2UI_BASE + "server_to_client_list.json"},
    "action": {"$ref": A2UI_BASE + "client_to_server_list.json"},
    "capabilities": {"$ref": A2UI_BASE + "client_capabilities.json"},
    "a2ui-params": {"$ref": A2UI_BASE + "server_capabilities.json#/properties/v0.9"},
}
FORMATS = Draft202012Validator.FORMAT_CHECKER
if not {"date-time", "date", "time"} <= set(FORMATS.checkers):
    problems.append("check_specs.py §6: jsonschema checks no date-time format here: install rfc3339-validator")
for f in sorted(A2UI.glob("messages/*.json")) + sorted(A2UI.glob("card/*.json")) + sorted(A2UI.glob("invalid/*.json")):
    schema = A2UI_SCHEMAS.get(f.name.split(".")[0])
    if schema is None:
        problems.append(f"{f.relative_to(ROOT)}: no schema for fixture")
        continue
    v = Draft202012Validator(schema, registry=a2ui, format_checker=FORMATS)
    errors = list(v.iter_errors(json.loads(f.read_text())))
    if f.parent.name == "invalid" and not errors:
        problems.append(f"{f.relative_to(ROOT)}: validates, but is a refused fixture")
    elif f.parent.name != "invalid":
        problems.extend(f"{f.relative_to(ROOT)}: {err.json_path}: {err.message}" for err in errors)

for line in problems:
    print(line)
sys.exit(1 if problems else 0)
