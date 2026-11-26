package cli

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/espcaa/sablier/internal/update"
	"github.com/espcaa/sablier/internal/version"
	"github.com/spf13/cobra"
)

func newUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "update [version]",
		Short:   "update sablier to the latest release, or to a given one",
		Example: "  sablier update\n  sablier update 0.2.0-alpha.4",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			current := version.Version
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}

			c := http.DefaultClient
			var tag string
			if len(args) == 1 {
				tag = "v" + strings.TrimPrefix(args[0], "v")
				if tag == current {
					fmt.Fprintf(out, "sablier %s is already installed\n", current)
					return nil
				}
			} else {
				if tag, err = update.Latest(cmd.Context(), c); err != nil {
					return err
				}
				if !update.Newer(tag, current) {
					fmt.Fprintf(out, "sablier %s is the latest\n", current)
					return nil
				}
			}

			base := os.Getenv("SABLIER_RELEASE_URL")
			if base == "" {
				base = update.ReleaseURL(tag)
			}
			fmt.Fprintf(out, "downloading sablier %s\n", tag)
			bin, err := update.Download(cmd.Context(), c, base)
			if err != nil && len(args) == 1 {
				return fmt.Errorf("can't install %s, check that the release exists: %w", tag, err)
			}
			if err != nil {
				return err
			}
			if err := update.Replace(exe, bin); err != nil {
				return err
			}
			fmt.Fprintf(out, "updated %s from %s to %s\n", exe, current, tag)
			return nil
		},
	}
}
