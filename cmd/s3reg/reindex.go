package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stephenwilliams/s3-registry/internal/index"
	"github.com/stephenwilliams/s3-registry/internal/store"
)

func newReindexCmd() *cobra.Command {
	var tool string
	cmd := &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild a tool's index.json purely from the S3 objects",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReindex(cmd.Context(), tool)
		},
	}
	cmd.Flags().StringVar(&tool, "tool", "", "tool name (required)")
	_ = cmd.MarkFlagRequired("tool")
	return cmd
}

func runReindex(ctx context.Context, tool string) error {
	st, err := newStore(ctx)
	if err != nil {
		return err
	}

	objs, err := st.ListObjects(ctx, tool+"/")
	if err != nil {
		return err
	}

	// A version keeps the publish time already in the index. Without one, the
	// earliest LastModified of its artifacts stands in for it.
	created := map[string]time.Time{}
	current, _, err := st.GetIndex(ctx, tool)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if current != nil {
		for _, r := range current.Releases() {
			if !r.Created.IsZero() {
				created[r.Version] = r.Created
			}
		}
	}
	earliest := map[string]time.Time{}

	rebuilt := index.New(tool)
	for _, obj := range objs {
		version, osArch, ok := parseArtifactKey(tool, obj.Key)
		if !ok {
			continue
		}
		sum, size, err := hashObject(ctx, st, obj.Key)
		if err != nil {
			return err
		}
		rebuilt.Upsert(version, osArch, index.Artifact{Key: obj.Key, SHA256: sum, Size: size})
		if e, ok := earliest[version]; !obj.LastModified.IsZero() && (!ok || obj.LastModified.Before(e)) {
			earliest[version] = obj.LastModified
		}
		fmt.Printf("indexed %s\n", obj.Key)
	}
	for _, r := range rebuilt.Releases() {
		t, ok := created[r.Version]
		if !ok {
			t = earliest[r.Version]
		}
		rebuilt.SetCreated(r.Version, t)
	}
	rebuilt.SortVersions()

	// Overwrite conditionally against the current index's ETag, retrying on a
	// concurrent change.
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		_, etag, gerr := st.GetIndex(ctx, tool)
		if errors.Is(gerr, store.ErrNotFound) {
			etag = ""
		} else if gerr != nil {
			return gerr
		}
		perr := st.PutIndex(ctx, tool, rebuilt, etag)
		if errors.Is(perr, store.ErrPreconditionFailed) {
			continue
		}
		if perr != nil {
			return perr
		}
		fmt.Printf("wrote index for %s (%d versions)\n", tool, len(rebuilt.Versions))
		return nil
	}
	return fmt.Errorf("index write for %s failed after %d attempts (contention)", tool, maxAttempts)
}

// parseArtifactKey splits "<tool>/<version>/<os>-<arch>/<filename>". It returns
// ok=false for index.json and any key that does not match the layout.
func parseArtifactKey(tool, key string) (version, osArch string, ok bool) {
	rest := strings.TrimPrefix(key, tool+"/")
	if rest == key || rest == "index.json" {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", "", false
	}
	if !strings.Contains(parts[1], "-") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func hashObject(ctx context.Context, st *store.Store, key string) (string, int64, error) {
	body, _, err := st.GetObject(ctx, key)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = body.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, body)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
