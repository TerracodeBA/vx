# Architecture

vx is a server-centric version control layer in front of git. The server does the heavy work (merging, rebasing, history queries, writing git commits); clients keep a working copy and a local object cache.

```mermaid
flowchart LR
  subgraph clients[Clients]
    cli[vx CLI<br/>working copy, object cache]
    agents[Agents<br/>Claude Code + vx CLI]
    ui[Web UI]
  end
  subgraph server[vx-server]
    api[connect-go API]
    svc[Service<br/>land, sync, ship]
    engine[Merge engine]
    bridge[Git bridge]
  end
  pg[(Postgres<br/>metadata)]
  blobs[(Blob store<br/>trees and files)]
  git[(Git host<br/>GitHub, any remote)]
  cli --> api
  agents --> api
  ui --> svc
  api --> svc --> engine
  svc --> pg
  engine --> blobs
  bridge --> blobs
  bridge <--> git
  svc --> bridge
```

## Data model

| Object | What it is | Where |
|---|---|---|
| Blob | File contents, addressed by BLAKE3 (domain-separated by kind), zstd-compressed | Blob store |
| Tree | One directory: sorted entries (name, mode, hash) | Blob store |
| Changeset | One change: parents, tree, intent, author (agent, person, model, session), evidence, stable change ID; hashed over a canonical encoding | Postgres |
| Effort | A shared line of work: base branch, base and head changesets, version for compare-and-swap | Postgres |
| Rebase | One replay of an effort onto a newer branch head: state, progress, conflicts | Postgres |
| Workspace | One task inside an effort: intent, change ID, base | Postgres |
| Branch | An upstream branch vx follows, with its imported head | Postgres |
| Shipment | A pushed `vx/` branch and its pull request; a split effort has one per part | Postgres |

Changesets list their tree, so two snapshots are compared by walking both trees and skipping every subtree whose hash is equal. The cost of a diff, a merge or an upload is proportional to the size of the change, not of the repository.

## Objects are scoped per repository

Blobs and trees are stored once, deduplicated across the whole server. Which repository an object belongs to is recorded separately in Postgres (`repo_objects`: repository, hash). Every object request names its repository, and:

- `GetObjects` needs read access to the repository and serves only objects that are part of it. Any other hash is reported as not found, with the same message as a hash nobody ever stored, so the store cannot be probed across repositories.
- `MissingObjects` answers for the repository: an object stored only for another repository counts as missing, so the client uploads it.
- `PutObjects` needs write access. The server hashes the bytes it receives and records those hashes for the repository, so an object becomes part of a repository only when its content was sent. Uploading content the store already has is cheap: the bytes are not written again.
- A land checks that every tree and blob its snapshot adds over its base is part of the repository, checking each level of trees before reading it. A snapshot that names another repository's objects without uploading them is refused, and learns nothing about them.

Objects also become part of a repository when the git bridge imports them and when the server makes them itself from the repository's objects (three-way merges on land and ship). The empty tree is part of every repository. The web UI and the MCP tools read file contents through `Service.BlobReader`, which checks the same things.

Requests without a repository come from clients older than v0.2 and are refused with "this vx is too old for the server: update vx".

Repositories from before `repo_objects` are indexed once, from their changesets (`internal/service/objects_backfill.go`). Migration 009 queues every existing repository, and each server start also queues any repository that has changesets but no recorded objects. The server then walks the queued repositories one at a time in the background, recording for each changeset what differs from its parent's tree, and logs its progress. An object request for a repository that is still queued backfills that repository first, so it waits for its own walk only. The walk is idempotent and resumable; `vx-server backfill-objects [--repo OWNER/NAME]` runs it by hand.

## Blob store

`blob.Store` (`internal/blob`) has three operations, `Put`, `Get` and `Has`, over immutable objects named by their hash; every read is checked against the hash. `VX_BLOB_URL` picks the backend (`blob.Open`):

- Disk (default): `DATA/objects/9f/1c/9f1c...`, each file `vx1` + a kind byte + zstd data, written to a temporary file and renamed.
- S3-compatible bucket (`s3://`, `gs://`): the same keys under a prefix and the same bytes, so a disk store copied into a bucket is a valid bucket store. `blob.S3Client` is a small SigV4 client (no SDK): put, get, head, list (ListObjects v1, which every service has), delete and multipart uploads, retried with exponential backoff and jitter on network errors, 5xx and 429; a download cut short resumes with a ranged request pinned to the object's ETag. `Put` first asks `HEAD`, so an object the bucket has is not uploaded again; `Has` is a `HEAD`, nothing is downloaded. Bucket stores sit behind `blob.Cached`, a size-bounded LRU of object data in memory; it caches presence, never absence, so objects another server just wrote are found.
- Memory (`blob.NewMem`), for tests.

Optional interfaces: `Lister` (every object, for `vx-server blobs copy`), `Sizer` (stored bytes without reading: a stat or a `HEAD`) and `Deleter`, which only the purge of a deleted organization uses (`internal/service/org_purge.go`): it marks the objects no repository outside the organization records (`blob_deletions`), and deletes each after checking again, under an advisory lock on its hash, that no repository records it. Recording objects for a repository (`Store.RecordRepoObjects`: uploads, merges, imports) checks the marks after recording, under the same lock shared, and writes back an object a purge deleted between its `Put` (which found it and skipped writing) and the recording. For the same reason `blob.Cached` never answers `Put` from the cache, and `blob.Fresh` reads past it. `blob.Copy` copies between any two stores with a pool of workers, reads each copy back, and skips what the destination has, which makes it resumable. The service records each object's stored size in `object_sizes` (`internal/service/objectsizes.go`): uploads and merges queue their new objects for a background sizer, and a full pass at start and every 10 minutes catches imports and older objects. `Store.RepoStoredBytes` and `Store.OrgStoredBytes` sum them for quotas, an object once per organization.

Objects are whole byte slices in memory, bounded by the API's message sizes; streaming with bounded memory is for backup files (`S3Client.PutStream`, `Open`).

`internal/blob/s3test` is an in-process fake of the S3 API that checks every signature (`blob.VerifySigV4`), for tests; `make test-minio` runs the store against MinIO.

## Landing

```mermaid
sequenceDiagram
  participant A as Agent (vx CLI)
  participant S as vx-server
  participant DB as Postgres
  A->>A: snapshot working copy into trees
  A->>S: upload objects the repository lacks on the server
  A->>S: Land(workspace, base, tree, evidence)
  S->>DB: read effort head
  alt head == base
    S->>S: new tree = snapshot
  else effort moved
    S->>S: three-way merge(base, snapshot, head)
    S-->>A: conflicts, if any (nothing lands)
  end
  S->>DB: insert changeset; UPDATE efforts SET head WHERE version = seen
  alt another land won the race
    S->>S: merge again onto the new head and retry
  end
  S-->>A: landed changeset; CLI updates the working copy
```

The merge runs file by file (`internal/engine`), then line by line for files both sides changed (`internal/merge`, Myers diff plus diff3). Moving the head is a single-row compare-and-swap, so concurrent lands never lose work.

Line merge rules, per region between lines both sides kept from base:

- Changed on one side only: take that side.
- Changed the same way on both sides: take it once.
- Both sides only inserted lines (the base region is empty) and the insertions differ: keep both, the side that landed first (the effort head, or the ship target) before the incoming one. The diff of base to each side decides where an insertion sits, so a line both sides kept from base (a closing brace after a list) stays once and both insertions go before it, while lines each insertion brings itself (its own closing brace or trailing blank line) are kept once per side. If the earlier side's insertion ends the file without a newline, one is added so the lines do not run together. Engine results list these paths in `KeptBoth` (a subset of `AutoMerged`); `Service.Land` returns them as `LandResult.KeptBoth` and `vx sync` reports them as well.
- Anything else, where either side changed or deleted base lines in the region, is a conflict written with diff3 markers. Keep-both never applies when the base file is empty or missing (both sides created the file), to binary files, or when either side already contains conflict markers. There is no per-repo switch yet.

Merge by declaration, for `.go` files whose line merge conflicts (`internal/engine/declmerge.go`, #17). The engine parses base, ours and theirs (`go/parser` with comments) and cuts each into a header (build constraints, package doc, package clause), the import declarations, one chunk per top-level declaration (the comments and blank lines above it, the declaration, its line comment) and a tail (comments after the last declaration). Declarations are keyed like `CheckGo` keys them: `func F`, `method T.M` (the receiver's base type, generics included), `type T`, `var a,b`, `const A,B`; blank declarations by their text, `init` funcs by their order. Then:

- A chunk only one side changed, added or deleted takes that side; one changed identically on both sides is taken once. The header and the tail merge the same way, as single pieces.
- It gives up, and the text conflict stands exactly as the line merge wrote it, when: both sides changed one declaration (or the header, or the tail) differently; one deleted a declaration the other changed; a name both sides touched came out differently (extending a `var`/`const` group on one side and deleting it on the other changes different keys but the same names); a side deleted a declaration and the other renamed it, or both renamed it differently (a rename is a deleted declaration and a new one of the same kind with at least half its non-blank lines equal once the names are blanked out); the result would declare a name twice; any version does not parse, has two declarations on one line, or the package names of ours and theirs differ; the file imports `"C"` and both sides changed its imports; the result does not parse or does not hold exactly the merged declarations after `gofmt`.
- Imports are united: an import stays unless one side removed it, both sides' additions are kept, and an import a side removed comes back when the merged code still uses its name (`X.Sel` with `X` unresolved in the file) and no other import provides it. Two different packages under one name give up. When only one side changed the import block it is taken verbatim (grouping, comments, a cgo preamble); otherwise the block is rebuilt as standard library, a blank line, the rest, keeping each spec's comments.
- Order: ours' declarations in ours' order; one only theirs has goes right after the declaration that precedes it in theirs, so where both sides inserted at one place theirs comes first, as with keep-both lines. Chunks are joined with one blank line and the file is `gofmt`ed.
- Never for generated files (a `[[generated]]` rule or the `Code generated ... DO NOT EDIT.` line): those keep ours before any merge is tried. Not for files both sides created (no base).

Results list these paths in `MergedByDecl` (a subset of `AutoMerged`, disjoint from `KeptBoth`): `Service.Land` returns `LandResult.MergedByDecl` (`merged_by_decl` in `LandResponse`), `vx land` and `vx sync` print `Merged by declaration (...): PATH` next to `Kept both insertions`, `vx sync --json` has `merged_by_decl`, and ship plans, rebased changes and the overlap forecast, which have no list of merged files, report a warning of kind `merged-by-declaration` per file.

After a land, the server parses the Go packages the change touched (`internal/engine/gocheck.go`) and returns warnings for top-level names declared twice in one package and for changed Go files that do not parse: the case where two changes each add the same function in different files, the text merges cleanly and the package stops compiling. Warnings never block a land; they are stored with the changeset (`changesets.warnings`) and ship plans report them too, as does the pull request body.

When a land merged onto newer work, `engine.CheckGoSides` (`internal/engine/semantic.go`, #14) also compares the two sides: base, ours, theirs and the merged tree. It reports where one side's change meets the other side's new code: kind `signature`, a function or method whose type parameters, parameter or result types one side changed (compared as written, names ignored, and the change survived the merge) that the other side calls with the old shape, in the same package or a package importing it within the tree (import paths from the nearest `go.mod`); kind `removed`, a top-level func, type, var or const one side removed or renamed (gone from the merged package) that the other side's new code still names. Only packages one side changed are searched for changed declarations, and only files the other side changed for uses; a use counts when its line in the merged file is one the first side's version of that file does not have. Heuristics, to stay quiet when unsure: a call is reported when its argument count fits the old signature and not the new one, when the result count changed and the value is used, or when only parameter types changed and the argument at a changed position is not a literal; methods are matched by name, only in files of their package or importing it, and only on an argument count that stops fitting, and removed methods are not reported; a same-package name counts only where the parser leaves it unresolved (not a local, parameter, field or composite-literal key), another package's only as `PKG.Name` with `PKG` its import. Build constraints are not evaluated; files that do not parse are skipped. `Warning.Path` is the using file, `Name` is `pkg.Name` (`pkg.T.M` for methods), `Files` are the declaring and using files. The same check runs on `vx sync` (your local changes against the incoming effort), ship plans (effort against target branch), each rebased change that was merged, and the overlap forecast (kept when the two files belong to one effort each; the forecast only simulates pairs that share a file or directory).

`vx land --run` adds a client-side loop: sync if the effort moved, run the checks on the merged files, land with `require_up_to_date`, and repeat if someone landed in between. The evidence then describes exactly the tree that landed. When the server merges instead, the changeset records the author's `snapshot_tree`, and every view marks its evidence as gathered before the merge.

## Repository rules (vx.toml)

`vx.toml` at the root of a tree declares what text merges cannot see (`internal/rules`, a pure package over `blob.Store`, so the server and the CLI apply it to the same trees): `[[unique]]` keys that files may not share, `[[generated]]` files that are never text-merged, and `[checks] required` commands. `rules.Parse` uses BurntSushi/toml and rejects unknown keys, invalid globs (`*`, `?` within a segment, `**` across segments) and regular expressions without a group, naming the line. `rules.At(tree)` reads the file at the root of a tree; no file means no rules.

- **Generated files in merges.** `engine.MergeTrees` and `MergeTreesKeepConflicts` take `engine.WithGenerated(func(path) (command, ok))`, built by `rules.MergeOptions` from the `[[generated]]` rules of every side's tree. A path both sides changed that a rule matches, or whose ours, theirs or base version says `// Code generated ... DO NOT EDIT.` (`engine.IsGenerated`, always on), is not merged: the result takes ours (the snapshot on land and sync, the replayed change on ship, rebase and the overlap forecast), it is not a conflict, and `MergeResult.Generated` lists it. Callers turn it into warnings of kind `generated` (`engine.GeneratedWarnings`): `` generated file gen/x.go: regenerate with `buf generate` ``. A rebase that stops on another file reports the generated files of that change as `RebaseConflict{Kind: "generated", Generated: true}`, and the CLI writes the change's version.
- **Unique keys.** `rules.Check(rules, base, tree)` lists, for each `[[unique]]` rule whose glob matches a path changed between base and tree, the files under the glob's literal directory, groups them by key and warns (kind `rule`, `Name` the rule, `Decl` the key, `Files` every path) about each key used twice where at least one file changed. Land checks the effort head against the landed tree, `vx sync` the effort head against the merged working copy, a ship plan the target head against the shipped tree, a rebase each replayed change, and the forecast the target head against both efforts combined. A change that breaks `vx.toml` gets one `rule` warning; later changes are not checked until it parses again.
- **Required checks.** `Land` sets `Evidence.NotVerified` to the required commands (rules at the snapshot tree) that no check with `RanByVX` names exactly, after trimming; a client's value is ignored. It is part of the hashed changeset (omitted when empty, so older hashes do not change).
- **Reservations.** `Reserve` reads the rule at the effort head, collects the keys of the rule's files in the head trees of the target branch and every open effort, adds earlier reservations (`rule_reservations`), and inserts one more than the largest (zero-padded like the existing keys). The table's primary key (repo, rule, value) makes a concurrent caller lose the insert; it reads the reservations again and retries.

## Rebasing an effort

`vx effort rebase` (`EffortService.RebaseEffort`) moves an effort onto the latest head of its base branch, after syncing that branch from upstream:

1. Record a running rebase (`effort_rebases`; a partial unique index allows one running or stopped rebase per effort) with the target changeset, the effort's old base and head, and the list of changes to replay: the vx changes from the old base to the head, oldest first.
2. For each change C with parent P: if P's tree equals the rebased chain so far, take C's tree; otherwise three-way merge base P, ours C, theirs the chain so far, exactly as shipping does, so keep-both puts the branch's lines first. Store the result as a new changeset with the same change id, intent, author, evidence and creation time, the chain so far as its parent, and `rebase` provenance: target (`onto`, `onto_git_sha`, `branch`), the changeset it replays (`from`), who ran it and, after a conflict, who resolved it. The provenance is part of the hashed encoding. Replayed changesets are stored without an effort until the rebase finishes.
3. On a conflict, save the progress and stop (`stopped`). `ContinueRebase` takes the resolved tree of that change (uploaded like a land's snapshot, on top of the chain so far), stores it as the change's rebased version marked `resolved_by`, and goes on; it may stop again. `AbortRebase` marks the rebase aborted; the effort never moved.
4. When every change replayed, compare the effort head with the newest old-chain head the rebase knows. Changes landed since (lands never wait for a rebase) are replayed the same way, and the check repeats until the head is stable.
5. In one transaction: compare-and-swap the rebase row (on its version, so a concurrent continue or abort wins cleanly) and the effort (base branch, base and head, on the effort version, so a land that sneaks in sends the rebase back to step 4), and attach the replayed changesets to the effort.

Every step that changes the rebase row is a compare-and-swap on its version. A change id resolves to its newest version on the effort; a version stored by a stopped or aborted rebase never wins. Workspaces opened before a rebase keep their old base: `Land` and `SetBase` accept a base on the old chain of a finished rebase, and the three-way merge from that base onto the rebased head brings their edits across. Evidence on rebased changes is labelled as gathered before the rebase wherever it is shown, like the evidence of changes the server merged.

## Git bridge

- One bare git mirror per repository under the data directory (go-git, no git binary needed). Mirrors are a cache of the upstream: everything vx needs lives in Postgres and the blob store, so a server without a mirror creates one and fetches, and a mirror that no longer opens is set aside and fetched again (`Bridge.mirror`). Exports reuse git objects only when the mirror has them and write the rest from vx objects.
- Several servers may import one branch at once: `SyncBranch` moves the branch only from the commit it read at the start (`Store.SetBranchIfUnchanged`) and starts over when another server moved it first, so a slower import never moves a branch back.
- Only branches in use are fetched; the first import is shallow.
- Git objects are mapped to vx objects in Postgres (`git_objects`), so later imports skip unchanged subtrees and exports reuse git objects.
- Repositories of a GitHub App installation use its installation token (HTTPS as `x-access-token`, cached until shortly before it expires). Others: without a GitHub token the bridge uses SSH with the server user's agent and `known_hosts`; with a token it uses HTTPS and the REST API.

## Shipping

1. Sync the target branch.
2. Replay each landed vx changeset, oldest first, onto the target head: take its tree when its parent equals the current tree, otherwise three-way merge it onto the current tree. Stop at the first conflict and report it; `vx effort rebase` resolves it inside vx (see "Rebasing an effort").
3. Write one git commit per change: the intent as the message, provenance trailers (`Vx-Change-Id`, `Vx-Agent`, `Vx-On-Behalf-Of`, `Vx-Model`, `Vx-Evidence`, and for rebased changes `Vx-Rebased-Onto` and `Vx-Resolved-By`), the person as author.
4. Push to `vx/<effort>` (the bridge refuses any other branch) and open or update the pull request.

### Several pull requests (`internal/service/ship_split.go`)

`PlanSplit` groups the changes the target lacks into parts. Two changes are tied when they touch a common path, or when the newer one's `depends_on` names the older one's change id (or a 4+ character prefix) or a symbol or path the older one changes (the same check claims use, `landTouches`).

- `independent`: union-find over the ties, parts ordered by their oldest change. Each part is replayed onto the target head alone (`replayChain` on the part's changes); parts that share a path are also replayed onto each other in both orders (`replayForecast`) and must be clean.
- `stacked`: one part per change, or a person's ordered list; part n is replayed onto the tree after part n-1.
- A person's grouping (`SplitInput.Groups`) must hold every change exactly once. Problems are reasons, not errors: a part whose replay conflicts names the change of another part whose lines it builds on (a change of another part, older than the conflicting one, that touches the conflicting path), or the target when there is none; a `depends_on` across independent parts, or onto a higher stacked part, is refused too. `OpenShipment` refuses a plan with problems.
- Without a person's grouping, open parts of an earlier split of the same mode seed the grouping, so shipping again keeps branches and pull requests; ties between two already shipped changes are not followed (a person may have split them on purpose), ties to new changes are. Parts the new grouping lacks are `Stale` and set to `closed` when the split ships.

`openSplit` pushes every branch first (stacked part n on part n-1's last commit), then opens or updates the pull requests in order with `Base` = the target or the previous part's branch, then updates the bodies that could not know the later parts' numbers yet. Each part is one `shipments` row (`split`, `part`, `target` = its base). Merged detection: `markMergedShipments` (run by `markMergedEfforts`) marks a part merged when the target branch carries all its change ids; `markShipmentMergedByHead` (merged pull request webhooks, for squash merges) marks the part pushed to the head branch merged, and its effort once no other part is open.

## Overlap forecast

Efforts ship separately, so two of them can change the same code, or each add migration 008, without anyone knowing until the second one ships (#26). The forecast (`internal/service/overlaps.go`) answers, for every pair of open efforts of a repository with the same base branch: what happens when both ship?

1. For each effort, the paths changed from its base tree to its head tree. Shared files are the paths both changed.
2. No shared file and no directory in common: the pair is `clean`, stored without a simulation.
3. Otherwise simulate: the effort whose first change landed earlier is replayed onto the branch head with the replay of shipping (`replayChain`, the loop `PlanShipment` uses, here with `MergeTreesKeepConflicts` so it goes on past a conflict). If that effort alone conflicts with the branch (it needs a rebase anyway), its own head tree stands in. The other effort's net change (its base tree to its head tree) is then three-way merged onto the result. Conflicts are the shared files that do not merge.
4. On the combined tree, `engine.CheckGo` and `CheckRules` (with the `vx.toml` of the combined tree, against the branch head's tree) run over the union of both efforts' changed paths, and `engine.CheckGoSides` over the second effort's net change against the first. Only warnings that involve a file of each effort are kept: a duplicate declared in a file of each, two migrations with one number, one from each, a signature or name one effort changed that the other's new code uses the old way. Shared files the merge could only do by declaration add a `merged-by-declaration` warning. The pair is `conflict` with conflicts, `overlap` with shared files or warnings, `clean` otherwise.
5. The result is stored in `effort_overlaps` (one row per ordered pair) with both effort heads and the branch head it was computed on. When none of the three moved since, the pair is skipped.
6. When status, conflicts or warnings differ from the stored row (shared files alone do not count), vx posts a note of kind `overlap` on both boards, authored by the `vx` service principal and marked `system`, on the shared files, superseding the pair's previous note on that board (`a_note`, `b_note`). A pair that turns clean gets one `Resolved:` note; clean after clean posts nothing. Writing the row and the notes holds a transaction-scoped advisory lock on the pair and re-reads the row, so concurrent runs, also on other servers, post once.

Triggers: every successful land queues a forecast of its effort, after the land transaction, in a background goroutine per repository that coalesces requests and is bounded by `ForecastTimeout` (default 2 minutes); errors are logged and never reach the land. A branch sync that imported commits (`vx repo sync`, shipping's sync, a GitHub push webhook) queues every open effort on that branch, and a finished rebase queues its effort. `ListOverlaps` recomputes stale pairs of the effort it is asked about first, within 20 seconds, and otherwise returns what is stored; the land response, `Context` and the web UI only read what is stored. On shutdown the server waits for running forecasts within its grace period.

Limits:

- The second effort is merged as one net change, not change by change: intermediate history that a later change undid is not reported, though shipping in that order may still stop at it (`vx effort rebase` resolves it).
- Only pairs are simulated. Three efforts that each merge pairwise but not all together are not detected, and the order is fixed (first land first); the other order can differ for keep-both insertions.
- Efforts with different base branches are never compared; closed efforts drop out.
- Directory sharing decides whether a pair without shared files is simulated, so a rule glob that spans directories can be missed when the efforts touch no common directory.
- A run that hits its timeout leaves the remaining pairs stale until the next land, branch move or `ListOverlaps`.
- Merged trees and conflict-marker blobs the simulation makes are written to the blob store but never recorded as part of the repository.

## Code layout

| Package | Responsibility |
|---|---|
| `internal/hash` | BLAKE3 object hashes |
| `internal/blob` | Content-addressed store (disk, S3-compatible buckets, memory cache, memory) with verification on read; copies between stores |
| `internal/object` | Trees, manifests, changesets, tree diff and in-place edits |
| `internal/merge` | Line diff, unified diff output, three-way text merge |
| `internal/engine` | Tree-level three-way merge |
| `internal/store` | Postgres access and migrations |
| `internal/service` | Application logic: repos, efforts, workspaces, land, ship |
| `internal/gitbridge` | Import from and export to git hosts |
| `internal/api` | connect-go handlers |
| `internal/web` | Web UI |
| `internal/client` | Go client with batched object transfer |
| `internal/workcopy` | Working copy: scan, stat index, apply |
| `internal/cli` | The `vx` command line |
| `proto/` | API definition |
