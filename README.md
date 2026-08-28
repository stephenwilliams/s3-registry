# s3reg

`s3reg` is a single Go binary that is both:

1. **An HTTP proxy** in front of a private S3 bucket of versioned tool
   artifacts. It resolves semver ranges and `latest`, then hands out
   short-lived presigned download URLs. It never redirects; it returns the URL
   as JSON so callers stay in control.
2. **A CLI** to manage the per-tool index in that bucket — publish, list,
   remove, reindex, and verify artifacts.

Every HTTP endpoint is a typed [huma](https://huma.rocks) operation mounted on
[chi](https://github.com/go-chi/chi), so requests and responses are validated
and an OpenAPI spec is served at `/openapi.json` with a docs UI at `/docs`.

## S3 layout

The bucket is the source of truth.

```
s3://<bucket>/<tool>/index.json
s3://<bucket>/<tool>/<version>/<os>-<arch>/<filename>
```

`os` is a Go `GOOS` (`darwin`, `linux`, `windows`) and `arch` a `GOARCH`
(`amd64`, `arm64`). Tool discovery is a `ListObjectsV2` with `Delimiter="/"`
over the bucket root, reading the top-level common prefixes.

Each tool's `index.json`:

```json
{
  "name": "mytool",
  "updated": "2026-08-27T00:00:00Z",
  "versions": [
    {
      "version": "1.2.3",
      "artifacts": {
        "darwin-arm64": { "key": "mytool/1.2.3/darwin-arm64/mytool.tar.gz", "sha256": "<hex>", "size": 12345 },
        "linux-amd64":  { "key": "mytool/1.2.3/linux-amd64/mytool.tar.gz",  "sha256": "<hex>", "size": 12345 }
      }
    }
  ]
}
```

Versions are stored ascending by semver. Index writes are conditional
(`If-Match` on the object ETag) so concurrent publishers cannot clobber each
other; a losing writer reloads and retries.

## Environment variables

| Variable                  | Used by     | Default              | Purpose |
| ------------------------- | ----------- | -------------------- | ------- |
| `S3REG_BUCKET`            | all S3 ops  | — (required)         | Target bucket |
| `S3REG_REGION`            | all S3 ops  | AWS default chain    | AWS region |
| `S3REG_ADDR`              | `serve`     | `:8080`              | Listen address |
| `S3REG_API_TOKEN`         | `serve`     | unset (open)         | If set, `/tools/**` requires `Authorization: Bearer <token>` |
| `S3REG_PRESIGN_TTL`       | `serve`     | `5m`                 | Presigned URL lifetime |
| `S3REG_CACHE_TTL`         | `serve`     | `60s`                | In-memory index cache TTL |
| `LIVENESS_DEADLINE`       | `serve`     | `200ms`              | Liveness budget |
| `READINESS_DEADLINE`      | `serve`     | `500ms`              | Readiness probe budget |
| `STATUS_DEADLINE`         | `serve`     | `1s`                 | Status probe hard deadline |
| `STATUS_CACHE_TTL`        | `serve`     | `1s`                 | Status result cache TTL |
| `READINESS_STARTUP_GRACE` | `serve`     | `15s`                | Startup window during which readiness returns 503 |

AWS credentials come from the standard environment / shared config / instance
role chain.

## CLI

```
s3reg serve    [--addr :8080]
s3reg publish  ...            # single or dist mode
s3reg ls       [--tool NAME]
s3reg rm       --tool NAME --version X.Y.Z [--os OS --arch ARCH]
s3reg reindex  --tool NAME
s3reg verify   --tool NAME [--version X.Y.Z]
```

### serve

```sh
S3REG_BUCKET=my-artifacts s3reg serve --addr :8080
```

### publish (single artifact)

Uploads one file, computes its sha256 and size, then does a conditional
read-modify-write of the index (retrying on a 412).

```sh
s3reg publish --tool mytool --version 1.2.3 \
  --os darwin --arch arm64 --file ./mytool.tar.gz
```

Add `--dry-run` to print the intended uploads and index change without writing.

### publish (dist directory)

Walks a goreleaser-style `dist` directory, infers `os`/`arch` from each archive
filename, and uploads them all in one pass. With `--checksums`, a recomputed
hash that disagrees with `checksums.txt` is a hard error.

```sh
s3reg publish --tool mytool --version 1.2.3 \
  --dist ./dist --checksums ./dist/checksums.txt
```

Filename tokens understood: `Darwin`/`darwin`/`macos`, `Linux`/`linux`,
`Windows`/`windows`; `x86_64`/`amd64`, `arm64`/`aarch64`.

To wire this into a tool's own release pipeline, see
[Publishing a tool with goreleaser](docs/publishing-with-goreleaser.md).

### ls

```sh
s3reg ls                 # list tool names
s3reg ls --tool mytool   # version / os-arch / size / short sha
```

### rm

```sh
s3reg rm --tool mytool --version 1.2.3                       # whole version
s3reg rm --tool mytool --version 1.2.3 --os linux --arch amd64  # one artifact
```

Deletes the object(s) and prunes the index with a conditional write.

### reindex

Rebuilds `index.json` purely from the objects under the tool prefix, streaming
each to recompute its sha256. Overwrites the existing index.

```sh
s3reg reindex --tool mytool
```

### verify

Streams each indexed artifact and checks its sha256. Exits non-zero, listing
mismatches, if any disagree.

```sh
s3reg verify --tool mytool --version 1.2.3
```

## HTTP endpoints

| Method & path | Returns |
| ------------- | ------- |
| `GET /tools` | `{"tools":["a","b"]}` |
| `GET /tools/{tool}/versions` | `{"versions":["1.0.0","1.2.3"]}` ascending |
| `GET /tools/{tool}/resolve?range=^1.2` | `{"version":"1.2.9"}` (empty/`latest` → highest) |
| `GET /tools/{tool}/versions/{version}/artifact?os=darwin&arch=arm64` | `{"url","sha256","size","version"}` — `version` may be concrete or a range/`latest` |
| `GET /openapi.json`, `GET /docs` | OpenAPI spec and docs UI |

Error codes: `404` for an unknown tool or missing os-arch, `422` when no
version satisfies a range, `401` when a token is required and absent/wrong.

### Health endpoints

Neutral health checks under `/-/health/`. The only downstream dependency is the
S3 bucket (`HeadBucket`).

| Path | Behavior |
| ---- | -------- |
| `GET /-/health/live` | Process only, no S3 call. `200 {"status":"ok"}`; `503` while draining. |
| `GET /-/health/ready` | `503` during startup grace and after `SIGTERM` (drain); otherwise probes S3 within the readiness deadline. |
| `GET /-/health/status` | Build metadata, dependency results, and counters, cached for `STATUS_CACHE_TTL`; probes run in parallel under `STATUS_DEADLINE` and yield `206` with `"incomplete":true` if it trips. |

Deprecated aliases redirect for one release: `/healthz` → `/-/health/live`,
`/readyz` → `/-/health/ready`.

On `SIGTERM` the server sets the drain flag (readiness → 503) and then does a
graceful `http.Server.Shutdown`.

## goreleaser walkthrough

### 1. Injecting build metadata into this binary

`/-/health/status` reports the real release because the linker sets the
`internal/buildinfo` vars. The `.goreleaser.yaml` ldflags do that:

```yaml
builds:
  - id: s3reg
    main: ./cmd/s3reg
    env: [CGO_ENABLED=0]
    ldflags:
      - -s -w
      - -X github.com/stephenwilliams/s3-registry/internal/buildinfo.Version={{ .Version }}
      - -X github.com/stephenwilliams/s3-registry/internal/buildinfo.Commit={{ .Commit }}
      - -X github.com/stephenwilliams/s3-registry/internal/buildinfo.BuiltAt={{ .Date }}
      - -X github.com/stephenwilliams/s3-registry/internal/buildinfo.GitTag={{ .Tag }}
```

### 2. A different project publishing into this registry

Any project whose own goreleaser build produces a `dist/` can push its release
into the registry with an after-release hook that runs `s3reg publish --dist`.
The full walkthrough — archive naming, a complete `.goreleaser.yaml`, and
GitHub Actions / GitLab CI wiring — is in
[Publishing a tool with goreleaser](docs/publishing-with-goreleaser.md).

## Consumer usage (mise plugin)

A consuming project pins a tool through the registry in its `mise.toml`:

```toml
[tools]
"s3reg:mytool" = "^1.2"
```

with the registry endpoint and token in the environment:

```sh
export S3REG_URL="https://registry.example.com"
export S3REG_TOKEN="<bearer token>"
mise plugin install s3reg <plugin-repo-url>
mise install
```

The plugin calls `GET /tools/mytool/resolve` and then the `artifact` endpoint
for the current `os`/`arch`, downloading via the returned presigned URL.

## Development

```sh
mise run build   # go build -o bin/s3reg ./cmd/s3reg
mise run test    # go test -race -cover ./...
mise run lint    # golangci-lint run
mise run run     # go run ./cmd/s3reg serve
```
