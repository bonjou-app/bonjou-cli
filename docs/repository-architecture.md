# Bonjou repository architecture

Decision approved on 2026-09-10: the [Bonjou organization](https://github.com/bonjou-app)
owns two public product repositories. The original CLI repository was transferred
with its history and releases; the extracted web application is deployed at
[bonjou.vercel.app](https://bonjou.vercel.app). The organization handle is
`bonjou-app` because `bonjou` was already occupied. The existing Vercel project
is connected to `bonjou-web`; its GitHub app has selected-repository access
limited to that web repository for automatic deployments.

## Ownership and boundaries

| Repository | Visibility | Responsibility |
| --- | --- | --- |
| `bonjou-cli` | Public | Go CLI, Go coordinator, Cloudflare signaling adapter, canonical protocol vectors, CLI releases, deployment templates |
| `bonjou-web` | Public | Marketing website, browser workspace, browser crypto, pinned vector copy, web deployment |

Use the organization's profile for the project overview. No parent product
repository, Git submodule, or combined checkout is required. An organization
owns repositories and manages access; a repository named `bonjou` would simply
be another sibling. See [GitHub's organization documentation](https://docs.github.com/en/organizations/collaborating-with-groups-in-organizations/about-organizations).

The marketing website and browser workspace are one web application. The Go
relay stays with the CLI's Go module even though it deploys independently. It
must never hold client encryption keys, decrypt content, or store payloads.
Separate releases justify this split; repository privacy is not its purpose.

The JavaScript adapter in `packaging/relay/cloudflare/` implements the same
coordinator control messages on Workers Free with one hibernating Durable
Object. It does not change or duplicate application encryption. Public session
keys and network-scoped membership remain routing metadata; chats, profiles,
file metadata, and file payloads stay on direct WebRTC. See the
[approved recovery design](cloudflare-coordinator-design.md) and
[hosting instructions](coordinator-hosting.md).

The existing `hamzaabdulwahab/homebrew-bonjou` and
`hamzaabdulwahab/scoop-bonjou` repositories remain distribution channels.
Preserve release assets and installation links during the transfer. The Go
module and internal imports now target `github.com/bonjou-app/bonjou-cli`, fixing
its previous `hamzawahab`/`hamzaabdulwahab` owner mismatch as part of this ownership
migration. Published version numbers and artifacts are unchanged.

## Protocol coordination

Go generates the canonical
[`internal/network/testdata/protocol-v2.json`](../internal/network/testdata/protocol-v2.json).
The browser repository keeps a copy at `src/share/vectors/protocol-v2.json`,
with the source repository, full commit SHA, path, and SHA-256 checksum recorded
in `protocol-source.json` alongside it.

For protocol changes:

1. Update the relevant Go and browser implementations through linked changes.
2. Regenerate the Go fixture with
   `BONJOU_WRITE_VECTORS=1 go test ./internal/network -run TestProtocolV2Vectors`.
3. Copy that fixture into the web repository and update its provenance metadata.
   Run `npm run check:protocol`, `npm test`, and `npm run build` there. For an
   unpublished Go revision, pass its checkout path to `check:protocol` locally.
4. Update the pinned web revision in this repository's CI. That job tests the
   browser against the candidate Go vectors and smoke-tests network-scoped room
   membership, opaque encrypted signaling, and the absence of payload routes in
   the candidate Go coordinator. The web browser suite exercises approved,
   authenticated payload transfers directly over WebRTC.
5. Require passing checks in both repositories before releasing a coordinated
   change. Never silently skip missing fixtures or use floating compatibility
   references.

Each repository builds independently. Shared cryptographic vectors establish
agreement on those protocol operations; they do not establish CLI LAN-to-browser
transport support. The migration changes fixture ownership without changing the
wire format or any cryptographic test result.

## The reported confidential file

The reported [original fixture](https://github.com/hamzaabdulwahab/bonjou-cli/blob/a6e7e29270fa39c6aaa83118c8917dbc883ba1b1/website/src/share/vectors/protocol-v2.json)
matched the inspected local file byte for byte. Its Alice and Bob private/public
keys and raw ECDH result match the public examples in
[RFC 7748, section 6.1](https://datatracker.ietf.org/doc/html/rfc7748#section-6.1),
published in January 2016. Derived keys and ciphertext use these fixed inputs
and synthetic payloads. These are intentional public test values.

[`vectors_test.go`](../internal/network/vectors_test.go) constructs the fixture.
The browser imports it only in crypto tests; runtime sessions call
`generateKeyPair()` to generate fresh keys. Searches for the two fixed private
keys found them only in the Go test generator and fixture. This file does not
provide evidence of a production credential leak or a reason to make the web
repository private. Its note now identifies the public source explicitly.

Classify values by provenance and use:

| Material | Treatment |
| --- | --- |
| Dependency integrity hashes and release checksums | Public verification data |
| Fixed synthetic crypto test inputs and expected outputs | Public fixtures; never runtime identities |
| Real password hashes, API tokens, private keys, or session keys | Confidential, including when encoded as hashes or hex |
| Real user configuration, messages, files, and logs | Outside Git and release artifacts |

Browser-delivered code and `VITE_*` environment values are public, even when
the source repository is private. Keep service credentials in server-side
secret storage and encryption keys on clients. See
[Vite's environment guidance](https://vite.dev/guide/env-and-mode).
The web package's `"private": true` prevents accidental npm publication; it
does not control GitHub visibility or change the MIT license. See
[npm's package documentation](https://docs.npmjs.com/cli/v11/configuring-npm/package-json/#private).

## Publication safeguards and review limits

The initial review checked GitHub metadata and 77 locally reachable commits
containing 713 text blobs. Selected credential-pattern and sensitive-filename
checks found no matches. GitHub reported secret scanning and push protection
enabled and returned no secret-scanning alerts. This was a limited review,
not proof that all history, credentials, deployments, or artifacts are safe.

Both repositories include secret-file ignore rules with placeholder example
exceptions, CI, dependency update configuration, contribution guidance, and MIT
notices. Secret scanning, push protection, private vulnerability reporting, and
dependency security updates are enabled. Main branches require pull requests
and passing CI, with force pushes and branch deletion blocked. No second
reviewer is required for the current solo-maintainer workflow.

The web lockfile received compatible security patches; its npm audit reports
zero advisories. The Go crypto dependency was updated to v0.52.0 (and its required
x/sys v0.45.0) to address GitHub advisories without changing the Go requirement
or protocol vectors. Windows CI also exposed a Unix-permission assumption and
a sender-cleanup race in existing tests; the tests now use a portable failure
condition and wait for the asynchronous completion they assert.

If real credentials are found, revoke or rotate them before relying on deletion
or history cleanup. Private visibility cannot erase public clones or forks. See
[GitHub's sensitive-data guidance](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/removing-sensitive-data-from-a-repository).

## Migration and rollback

The source baseline is `a6e7e29270fa39c6aaa83118c8917dbc883ba1b1`. A full local
Git bundle preserves it and the original refs. The web repository preserves
the history extracted from `website/`; it includes the existing MIT license
and its own `AGENTS.md`. This repository retains its full original history.

The canonical-vector commit was published before extracting the CLI tree. The
web repository's CI verifies its pinned canonical source, and the CLI's browser
compatibility job verifies a pinned web revision against candidate Go vectors
and relay behavior. Production uses the standalone web source at the root of
the existing Vercel project, retaining the original domain. The old personal
repository URL redirects to the organization, and existing release downloads
remain accessible.

The previous ready Vercel deployment is retained for rollback. Restore it from
the existing project if the new web deployment fails. Revert migration commits
normally if necessary; do not rewrite shared history. The running production
relay is unaffected.

Both repositories carry their own agent guidance. Shared defaults include
relevant skills, proportionate verification, explicit protocol coordination,
secret handling, and completing work within the user's existing authorization.
