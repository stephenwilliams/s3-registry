package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newVerifyCmd() *cobra.Command {
	var tool, version string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify stored artifacts match the sha256 recorded in the index",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVerify(cmd.Context(), tool, version)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&tool, "tool", "", "tool name (required)")
	fl.StringVar(&version, "version", "", "limit to one version")
	_ = cmd.MarkFlagRequired("tool")
	return cmd
}

func runVerify(ctx context.Context, tool, version string) error {
	st, err := newStore(ctx)
	if err != nil {
		return err
	}
	idx, _, err := st.GetIndex(ctx, tool)
	if err != nil {
		return err
	}

	var mismatches []string
	checked := 0
	for _, v := range idx.Versions {
		if version != "" && v.Version != version {
			continue
		}
		for osArch, a := range v.Artifacts {
			sum, _, herr := hashObject(ctx, st, a.Key)
			if herr != nil {
				mismatches = append(mismatches, fmt.Sprintf("%s %s: read error: %v", v.Version, osArch, herr))
				continue
			}
			checked++
			if sum != a.SHA256 {
				mismatches = append(mismatches, fmt.Sprintf("%s %s: index=%s actual=%s", v.Version, osArch, a.SHA256, sum))
			} else {
				fmt.Printf("ok %s %s\n", v.Version, osArch)
			}
		}
	}

	if len(mismatches) > 0 {
		for _, m := range mismatches {
			fmt.Printf("MISMATCH %s\n", m)
		}
		return fmt.Errorf("%d artifact(s) failed verification", len(mismatches))
	}
	fmt.Printf("verified %d artifact(s)\n", checked)
	return nil
}
