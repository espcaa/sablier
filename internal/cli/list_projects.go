package cli

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/espcaa/sablier/internal/credit"
	"github.com/espcaa/sablier/internal/paths"
	"github.com/espcaa/sablier/internal/store"
	"github.com/spf13/cobra"
)

func newListProjectsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list-projects",
		Short: "list projects on this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := paths.DataDir()
			if err != nil {
				return err
			}
			s, err := store.Open(filepath.Join(dir, "sablier.db"))
			if err != nil {
				return err
			}
			defer s.Close()

			projects, err := s.ListProjects(cmd.Context())
			if err != nil {
				return err
			}
			now := time.Now()
			from := now.Add(-7 * 24 * time.Hour)
			rules := credit.Default
			events, err := s.Between(cmd.Context(), from.Add(-max(rules.AgentCap, rules.InputCap)), now.Add(time.Millisecond))
			if err != nil {
				return err
			}
			times := credit.ComputeBetween(events, rules, from, now).ByProject
			sort.SliceStable(projects, func(i, j int) bool {
				return times[projects[i]] > times[projects[j]]
			})
			out := cmd.OutOrStdout()
			for _, id := range projects {
				if _, err := fmt.Fprintf(out, "%-8s %s\n", credit.Format(times[id]), id); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
