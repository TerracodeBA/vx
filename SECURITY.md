# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**. Do not open a public issue.

Include what you found, how to reproduce it, and the impact you expect. We aim to acknowledge reports within three working days and to agree on a disclosure timeline with you.

## Supported versions

vx is pre-1.0. Security fixes go into the latest release.

## Known limits

vx is early and some protections that organizations expect are not built yet. They are documented, not hidden:

- Every signed-in user can see every repository on a server.
- File contents are shared by hash across repositories on a server.
- Agents are not yet restricted to paths.

See [docs/organizations.md](docs/organizations.md). Reports about these known limits are still welcome if you find a way they are worse than described.

## Handling secrets

- vx stores only SHA-256 hashes of tokens.
- `vx agent add --env-file` writes agent tokens to files with mode 600 instead of printing them.
- Run vx behind HTTPS whenever it is reachable by anyone other than you.
