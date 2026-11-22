# 7. The Go module is called `frontend` and the entry point is `cmd/server`

Status: accepted exception (owner decision 2026-11-14, confirmed 2026-11-22; application standards rules REPO-01 and REPO-03).

## Context

The standard asks for a module path equal to the GitHub path (`github.com/platform-boutique/app-frontend`) and a `cmd/<service>/` directory. This repository uses the bare module name `frontend` (`go.mod`) and `cmd/server`, which is also the path the shared pipeline (`platform-demo-workflows`) builds and starts and the same in the other shop repositories.

## Decision

Leave both as they are. The module is never imported by another module, so the name has no effect on consumers. Renaming it would change every import of the application and of the generated stubs, the `-ldflags -X frontend/internal/obs.Version=...` paths of the Dockerfile and the pipeline's build step, for no change in behaviour.

## Consequences

- `go install` of this repository by path is not possible; nobody needs it, the artefact is the image.
- The rule stays an exception for the shop repositories until the platform decides to rename them all in one change.
