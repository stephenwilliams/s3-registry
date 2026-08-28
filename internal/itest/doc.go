// Package itest provides a MinIO-backed harness shared by the integration tests
// across the store, server, and CLI packages. The harness itself lives in
// minio.go behind the "integration" build tag; this file keeps the package
// buildable (and skippable) under the default hermetic build.
package itest
