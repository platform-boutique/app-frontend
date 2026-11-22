# Container image

Multi-stage [`Dockerfile`](https://github.com/platform-boutique/app-frontend/tree/main/Dockerfile):

| Stage | Base | What happens |
|---|---|---|
| builder | `golang:1.26-alpine@sha256:ecb67f98...` | `go mod download`, `go build -ldflags="-s -w -X ...obs.Version/Commit/BuildTime" -o /bin/frontend ./cmd/server` (`CGO_ENABLED=0`; target arch = builder's arch) |
| runtime | `alpine:3.24@sha256:f59abe65...` | binary + `templates/` + `static/` under `/app`; adds `ca-certificates` |

- **Entrypoint:** `./frontend` (working dir `/app` — templates and static files are loaded by relative path, so don't change the working dir).
- **User:** numeric `USER 10001:10001` (`appuser`/`appgroup`, created with these fixed ids), so Kubernetes `runAsNonRoot` can verify it (matches the chart's `runAsUser`). Build args `SERVICE_VERSION` (default `dev`) and `SERVICE_COMMIT` (default `unknown`) set the OCI labels `version`/`revision` (plus `source`, `licenses=Apache-2.0`).
- **Base images** are pinned by tag and digest (the multi-arch index digests of 2026-11-22, `Dockerfile`); Dependabot's docker ecosystem refreshes them.
- **EXPOSE:** `8080`.
- **Docker `HEALTHCHECK`:** exec form `["/app/frontend", "healthcheck"]` (no shell, no `wget`): the subcommand does `GET http://127.0.0.1:${PORT:-8080}${BASE_URL}/healthz` with a 3 s limit and exits 0 only for `200`.
- **Image size:** 76.3 MB for `platform-demo/frontend:local` (`docker images`, built 2026-11-22 from the final tree; `static/` alone is 17 MB because of the product photos).
- The repo root may contain a compiled `server` binary (git-ignored) — it is not used by the image.
