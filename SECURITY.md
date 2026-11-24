# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**. Do not open a public issue.

Include what you found, how to reproduce it, and the impact you expect. We aim to acknowledge reports within three working days and to agree on a disclosure timeline with you.

## Supported versions

vx is pre-1.0. Security fixes go into the latest release.

## Known limits

vx is early and some protections that organizations expect are not built yet. They are documented, not hidden:

- Every signed-in user can see every repository on a server.
- Agents are not yet restricted to paths.

See [docs/organizations.md](docs/organizations.md). Reports about these known limits are still welcome if you find a way they are worse than described.

## Objects

File contents and trees are stored once per server, but they are served only through a repository the caller can read and that the object is part of. Knowing a hash is not enough to fetch content, and the server does not reveal whether another repository has it. See [docs/architecture.md](docs/architecture.md#objects-are-scoped-per-repository).

## Handling secrets

- vx stores only SHA-256 hashes of tokens.
- `vx agent add --env-file` writes agent tokens to files with mode 600 instead of printing them.
- Run vx behind HTTPS whenever it is reachable by anyone other than you.
