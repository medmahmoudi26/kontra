# Security Policy

kontra runs code it did not write, on machines it may not own. Please treat findings accordingly.

## Reporting a vulnerability

**Do not open a public issue.** Report privately through GitHub's
[private vulnerability reporting](../../security/advisories/new), or email the maintainer.

Please include what you did, what you observed, and what you expected. A proof of concept helps
enormously; a scenario written out in prose is fine if a PoC is impractical.

You will get an acknowledgement within a few days. kontra is `0.x` and maintained by a small team,
so please allow reasonable time before disclosing.

## Scope

In scope: the control plane, the CLI, the Warden, the SDKs and runtimes, the Pulumi programs, and
the console.

Out of scope: findings that reduce to a boundary the
[Security Model](../../wiki/Security-Model) already names as absent. That page states what does
and does not isolate today — most importantly, that **the data plane has no tenant boundary** and
one control plane should serve one tenant. A report that a co-tenant can read another's Datasets
is documented, not novel; a report of a way past a boundary we claim to hold is very much wanted.

## Supported versions

Only the latest `0.x` minor. There are no backports while the project is pre-1.0.
