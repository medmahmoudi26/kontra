# nscheck

Hunts lame delegations across a dataset of domains, on a fleet it provisions and destroys itself.

A lame delegation is a nameserver in a zone's own NS set that does not answer for that zone: a
resolution fault at best, and at worst a takeover, when the delegation points at a provider where
an unclaimed zone can simply be registered.

## What it does

It is a whole run, infrastructure included, in one durable program. `fleet.hold(tag="dns", …)`
takes a **Lease** on **four Droplets** (`machines`, default 4), `f.place("nscheck", "0.1.0", …)`
says what runs on them, and `f.ready()` waits for Workers to actually poll — not merely for the
converge to return, which happens while systemd is still starting. The Fleet is named after the
TAG, not the Actor: it is capacity, several Runs may hold it at once, and its Machines go when the
last Lease drops rather than when this scope exits. Then it pages the input dataset and drives two
Methods on that one Actor:

    delegation   domain            → the nameservers the zone delegates to
    ask          (domain, ns)      → a verdict for that pair

`delegation` fans out, so a 100-domain page comes back as ~400 pairs and is re-paged before `ask`
sees it. Every pair gets a row, including the failures — a refusing server, an unresolvable one, a
domain that delegates nowhere — because a broken nameserver is the finding, not a dropped Unit.

This is the run the monitoring plane was built from. Four Machines, a fleet that appears and
disappears inside one Run, and a dataset filling while it happens is what the Monitor, the run
rails and the fleet window were all read against.

## What it takes

    {"dataset": "domains", "into": "lame", "machines": 4, "sessions": 8, "size": 100,
     "run": "dns"}

Every key is optional. `machines: 0` means zero Machines and is honoured as such — the workflow
reads its numbers so that absent and zero are different answers.

## What it leaves behind

**The `lame` dataset** (`into`, default `lame`) — one row per (domain, nameserver) pair, with
`domain`, `ns`, `verdict`, `detail` and `ok`. It is published streaming, so it is queryable while
the run is still running and long after the fleet is gone:

    kontra db query "SELECT verdict, count(*) FROM lame WHERE NOT ok GROUP BY 1 ORDER BY 2 DESC"
    kontra db query "SELECT domain, ns, verdict, detail FROM lame WHERE NOT ok LIMIT 20"

**No Machines.** The fleet's lifetime is the run's: the scope exit destroys it, and because
this is a workflow rather than a script, Temporal runs that teardown whether or not the process
that started the run still exists. Cancelling a run therefore leaves nothing behind; *terminating*
one skips the scope exits, and four Droplets keep billing with nothing tracking them.

**A result** naming what it produced: `into`, `machines`, the immutable `bundle` sha the mutable
`latest` pointer resolved to, `pairs`, `checked`, and `dropped` — the count of inputs isolation
threw away, reported so a run that dropped everything cannot read like a run that found nothing.

## Run it

    kontra build --actor examples/go/nscheck        # publish the Artifact ONCE
    kontra workflow serve nscheck --tmux
    kontra workflow start nscheck --wait \
        --input '{"dataset": "domains", "into": "lame", "machines": 4}'
