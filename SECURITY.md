# Security

## Reporting a vulnerability

Report privately, not in a public issue.

Use GitHub's [private vulnerability
reporting](https://github.com/mach6/yore/security/advisories/new) on this
repository, or email <doug@mach6.net>.

Say what you found, how to reproduce it, and what an attacker gets out of it.
Expect an acknowledgement within a week or so. If a fix is needed, it gets
worked out with you before anything is announced, and you are credited in the
release notes unless you would rather not be.

## What is in scope

The threat model is written down in [`docs/protocol.md`](docs/protocol.md) and
[`docs/architecture.md`](docs/architecture.md). The reports that matter most:

- Anything that lets the sync server, or someone who has taken it over, read
  history in plaintext or learn what a command was.
- Anything that lets one device read or write another device's history without
  having been enrolled and approved.
- Forging, replaying, or tampering with a record so that it decrypts as
  something other than what was recorded.
- A revoked device that can still read history synced after its revocation.
- Any path that writes plaintext history, a device key, or a token somewhere it
  was not meant to go, including into the plaintext shell history of the
  machine itself.

## What is not in scope

- An attacker who already holds your device key or root on your machine. yore
  protects history in transit and at rest on the server; it cannot protect a
  machine that is already compromised, and the README says so.
- The secret filter missing a credential shape it has no rule for. It is a
  pattern-based safety net, not a guarantee. A new rule is an ordinary pull
  request, not a vulnerability report.
- The absence of an independent audit. yore has not had one.

## Supported versions

yore is before 1.0. Fixes land on the latest release only.
