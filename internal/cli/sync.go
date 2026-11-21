package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/config"
	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/paths"
	"github.com/espcaa/sablier/internal/project"
	"github.com/espcaa/sablier/internal/store"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "upload tracked events to the server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			if c.APIKey == "" {
				return errors.New("not signed in, run sablier login first")
			}
			dir, err := paths.DataDir()
			if err != nil {
				return err
			}
			s, err := store.Open(filepath.Join(dir, "sablier.db"))
			if err != nil {
				return err
			}
			defer s.Close()
			if all {
				if err := s.MarkAllPending(cmd.Context()); err != nil {
					return err
				}
			}
			return syncEvents(cmd.Context(), s, api.New(c.APIURL, c.APIKey), cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "send every event on this machine again, not only new ones (the server skips what it has)")
	return cmd
}

func syncEvents(ctx context.Context, s *store.Store, client *api.Client, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	total, err := upload(ctx, s, client, out)
	if err != nil {
		return err
	}

	switch {
	case total.Accepted+total.Duplicates == 0:
		fmt.Fprintln(out, "nothing to sync")
	case total.Duplicates > 0:
		fmt.Fprintf(out, "synced %d events (%d were already there)\n", total.Accepted, total.Duplicates)
	default:
		fmt.Fprintf(out, "synced %d events\n", total.Accepted)
	}
	return nil
}

// sends every pending event in batches, marking each batch once the server has it.
// notice gets told when this machine had to start a new install
func upload(ctx context.Context, s *store.Store, client *api.Client, notice io.Writer) (api.Result, error) {
	var total api.Result
	install, err := s.InstallID(ctx)
	if err != nil {
		return total, err
	}

	switched := false
	for {
		pending, err := s.Pending(ctx, api.MaxBatch)
		if err != nil {
			return total, err
		}
		if len(pending) == 0 {
			return total, nil
		}
		res, err := client.Upload(ctx, api.Batch{InstallID: install, Events: pending, Projects: projectsOf(pending)})
		if errors.Is(err, api.ErrUnauthorized) {
			return total, errors.New("api key rejected, run sablier login again")
		}
		if errors.Is(err, api.ErrConflict) && !switched {
			// once, so a server that keeps refusing doesn't make installs forever
			if err := s.NewInstall(ctx, install); err != nil {
				return total, err
			}
			if install, err = s.InstallID(ctx); err != nil {
				return total, err
			}
			switched = true
			fmt.Fprintln(notice, "sablier: the server has different events for this machine (restored backup or copied database?), continuing as a new install")
			continue
		}
		if err != nil {
			return total, err
		}
		if err := s.MarkUploaded(ctx, pending[len(pending)-1].Seq, time.Now()); err != nil {
			return total, err
		}
		total.Accepted += res.Accepted
		total.Duplicates += res.Duplicates
	}
}

// display names for the projects in a batch
func projectsOf(events []event.Event) []api.Project {
	seen := map[string]bool{}
	var out []api.Project
	for _, e := range events {
		if e.ProjectID == "" || seen[e.ProjectID] {
			continue
		}
		seen[e.ProjectID] = true
		if name := project.Name(e.ProjectID); project.ValidName(name) {
			out = append(out, api.Project{ID: e.ProjectID, Name: name})
		}
	}
	return out
}
