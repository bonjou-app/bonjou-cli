# Security

Report suspected vulnerabilities through
[GitHub's private vulnerability reporting](https://github.com/kodolabs-hq/bonjou-cli/security/advisories/new).
Include affected revisions, reproduction steps using synthetic data, and impact.
Do not include real credentials, identity keys, or user payloads in public issues.

Read [the security model](docs/security-model.md) for the current guarantees
and limitations. The relay must forward opaque payloads without holding client
encryption keys or storing file contents.

`internal/network/testdata/protocol-v2.json` contains public test inputs and
outputs, including the X25519 examples from RFC 7748 section 6.1. These values
are intentional fixtures, not runtime credentials. Real identity secrets live
in user configuration and must remain confidential.
