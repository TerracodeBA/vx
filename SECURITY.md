# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**. Do not open a public issue.

Include what you found, how to reproduce it, and the impact you expect. We aim to acknowledge reports within three working days and to agree on a disclosure timeline with you.

## Supported versions

vx is pre-1.0. Security fixes go into the latest release.

## Known limits

vx is early and some protections that organizations expect are not built yet. They are documented, not hidden:

- Repository content is not encrypted per organization: the server's operators can read it with direct access to the database and storage (see Superadmins below).

See [docs/organizations.md](docs/organizations.md). Reports about these known limits are still welcome if you find a way they are worse than described.

## Superadmins

The people who run a vx server (superadmins: the operator on vx Cloud) manage people, invites, sign-up, the GitHub App and server settings, and see the operation log's metadata. They have no role on any repository from that: a customer's repository, its files and objects, changes, diffs, board notes, claims, tasks, overlaps and reviews are "not found" to them in the CLI, the API, the web UI and MCP, like to anyone without a role. They reach a repository only through a role its admins grant: an ordinary grant, or support access, which is read or write, ends by itself after at most 30 days, shows on the repository's access page, is recorded in the operation log, and can be revoked at any time. See [docs/organizations.md](docs/organizations.md#superadmins-and-support-access).

This is enforced by vx, not by the infrastructure: whoever has the database, the object storage or the server's disk (and `VX_SECRET_KEY`) can read everything stored there. A report that a superadmin can reach repository content through vx without a grant is a vulnerability.

## Objects

File contents and trees are stored once per server, but they are served only through a repository the caller can read and that the object is part of. Knowing a hash is not enough to fetch content, and the server does not reveal whether another repository has it. See [docs/architecture.md](docs/architecture.md#objects-are-scoped-per-repository).

## Handling secrets

- vx stores only SHA-256 hashes of tokens.
- `vx agent add --env-file` writes agent tokens to files with mode 600 instead of printing them.
- Run vx behind HTTPS whenever it is reachable by anyone other than you.
