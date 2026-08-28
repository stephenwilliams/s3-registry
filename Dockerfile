FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILT_AT=unknown
ARG GIT_TAG=unknown
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w \
      -X github.com/stephenwilliams/s3-registry/internal/buildinfo.Version=${VERSION} \
      -X github.com/stephenwilliams/s3-registry/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/stephenwilliams/s3-registry/internal/buildinfo.BuiltAt=${BUILT_AT} \
      -X github.com/stephenwilliams/s3-registry/internal/buildinfo.GitTag=${GIT_TAG}" \
    -o /out/s3reg ./cmd/s3reg

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/s3reg /s3reg
EXPOSE 8080
ENTRYPOINT ["/s3reg"]
CMD ["serve"]
