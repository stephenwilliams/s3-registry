package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/stephenwilliams/s3-registry/internal/index"
)

func newRmCmd() *cobra.Command {
	var tool, version, osName, arch string
	cmd := &cobra.Command{
		Use:   "rm",
		Short: "Remove an artifact or a whole version from a tool",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRm(cmd.Context(), tool, version, osName, arch)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&tool, "tool", "", "tool name (required)")
	fl.StringVar(&version, "version", "", "version X.Y.Z (required)")
	fl.StringVar(&osName, "os", "", "GOOS; omit with --arch to remove the whole version")
	fl.StringVar(&arch, "arch", "", "GOARCH")
	_ = cmd.MarkFlagRequired("tool")
	_ = cmd.MarkFlagRequired("version")
	return cmd
}

func runRm(ctx context.Context, tool, version, osName, arch string) error {
	if (osName == "") != (arch == "") {
		return fmt.Errorf("--os and --arch must be given together")
	}
	st, err := newStore(ctx)
	if err != nil {
		return err
	}

	// Collect the object keys to delete from the current index before mutating.
	idx, _, err := st.GetIndex(ctx, tool)
	if err != nil {
		return err
	}
	osArch := ""
	if osName != "" {
		osArch = osName + "-" + arch
	}
	var keys []string
	for _, v := range idx.Versions {
		if v.Version != version {
			continue
		}
		for oa, a := range v.Artifacts {
			if osArch == "" || oa == osArch {
				keys = append(keys, a.Key)
			}
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("nothing to remove for %s %s %s", tool, version, osArch)
	}

	// Update the index first. If it fails, leave the objects intact — orphaned
	// objects are safer than an index pointing at deleted keys.
	if err := updateIndex(ctx, st, tool, func(i *index.Index) {
		i.Remove(version, osArch)
	}); err != nil {
		return err
	}

	for _, k := range keys {
		if err := st.DeleteObject(ctx, k); err != nil {
			return err
		}
		fmt.Printf("deleted %s\n", k)
	}
	return nil
}
