# Security policy

## Supported versions

CloudRig is pre-1.0. Security fixes go into the latest release only.

| Version | Supported |
|---|---|
| Latest `v0.x` release | Yes |
| Older releases | No |

## Reporting a vulnerability

**Don't open a public issue for a security problem.**

Report it privately through GitHub:
[Report a vulnerability](https://github.com/monirz/cloudrig/security/advisories/new).
The report is visible only to you and the maintainers.

Include:

- the CloudRig version, which is the first line `cloudrig start` prints, or the commit
- what an attacker can do, and under what conditions
- the steps to reproduce it, or a proof of concept

## What happens next

- You get an acknowledgement within 7 days.
- We confirm the issue, agree a fix and a disclosure date with you, and keep
  you updated until the fix ships.
- The fix is released with a GitHub security advisory that credits you,
  unless you ask not to be named.

## Scope

CloudRig is a local emulator for development and testing. It has no
authentication, and it doesn't enforce Google Cloud IAM. `cloudrig start`
listens on every network interface, so run it only on a network you trust.

In scope:

- escape from the containers or subprocesses that CloudRig runs, such as Cloud
  Functions, Cloud Run or GKE workloads
- file access outside CloudRig's data directory
- vulnerabilities in the released binaries or their dependencies

Out of scope: the lack of authentication or authorization itself.
