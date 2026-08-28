package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/stephenwilliams/s3-registry/internal/buildinfo"
	"github.com/stephenwilliams/s3-registry/internal/store"
)

// global flag-backed values, defaulted from env in init.
var (
	flagBucket string
	flagRegion string
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "s3reg",
		Short:         "Proxy and manager for an S3-backed tool artifact registry",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s (commit %s, built %s)", buildinfo.Version, buildinfo.Commit, buildinfo.BuiltAt),
	}
	root.PersistentFlags().StringVar(&flagBucket, "bucket", envOr("S3REG_BUCKET", ""), "S3 bucket (env S3REG_BUCKET)")
	root.PersistentFlags().StringVar(&flagRegion, "region", envOr("S3REG_REGION", ""), "AWS region (env S3REG_REGION)")

	root.AddCommand(
		newServeCmd(),
		newPublishCmd(),
		newLsCmd(),
		newRmCmd(),
		newReindexCmd(),
		newVerifyCmd(),
	)
	return root
}

// newStore builds a store from the resolved bucket/region flags.
func newStore(ctx context.Context) (*store.Store, error) {
	if flagBucket == "" {
		return nil, fmt.Errorf("bucket is required (set --bucket or S3REG_BUCKET)")
	}
	return store.New(ctx, store.Config{Bucket: flagBucket, Region: flagRegion})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
