# Container image

Multi-stage [`Dockerfile`](https://github.com/platform-boutique/app-frontend/tree/main/Dockerfile):

| Stage | Base | What happens |
|---|---|---|
| builder | `golang:1.26-alpine` | `go mod download`, `go build -ldflags="-s -w" -o /bin/frontend ./cmd/server` (`CGO_ENABLED=0`; target arch = builder's arch) |
| runtime | `alpine:3.24` | binary + `templates/` + `static/` under `/app`; adds `ca-certificates` |

- **Entrypoint:** `./frontend` (working dir `/app` — templates and static files are loaded by relative path, so don't change the working dir).
- **User:** `appuser` — UID `100`, GID `101` (verified). Set `runAsUser: 100` for `runAsNonRoot`.
- **EXPOSE:** `8080`.
- **Docker `HEALTHCHECK`:** exec form `["/app/frontend", "healthcheck"]` (no shell, no `wget`): the subcommand does `GET http://127.0.0.1:${PORT:-8080}${BASE_URL}/healthz` with a 3 s limit and exits 0 only for `200`.
- **Image size:** 76.7 MB for `platform-demo/frontend:local` (`docker images`, the local compose build of this branch; `static/` alone is 17 MB because of the product photos). The older ~15 MB figure predates them.
- The repo root may contain a compiled `server` binary (git-ignored) — it is not used by the image.
