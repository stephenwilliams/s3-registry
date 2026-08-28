package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newLsCmd() *cobra.Command {
	var tool string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List tools, or a tool's versions and artifacts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLs(cmd.Context(), tool)
		},
	}
	cmd.Flags().StringVar(&tool, "tool", "", "tool name; omit to list all tools")
	return cmd
}

func runLs(ctx context.Context, tool string) error {
	st, err := newStore(ctx)
	if err != nil {
		return err
	}

	if tool == "" {
		tools, err := st.ListTools(ctx)
		if err != nil {
			return err
		}
		sort.Strings(tools)
		for _, t := range tools {
			fmt.Println(t)
		}
		return nil
	}

	idx, _, err := st.GetIndex(ctx, tool)
	if err != nil {
		return err
	}
	// Stored indexes are written sorted, so reads print in order without
	// mutating the index here.

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "VERSION\tOS-ARCH\tSIZE\tSHA256")
	for _, v := range idx.Versions {
		osArches := make([]string, 0, len(v.Artifacts))
		for oa := range v.Artifacts {
			osArches = append(osArches, oa)
		}
		sort.Strings(osArches)
		for _, oa := range osArches {
			a := v.Artifacts[oa]
			short := a.SHA256
			if len(short) > 12 {
				short = short[:12]
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", v.Version, oa, a.Size, short)
		}
	}
	return tw.Flush()
}
