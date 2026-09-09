# Trust and security

This page states what Entire Graph reads, writes, executes, and sends over the
network, scoped precisely enough to be checked. Statements match the 0.4.0
release target and the implementation under `internal/`.

## Network

The built-in analyzer is local-only. During analysis it does not fetch remote
code, download grammars or models, call hosted APIs, resolve embeddings, or
send telemetry. Tree-sitter grammars are compiled into the binary. The
benchmark harness's clone phase and `entire plugin install graph` are the
networked operations, and both are explicit: one is a development tool, the
other is installation.

## What it reads

- Repository content: the working tree (default for interactive queries) or
  committed objects via `git` (for `--head`, bulk streams, and `diff`/
  `commit`).
- Bounded local Git history, for co-change relations (`FILE_CHANGES_WITH`) and
  change analysis. History never leaves the machine.
- Structural Git metadata needed by those subprocesses. Before Git starts, the
  provider walks the resolved administrative/common directories and recursive
  alternate object stores from held handles, with fixed entry and
  resolver-relative-path bounds, and rejects mount points, off-volume redirects,
  and special files.
  Git's own fsmonitor daemon socket is permitted because every provider Git
  subprocess explicitly disables `core.fsmonitor`. Before those subprocesses
  start, inherited global and system Git configuration is disabled. Protected
  command-scope `safe.directory` entries authorize only the selected command
  directory and its ancestors (the exact repository-discovery candidates),
  never the unrestricted `*` value, so explicitly selected shared checkouts
  remain usable without trusting unrelated repositories. The bounded
  repository-metadata preflight rejects active repository-local
  `[include]` and `[includeIf]` sections, including in an enabled
  `config.worktree`. Repository-local configuration remains available for
  structural settings Git needs; active `core.worktree` paths are checked by
  the same rooted preflight. Active partial-clone/promisor configuration is
  refused because supported Git versions before 2.45 can inspect a local or UNC
  promisor URL before enforcing the transport deny-list. At command scope the
  provider sets
  `core.fsmonitor=false`, `log.showSignature=false`, `log.mailmap=false`,
  `submodule.recurse=false`, and empty values for `core.excludesFile` and
  `core.attributesFile`; `diff.orderFile` is pinned to the platform null device.
  Configuration-derived external ignore, attribute, mailmap, and diff-order
  files are therefore not read or honored, and grep does not enter nested
  submodule worktrees. Machine-readable grep calls explicitly disable configured
  line numbers, columns, color, and full-name rewriting. Working-tree snapshot
  caching stays disabled instead of asking Git's conversion-aware status
  machinery to run repository-configured clean or process filters. Remaining
  configuration-derived structural limitations, including relocated ref storage,
  are recorded in `docs/parking-lot.md`. Adjacent setup helpers reuse a
  repository-bound validation receipt only within the current operation;
  validation results are not cached globally or persisted.
- Before Git recursively enumerates untracked worktree content, a bounded
  held-root preflight walks the complete directory tree without resolving a
  nested case-insensitive `.git` marker. A nested marker selects the bounded
  filesystem fallback only after the preflight finishes checking every other
  directory. Unreadable local subtrees retain the fallback's warned omission,
  while the preflight continues across every other reachable directory. A
  traversable redirect, mount boundary, cancellation, or traversal ceiling
  fails closed. Every earlier Git failure also passes through this safety gate
  before the filesystem fallback begins. Under Go's legacy Windows reparse-mode
  compatibility, mount points surface as symlinks instead; both filesystem
  fallbacks apply their no-follow rule before considering a directory entry.
  This prevents an untracked gitfile from making Git resolve a UNC or off-volume
  target before the post-listing Git-directory excluder can observe it.
- `stats` applies the same worktree safety gate before sampling on-disk file
  sizes. Transcript-derived top-hit paths are confined beneath the selected
  repository and measured with a final-component no-follow lookup; an unsafe
  worktree contributes no filesystem-size estimate.
- Repository-controlled Git ignore policy, including nested `.gitignore` files,
  `.git/info/exclude`, and per-worktree excludes, plus `.graphignore` and any
  `--ignore-file`/`--include-file` the caller passes.
  Caller-supplied ignore inputs are bounded to 1 MiB per file and 64 KiB per rule
  line. One listing retains at most 16,384 parsed external rules and observes at
  most 512 nested `.gitignore` files. A limit refusal is reported instead of
  truncating the policy and silently changing the indexed corpus, and so is a
  `.git` this process cannot read while resolving `info/exclude`: absence of a
  git directory degrades to "no exclude list", but a failure to READ one that is
  there is refused rather than dropping the repository's own exclusions. Nested
  worktree ignore files are confined to the repository and are not followed
  through a symlink that escapes it. When Git cannot enumerate a worktree, the
  bounded filesystem fallback applies the ignore files it can observe and emits
  `W_GIT_WORKTREE_FALLBACK`; Git-only policy may be unavailable, so the warning
  explicitly reports that excluded files can be present. Configuration-derived
  `core.excludesFile` is outside the effective policy in both the Git and
  fallback paths. The fallback's ignored-tree `.git`-pointer sweep stays beneath
  a held repository root and refuses redirects or mount points (including
  same-filesystem bind mounts on Linux); a refused directory is disclosed as
  `W_GITDIR_SWEEP_UNREADABLE_DIRECTORY`.
- Inherited repository-selection, attribute-source, and pathspec-control
  variables, every `GIT_TRACE*` output target, and Git for Windows'
  `GIT_REDIRECT_*` standard-stream targets are removed from production Git
  subprocesses. `GIT_TEXTDOMAINDIR` is removed too because Git probes it during
  startup. Those inherited values therefore cannot open an arbitrary path or
  socket (including a Windows UNC target) or replace explicit machine-command
  path semantics. When `GIT_CEILING_DIRECTORIES` is present during implicit
  discovery, the CLI applies usable absolute entries in-process. Entries before
  Git's first empty-list marker are canonicalized with a same-volume guarded
  walk; subsequent entries stay lexical as Git specifies. The CLI never gives
  Git the raw list, whose canonicalization could itself probe an off-volume or
  UNC path. If a pre-marker entry cannot be resolved without such a probe,
  implicit discovery fails closed. All selected-repository subprocesses
  discard it.
- For `stats` only: local coding-agent session transcripts
  (`~/.claude/projects/<path-slug>/*.jsonl`, or `--sessions-dir`/
  `--transcript` overrides). This is Claude Code's transcript layout; the
  report is read-only and local. No other command reads transcripts.

### Credential-store path exclusions

A built-in exclude list keeps credential stores out of the provider's
working-tree and committed-tree source corpora. Within those corpora, unless a
caller deliberately re-admits one, a denied path is not parsed as source,
indexed, written to graph/search caches, returned in graph or search results, or
quoted in search context blocks.

This is a corpus and output boundary, not a promise that no local process reads
the underlying file or Git blob. Git-backed search acceleration can scan
tracked or committed blobs before credential-store path rules are applied to
its matches; denied matches are discarded before Entire Graph's content reader,
source parser, caches or responses. The `diff`, `analyze`, `commit` and
`checkpoint` families analyze requested Git ranges rather than the graph/search
corpus, so they can read credential-store paths and report content-derived
metadata about them. Those built-in operations remain local under the provider's
no-egress contract. `verify` runs caller-supplied commands with the caller's
privileges and is outside this exclusion boundary.

The list covers `.env` and its `.env.<environment>` variants, `.envrc`,
`.npmrc`, `.netrc`/`_netrc`, `.pgpass`, `.htpasswd`, `.pypirc`, `.dockercfg`,
`.boto`, `.git-credentials`, Docker's `.docker/config.json`, Kubernetes'
`.kube/config`, Terraform's `credentials.tfrc.json`, Google Cloud's
`application_default_credentials.json`, SSH private keys (`id_rsa`, `id_dsa`,
`id_ecdsa`, `id_ed25519`),
`credentials`/`credentials.{json,yml,yaml,ini,toml}`,
`secrets.{json,yml,yaml,ini,toml}`, key material and encrypted stores by suffix
(`.pem`, `.key`, `.pfx`, `.p12`, `.pkcs12`, `.jks`, `.keystore`, `.truststore`,
`.ppk`, `.kdbx`, `.asc`, `.gpg`), and files ending in `.yaml`, `.yml`, `.json`,
`.ini`, `.toml`, `.cfg`, `.conf`, `.properties`, `.txt` or `.enc` under a
directory segment named `secrets/` or `credentials/` at any depth. Matching is
case-insensitive. The Git, Docker, Kubernetes, Terraform, and Google Cloud
entries match at any depth and are file-only, so a same-named directory and its
descendants remain searchable.

Outside those file-only entries, matching retains Git-style path semantics. A
basename or suffix pattern can therefore match a directory segment; when it
does, that directory's descendants are excluded. For example, `*.key` also
excludes `pkg/client.key/**`. This behavior is intentional.

Classification is based on PATHS, not content: Entire Graph does not inspect a
file's bytes to decide whether an otherwise-unrecognized path contains a
secret. A credential store whose path gives no signal — `deploy/prod-values.yaml`
holding an inline API key — is not covered. Public halves (`.crt`, `.cer`,
`.pub`) are deliberately not excluded, and source code under
`internal/secrets/` or `pkg/credentials/` stays fully searchable.

The list is loaded after the repository's own exclude files, so a negation
inside the repository under analysis cannot switch it off, and before the
caller's `--ignore-file`/`--include-file`, so `--include-file` remains the way
to deliberately re-admit a path (a checked-in `.env.example` used as
configuration documentation, for example).

Because of that position the list only ever adds exclusions: it contains no
negation, so it cannot re-admit anything the repository itself excluded. The
bare `credentials` entry — the AWS CLI shape, `.aws/credentials` — is matched as
a FILENAME only for that reason: a directory named `credentials/` is neither
excluded by it nor re-admitted when the repository's own `.gitignore` or
`.graphignore` excludes it.

Both persistent caches (`index`/`search` snapshots and the streamed record
caches) key on a digest of the effective built-in rules, so a cache entry warmed
by a build with a different policy is not reachable — an entry written before
these rules existed misses instead of re-emitting the paths it named.

## What it writes

- Derivative caches, under `--cache-dir`, else `ENTIRE_PLUGIN_DATA_DIR`, else
  (for most query commands) the per-user cache directory. Cache entries are
  compressed snapshots rebuilt from repository state; deleting them costs a
  rebuild, nothing else. Queries never modify repository source files. The
  cache directory you name is the trust boundary: it is resolved as given and
  may itself be a symlink. Everything below it is named by Entire Graph — a
  family, a version, and a SHA-256 digest. Writes open each family and version
  component, compare the held directory with the name's filesystem identity,
  and refuse symlinks, Windows junction and mount-point reparse entries, and
  identity swaps even when the redirect would remain inside the opened root.
  This intentionally drops the older behavior where an in-root family or
  version alias could work; allowing it would let a repository steer derivative
  bytes into `.git` whenever the cache root is a checkout. To relocate the
  cache, name the backing directory as the root or make the root itself a
  symlink. Reads remain confined by `os.Root`; query writes fall back cold on a
  refusal, while `index` reports it. This boundary covers repository-controlled
  entries and substitutions observable while a component is opened, not a
  concurrently running process with permission to rename an already-opened
  directory. `os.Root` intentionally keeps using that directory object after a
  move; portable Go cannot pin its lexical ancestry, and a process with that
  namespace authority can already move existing cache artifacts.
- `init-agents` writes through exactly three repository paths, disclosed in
  [agent activation](agents.md): `.entire/graph-agent.md` and managed blocks
  in `AGENTS.md` and `CLAUDE.md`. A repository-committed symlink at one of
  those paths may redirect the write to another file, which is what makes the
  alias support in the activation guide work; the redirection is confined to
  the project root, and additionally refused when it lands in a git directory —
  recognised by structure, so an administrative directory not named `.git` is
  covered too — or on an existing file that is not an agent-instruction file, so
  a hostile checkout cannot aim a managed block at `.git/config`, a hook, a
  `Makefile`, `.envrc`, or a CI workflow.
  A hard link is the one route none of that covers: `ln .git/config CLAUDE.md`
  gives git's config a second name that resolves to `CLAUDE.md`, spells no
  `.git` component and stats as an ordinary regular file. An inode's other names
  cannot be read back from it, so a managed target is refused unless every name
  it has is `AGENTS.md` or `CLAUDE.md`. Those two instruction files may share
  one inode; the generated `.entire/graph-agent.md` guide must remain distinct.
  This also refuses a hard link to a harmless file, including one outside the
  project. A symlink remains the preferred instruction-file alias.
- `index --report <path>` writes a Markdown graph report to the path you give
  it.
- `verify --record-baseline <path>` creates parent directories as needed and
  writes a JSON baseline to the path you give it.

No other command family writes into the repository unless a caller-provided
command does so.

## What it executes

- Graph queries (`search`, `def`, `explain`, `neighbors`, `impact`) and
  streams run `git` subprocesses and parse files. They do not execute
  repository code.
- `search` **suggests** a `VERIFY:` command derived from repository contents
  (test names, build files). It does not run it. Anything that later runs
  that command is executing text influenced by repository contents. Read the
  command first in repositories you do not trust.
- `entire graph verify` executes the test command the caller provides and
  adjudicates the result. With `--setup <command>`, it executes that setup
  command first. Both run with your privileges; pass only commands you would
  run yourself.

## Payload integrity (`text` and `agent` formats)

The `text` and `agent` payloads are a line-anchored record stream: every record
— a ranked hit, a passage header, the `VERIFY:` command, a declaration card
entry — begins at column 0, and the source quoted between records is lifted
verbatim out of tracked files. A file whose own content holds a column-0 line
shaped like a record is therefore, once quoted into a snippet, hard to tell
apart from output this tool authored. `VERIFY:` is the sharp edge: it is the
one line the agent guide tells an agent to run.

What the tool does about it: every repository-derived body these two formats
print is scanned, and any line that would be read as one of the tool's own
record heads is indented by one space, which takes it out of record position
while leaving its content byte-for-byte intact. A payload that indented
anything says so, on its own first line, beginning `UNTRUSTED FILE CONTENT:`.

What that does **not** give you:

- It is not authentication. A forged record becomes detectable, not
  impossible; nothing stops a reader that ignores indentation from acting on an
  indented line.
- The grammar is a closed set covering the records the `search` renderers emit.
  `def`, `impact`, `neighbors` and `callsite` print source through their own
  paths and are not covered.
- `--presearch` echoes a caller-supplied file verbatim and is not inspected.

**`json` and `ndjson` are structurally immune** and are the right choice for
any consumer that parses output: a snippet is a quoted string value with its
newlines escaped, so repository content cannot become a record there whatever
it holds.

## Determinism and heuristics

The same repository view and options produce the same graph. There is no
hidden per-user state influencing results; learned or durable memory belongs
to [Entire Brain](brain-and-graph-boundaries.md), not this provider.

Within that determinism, static relations are heuristic. Call, route, event,
test, and data-flow edges carry `resolution` and `confidence` fields instead
of a completeness promise; dynamic dispatch, reflection, and generated code
can produce missing or unresolved edges. Inventory-only languages get file and
symbol structure with no semantic relations. `capabilities --json` reports
which tier a language is in. Files the parser cannot process emit
machine-readable partial failures rather than disappearing silently.

## Repository-controlled exclusions are disclosed

`.graphignore`, `.gitignore` and `.git/info/exclude` live in the repository, so
whoever can commit to it decides part of what the graph sees. One committed line
naming a tracked source file removes that file from every answer.

`search` therefore reports what those rules removed rather than presenting the
surviving corpus as the whole of it. When repository-controlled rules exclude
files Git itself lists, the response carries `repo_ignored` (the count, the ignore
files responsible, and up to ten of the excluded paths),
`stats.files_excluded_by_repo_ignore_rules`, and a `W_REPO_IGNORED_SOURCE`
warning; the text and agent payloads print the count and name the paths. A
repository that excludes nothing adds nothing.

Exclusions **you** asked for with `--ignore-file` are not reported: they are your
own instruction, and reporting them back would bury the case that is not.

The count is exact except in two cases, and both say so. When enumerating an
excluded directory tree hits something it cannot read, `repo_ignored` carries
`count_incomplete` with the paths responsible and the response carries an
`E_REPO_IGNORE_UNREADABLE` partial failure. When the excluded tree is larger than
the accounting enumerates — the walk is bounded so that a committed rule over a
huge tree cannot hand back the cost the prune saved, on every search — the report
carries `count_incomplete` and an `E_REPO_IGNORE_COUNT_INCOMPLETE` partial
failure. Either way the number is known to be a lower bound rather than quietly
understated.

This is disclosure, not prevention. It tells you that files were removed and
which ones; it does not tell you whether one of them was the answer to your
query, and deciding that still means reading the file. Commands other than
`search` do not yet carry the disclosure.

## Command-family tree semantics

Interactive queries read the working tree by default and the committed tree
with `--head`. Bulk streams and ref-based analysis default to committed state
(`--worktree` opts the streams into the working tree). A result is therefore
always attributable to one requested repository view; the cache keying that
preserves this is described in the [operations cache guide](operations.md#cache).
