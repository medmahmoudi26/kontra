#!/usr/bin/env python3
"""Fail unless every published HostIp is 127.0.0.1. Vacuous (no bindings) is a failure."""
import json
import subprocess
import sys

ids = subprocess.check_output(["docker", "compose", "ps", "-q"], text=True).split()
hosts = set()
for cid in ids:
    raw = subprocess.check_output(
        ["docker", "inspect", "-f", "{{json .NetworkSettings.Ports}}", cid], text=True
    )
    ports = json.loads(raw)
    if not ports:
        continue
    for bindings in ports.values():
        if not bindings:
            continue
        for b in bindings:
            ip = b.get("HostIp") or ""
            if ip:
                hosts.add(ip)
if not hosts:
    sys.exit("no port bindings found — inspect shape changed; this check was vacuous")
bad = [h for h in sorted(hosts) if h != "127.0.0.1"]
if bad:
    sys.exit("a port is published beyond loopback: " + ",".join(bad))
print("all bindings on 127.0.0.1", sorted(hosts))
