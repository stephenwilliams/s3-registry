package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/stephenwilliams/s3-registry/internal/index"
	"github.com/stephenwilliams/s3-registry/internal/store"
)

type publishFlags struct {
	tool      string
	version   string
	os        string
	arch      string
	file      string
	dist      string
	checksums string
	dryRun    bool
}

func newPublishCmd() *cobra.Command {
	var f publishFlags
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Publish artifacts and update a tool's index",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPublish(cmd.Context(), f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.tool, "tool", "", "tool name (required)")
	fl.StringVar(&f.version, "version", "", "version X.Y.Z (required)")
	fl.StringVar(&f.os, "os", "", "GOOS (single mode)")
	fl.StringVar(&f.arch, "arch", "", "GOARCH (single mode)")
	fl.StringVar(&f.file, "file", "", "artifact file (single mode)")
	fl.StringVar(&f.dist, "dist", "", "dist directory (dist mode)")
	fl.StringVar(&f.checksums, "checksums", "", "checksums.txt (dist mode)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print intended actions without writing")
	_ = cmd.MarkFlagRequired("tool")
	_ = cmd.MarkFlagRequired("version")
	return cmd
}

func runPublish(ctx context.Context, f publishFlags) error {
	if f.file != "" && f.dist != "" {
		return fmt.Errorf("--file and --dist are mutually exclusive")
	}
	if f.file == "" && f.dist == "" {
		return fmt.Errorf("one of --file or --dist is required")
	}
	if f.file != "" {
		return publishSingle(ctx, f)
	}
	return publishDist(ctx, f)
}

// stagedArtifact is one artifact ready to upload plus its index entry.
type stagedArtifact struct {
	osArch string
	key    string
	path   string
	art    index.Artifact
}

func publishSingle(ctx context.Context, f publishFlags) error {
	if f.os == "" || f.arch == "" {
		return fmt.Errorf("--os and --arch are required in single mode")
	}
	sum, size, err := hashFile(f.file)
	if err != nil {
		return err
	}
	osArch := f.os + "-" + f.arch
	key := fmt.Sprintf("%s/%s/%s/%s", f.tool, f.version, osArch, filepath.Base(f.file))
	staged := []stagedArtifact{{
		osArch: osArch,
		key:    key,
		path:   f.file,
		art:    index.Artifact{Key: key, SHA256: sum, Size: size},
	}}
	return uploadAndIndex(ctx, f, staged)
}

func publishDist(ctx context.Context, f publishFlags) error {
	var checksums map[string]string
	if f.checksums != "" {
		cf, err := os.Open(f.checksums)
		if err != nil {
			return fmt.Errorf("open checksums: %w", err)
		}
		checksums, err = store.ParseChecksums(cf)
		_ = cf.Close()
		if err != nil {
			return err
		}
	}

	entries, err := os.ReadDir(f.dist)
	if err != nil {
		return fmt.Errorf("read dist dir: %w", err)
	}

	var staged []stagedArtifact
	matched := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !store.IsArchive(e.Name()) {
			continue
		}
		goos, goarch, ok := store.InferOSArch(e.Name())
		if !ok {
			fmt.Fprintf(os.Stderr, "skip %s: cannot infer os/arch\n", e.Name())
			continue
		}
		path := filepath.Join(f.dist, e.Name())
		computed, size, herr := hashFile(path)
		if herr != nil {
			return herr
		}
		if checksums != nil {
			want, ok := checksums[e.Name()]
			if !ok {
				return fmt.Errorf("no checksum entry for %s in checksums file", e.Name())
			}
			if want != computed {
				return fmt.Errorf("checksum mismatch for %s: checksums.txt=%s computed=%s", e.Name(), want, computed)
			}
			matched[e.Name()] = true
		}
		osArch := goos + "-" + goarch
		key := fmt.Sprintf("%s/%s/%s/%s", f.tool, f.version, osArch, e.Name())
		staged = append(staged, stagedArtifact{
			osArch: osArch,
			key:    key,
			path:   path,
			art:    index.Artifact{Key: key, SHA256: computed, Size: size},
		})
	}
	if len(staged) == 0 {
		return fmt.Errorf("no matching archives found in %s", f.dist)
	}
	for name := range checksums {
		if !matched[name] {
			fmt.Fprintf(os.Stderr, "warning: checksum entry %s has no matching uploaded archive\n", name)
		}
	}
	return uploadAndIndex(ctx, f, staged)
}

// uploadAndIndex uploads staged artifacts then applies them to the index under
// a conditional write with retry on precondition failure.
func uploadAndIndex(ctx context.Context, f publishFlags, staged []stagedArtifact) error {
	if f.dryRun {
		for _, s := range staged {
			fmt.Printf("would upload %s (%d bytes, sha256 %s)\n", s.key, s.art.Size, s.art.SHA256)
		}
		fmt.Printf("would update index for %s version %s (%d artifact(s))\n", f.tool, f.version, len(staged))
		return nil
	}

	st, err := newStore(ctx)
	if err != nil {
		return err
	}

	for _, s := range staged {
		file, err := os.Open(s.path)
		if err != nil {
			return err
		}
		err = st.PutArtifact(ctx, s.key, file)
		_ = file.Close()
		if err != nil {
			return err
		}
		fmt.Printf("uploaded %s\n", s.key)
	}

	return updateIndex(ctx, st, f.tool, func(idx *index.Index) {
		idx.Name = f.tool
		for _, s := range staged {
			idx.Upsert(f.version, s.osArch, s.art)
		}
	})
}

// updateIndex performs a read-modify-write on the tool index with a conditional
// If-Match write, retrying on precondition failure.
func updateIndex(ctx context.Context, st *store.Store, tool string, mutate func(*index.Index)) error {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		idx, etag, err := st.GetIndex(ctx, tool)
		if errors.Is(err, store.ErrNotFound) {
			idx = index.New(tool)
			etag = ""
		} else if err != nil {
			return err
		}
		// Upsert/Remove already re-sort and stamp Updated, so no extra work here.
		mutate(idx)

		err = st.PutIndex(ctx, tool, idx, etag)
		if errors.Is(err, store.ErrPreconditionFailed) {
			continue
		}
		return err
	}
	return fmt.Errorf("index write for %s failed after %d attempts (contention)", tool, maxAttempts)
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
