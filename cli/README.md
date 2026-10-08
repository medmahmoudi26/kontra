# kontra CLI

Build: `cd cli && go mod tidy && go build -o kontra .` — test with `go test ./...`.

    kontra doctor [--api url]                               # infra state: services, web consoles, actors
    kontra infra up|down|status [--repo <dir>]              # control-plane compose + health table
    kontra deploy --actor <dir> [--engine py|go] [--registry host:port] [--controller host] [--host-only] [--override]
                                                            # build+push a self-contained worker image; prints pull/run
                                                            # (refuses an existing version unless --override; built by `pack` onto a pinned runtime)
    kontra workers list                                     # catalog x live Temporal pollers
    kontra actor register <dir> [--init] [--json]           # declare an actor in the catalog
    kontra workflow serve <folder>                          # run YOUR workflows here
    kontra workflow start <folder> [--input <json|@file>] [--wait]
                                                            # DISPATCH: only a caller's workflow can.
                                                            # `kontra actor … dispatch` and `kontra graph …
                                                            # dispatch` are gone — they POSTed a graph to
                                                            # /api/runs, which the orchestrator stopped
                                                            # accepting with ADR 0023 §12.

Env: `KONTRA_ORCHESTRATOR_URL` (http://localhost:8088), `KONTRA_ADDRESS` (localhost:7233), `KONTRA_NAMESPACE` (default).
