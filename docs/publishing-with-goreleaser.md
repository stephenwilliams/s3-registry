# Publishing a tool with goreleaser

This guide shows how a tool's own release pipeline pushes its artifacts into an
`s3reg` registry. Your project's [goreleaser](https://goreleaser.com) build
produces a `dist/` directory of archives; `s3reg publish --dist` then uploads
every archive and updates that tool's `index.json` in one pass. Tool name and
version are supplied explicitly; `os`/`arch` are inferred from each archive
filename.

See the [S3 layout](../README.md#s3-layout) in the README for how the uploaded
keys and index are structured.

## Prerequisites

- **The `s3reg` binary on the release runner.** Build it from source
  (`go build -o s3reg ./cmd/s3reg` from a checkout of this repo) or install a
  released `s3reg` from the registry itself.
- **AWS credentials with write access to the bucket**, from the standard AWS
  environment / shared-config / instance-role chain.
- **`S3REG_BUCKET`** set to the target bucket. Publish talks to S3 directly, so
  it does **not** need `S3REG_API_TOKEN` — that token only gates the `serve`
  proxy.

For an S3-compatible store (MinIO, LocalStack), add `--endpoint` (or
`AWS_ENDPOINT_URL`) and `--s3-path-style` (or `S3REG_S3_PATH_STYLE=true`).

| Setting | Flag | Env | Required |
| ------- | ---- | --- | -------- |
| Bucket | `--bucket` | `S3REG_BUCKET` | yes |
| Region | `--region` | `S3REG_REGION` | no (AWS default chain) |
| Endpoint override | `--endpoint` | `AWS_ENDPOINT_URL` | no |
| Path-style addressing | `--s3-path-style` | `S3REG_S3_PATH_STYLE=true` | no |

## Naming archives so os/arch is detected

This is the most failure-prone part. `s3reg` reads the `dist/` directory listing
directly — it does not parse `artifacts.json` — and infers `os`/`arch` from each
archive filename. A file is only considered when its extension is one of
`.tar.gz`, `.tgz`, `.tar.xz`, `.tar.bz2`, `.zip`, `.tar`; anything else (checksum
files, raw binaries, `.sig`) is ignored.

Matching is case-insensitive. Each of the `os` and `arch` tokens must appear as
a **whole token** bounded by `_`, `-`, `.`, or a string edge — so `mac` inside a
tool name like `macaroni` will not falsely match.

| Detected `os` | Filename tokens |
| ------------- | --------------- |
| `darwin`  | `darwin`, `macos`, `mac` |
| `linux`   | `linux` |
| `windows` | `windows`, `win` |

| Detected `arch` | Filename tokens |
| --------------- | --------------- |
| `amd64` | `amd64`, `x86_64`, `x8664` |
| `arm64` | `arm64`, `aarch64` |

goreleaser's default casing (`Darwin`, `x86_64`) resolves fine. The name
template below produces resolvable names for every target:

```yaml
archives:
  - id: mytool
    formats: [tar.gz]
    name_template: >-
      {{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}
```

This yields, for example, `mytool_1.2.3_darwin_arm64.tar.gz` and
`mytool_1.2.3_linux_amd64.tar.gz`. An archive whose os or arch cannot be
resolved is skipped with a `skip <name>: cannot infer os/arch` line on stderr,
not an error — but if **no** archive resolves, publish fails with
`no matching archives found`.

## A complete `.goreleaser.yaml`

The `checksum` block makes `dist/checksums.txt`; passing it to `--checksums`
turns any hash disagreement into a hard failure. The `release.hooks.after` step
runs the publish once the archives and checksums exist.

```yaml
version: 2

builds:
  - id: mytool
    main: ./cmd/mytool
    binary: mytool
    env: [CGO_ENABLED=0]
    goos: [linux, darwin]
    goarch: [amd64, arm64]

archives:
  - id: mytool
    formats: [tar.gz]
    name_template: >-
      {{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}

checksum:
  name_template: checksums.txt

release:
  hooks:
    after:
      - >-
        s3reg publish --tool mytool --version {{ .Version }}
        --dist ./dist --checksums ./dist/checksums.txt
```

The hook inherits the process environment, so `S3REG_BUCKET` and AWS credentials
must be set when goreleaser runs.

Test the whole flow locally without writing to S3:

```sh
goreleaser release --snapshot --clean   # build dist/ without tagging
s3reg publish --tool mytool --version 0.0.1-next \
  --dist ./dist --checksums ./dist/checksums.txt --dry-run
```

`--dry-run` prints each intended upload and the index change, then exits without
touching the bucket.

## CI: GitHub Actions

Tag-triggered release. This builds `s3reg` from its repo, authenticates to AWS
with OIDC, exports `S3REG_BUCKET`, and lets the goreleaser after-hook publish.

```yaml
name: release
on:
  push:
    tags: ['v*']

permissions:
  contents: write   # goreleaser creates the GitHub release
  id-token: write   # OIDC for AWS

jobs:
  release:
    runs-on: ubuntu-latest
    env:
      S3REG_BUCKET: my-artifacts
      S3REG_REGION: us-east-1
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - uses: actions/setup-go@v5
        with:
          go-version: stable

      - name: Install s3reg
        run: go install github.com/stephenwilliams/s3-registry/cmd/s3reg@latest

      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: arn:aws:iam::123456789012:role/s3reg-publisher
          aws-region: us-east-1

      - uses: goreleaser/goreleaser-action@v6
        with:
          version: latest
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

Prefer to keep the publish out of goreleaser? Drop the `release.hooks.after`
block and run publish as an explicit step after the goreleaser step:

```yaml
      - name: Publish to registry
        run: |
          s3reg publish --tool mytool \
            --version "${GITHUB_REF_NAME#v}" \
            --dist ./dist --checksums ./dist/checksums.txt
```

## CI: GitLab CI

Tag-pipeline `release` job. AWS credentials and `S3REG_BUCKET` come from
project CI/CD variables.

```yaml
release:
  image: golang:1.23
  rules:
    - if: '$CI_COMMIT_TAG'
  variables:
    S3REG_BUCKET: my-artifacts
    S3REG_REGION: us-east-1
    # AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY set as protected CI/CD variables
  script:
    - go install github.com/stephenwilliams/s3-registry/cmd/s3reg@latest
    - curl -sfL https://goreleaser.com/static/run | bash -s -- release --clean
```

With the goreleaser after-hook in place, that `release --clean` publishes. To
publish explicitly instead, drop the hook and add:

```yaml
    - s3reg publish --tool mytool --version "${CI_COMMIT_TAG#v}"
        --dist ./dist --checksums ./dist/checksums.txt
```

## Verify the publish

After a release, confirm the registry sees it:

```sh
s3reg ls --tool mytool                       # versions / os-arch / size / sha
s3reg verify --tool mytool --version 1.2.3   # re-hash every artifact, non-zero on mismatch
```

Through the proxy, resolve a range and fetch an artifact URL:

```sh
curl "$S3REG_URL/tools/mytool/resolve?range=^1.2"
curl "$S3REG_URL/tools/mytool/versions/1.2.3/artifact?os=darwin&arch=arm64"
```

See the README [`ls`](../README.md#ls), [`verify`](../README.md#verify), and
[HTTP endpoints](../README.md#http-endpoints) sections for the full output
shapes.

## Troubleshooting

| Symptom | Cause and fix |
| ------- | ------------- |
| `no matching archives found in ./dist` | No file in `dist/` has a recognized archive extension, or none resolved to an os **and** arch. Check `formats` and `name_template`. |
| `skip <name>: cannot infer os/arch` (on stderr, file not uploaded) | That archive's filename lacks a bounded os or arch token. Use the name template above; avoid tokens buried inside the tool name. |
| `checksum mismatch for <name>` / `no checksum entry for <name>` | The `dist/` and `checksums.txt` are out of sync — regenerate the release; don't hand-edit `dist/`. |
| `warning: checksum entry <name> has no matching uploaded archive` | Informational: `checksums.txt` lists a file that wasn't an uploaded archive (e.g. a raw binary). Safe to ignore. |
| `bucket is required (set --bucket or S3REG_BUCKET)` | `S3REG_BUCKET` not in the hook/job environment. |
| `index write for <tool> failed after 5 attempts (contention)` | Concurrent publishers kept losing the conditional write. Retry; serialize releases for the same tool. |
| AWS `AccessDenied` / credential errors | The runner's role lacks `s3:PutObject`/`s3:GetObject` on the bucket, or credentials aren't wired into the environment. |
