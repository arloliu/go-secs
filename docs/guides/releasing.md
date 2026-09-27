# Releasing

This repository publishes two Go modules from `main`:

| Module | Directory | Tags | GitHub release |
|---|---|---|---|
| `github.com/arloliu/go-secs/v2` | repository root | `vX.Y.Z` | marked Latest |
| `github.com/arloliu/go-secs/tracepack` | `tracepack/` | `tracepack/vX.Y.Z` | `--latest=false` |

The two modules release independently.
A released tracepack requires a released go-secs (v2.5.0 or later),
so a go-secs change that tracepack needs is released in go-secs first.

Both flows share the same shape: PR, rebase-merge, reset local `main`, annotated tag, GitHub release, proxy warm-up.
The repository allows rebase and squash merges only.
Never add `Co-Authored-By` or other attribution trailers to commits or tag messages.

## go-secs (`vX.Y.Z`)

```bash
# 1. Push the branch that holds the release work (CHANGELOG.md updated) and open a PR.
git push -u origin <branch>
gh pr create --base main --head <branch> --title "chore: release vX.Y.Z" --body "..."
gh pr checks <num>

# 2. Gates CI does not run: stress tests, and gorelease against the previous tag.
make stress-test
go run golang.org/x/exp/cmd/gorelease@latest -base=<previous vX.Y.Z>

# 3. Rebase-merge, then reset local main to the rewritten SHAs.
gh pr merge <num> --rebase --delete-branch
git checkout main && git fetch origin --prune && git reset --hard origin/main

# 4. Annotated tag and GitHub release with the CHANGELOG section as notes.
git tag -a vX.Y.Z -m "Release vX.Y.Z"
git push origin vX.Y.Z
awk '/^## \[X\.Y\.Z\]/{flag=1; next} /^## \[/{flag=0} flag' CHANGELOG.md > /tmp/notes.md
gh release create vX.Y.Z --title "vX.Y.Z" --notes-file /tmp/notes.md

# 5. Warm the module proxy (and transitively pkg.go.dev).
make update-pkg-cache
curl -s https://proxy.golang.org/github.com/arloliu/go-secs/v2/@v/vX.Y.Z.info
```

## tracepack (`tracepack/vX.Y.Z`)

Tags are annotated, on `main`, and independent of go-secs tags.
The first release is `tracepack/v0.1.0`.
A later major version is `github.com/arloliu/go-secs/tracepack/v2` in the same directory:
before tagging `tracepack/v2.0.0`, change the `module` directive in `tracepack/go.mod` to that path
and update every import of the module, including its own internal imports.
The tag alone would publish an invalid v2 module.

Before tagging:

- `tracepack/go.mod` requires a released go-secs (v2.5.0 or later), never a pseudo-version and never through a `replace`.
- The consumer CI leg is green on `main`.
  Locally: `make check-tracepack-consumer` (GOWORK=off, `go mod tidy -diff`, build, `-race` tests).
- `gorelease` runs inside `tracepack/` against the previous tracepack version (none before the first release).
- `tracepack/CHANGELOG.md` has the section for the version.

```bash
# 1. PR with the release work, merged as above (steps 1 and 3 of the go-secs flow).

# 2. Pre-tag gates.
make check-tracepack-consumer
(cd tracepack && go run golang.org/x/exp/cmd/gorelease@latest -base=vA.B.C)   # previous tracepack version; omit before v0.1.0

# 3. Annotated tag and GitHub release; --latest=false keeps the go-secs release marked Latest.
git tag -a tracepack/vX.Y.Z -m "Release tracepack/vX.Y.Z"
git push origin tracepack/vX.Y.Z
awk '/^## \[X\.Y\.Z\]/{flag=1; next} /^## \[/{flag=0} flag' tracepack/CHANGELOG.md > /tmp/notes.md
gh release create tracepack/vX.Y.Z --latest=false --title "tracepack/vX.Y.Z" --notes-file /tmp/notes.md

# 4. Warm the module proxy.
make update-pkg-cache-tracepack
curl -s https://proxy.golang.org/github.com/arloliu/go-secs/tracepack/@v/vX.Y.Z.info
```

## Local development across both modules

`make work` creates a `go.work` joining the root and `tracepack/`.
It is gitignored and never committed:
a committed workspace would make root builds select dependency versions from tracepack's requirements,
and it would hide a missing `require` in `tracepack/go.mod`.
Makefile variables built on `go list` run with `GOWORK=off`,
and `LATEST_GIT_TAG` matches only `v[0-9]*` tags,
so root targets behave the same with or without a workspace.
