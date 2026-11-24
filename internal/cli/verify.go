package cli

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/config"
	"github.com/espcaa/sablier/internal/paths"
	"github.com/espcaa/sablier/internal/prompt"
	"github.com/espcaa/sablier/internal/store"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <project>",
		Short: "send new project prompts for verification",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("specify a project (see sablier list-projects)")
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			err := verifyProject(cmd, args[0])
			if errors.Is(err, api.ErrUnauthorized) {
				return errors.New("api key rejected, run sablier login again")
			}
			return err
		},
	}
}

func verifyProject(cmd *cobra.Command, project string) error {
	ctx := cmd.Context()
	dir, err := paths.DataDir()
	if err != nil {
		return err
	}
	s, err := store.Open(filepath.Join(dir, "sablier.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	summary, err := s.ProjectPromptSummary(ctx, project)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if summary.Cached == 0 {
		fmt.Fprintln(out, "cannot verify this project: no local prompt data")
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.APIKey == "" {
		return errors.New("not signed in, run sablier login first")
	}
	client := api.New(cfg.APIURL, cfg.APIKey)
	installs, err := client.ReviewInstalls(ctx, project)
	if err != nil {
		return err
	}
	notice := cmd.ErrOrStderr()
	fmt.Fprintf(notice, "%d cached %s • %d total %s\n\n", summary.Cached, Noun(summary.Cached, "prompt", "prompts"), summary.Total, Noun(summary.Total, "prompt", "prompts"))
	fmt.Fprintf(notice, "sablier will send a subset of your prompts in clear text and their hashes to %s. The server will check and analyze the prompt with a decision model before deleting everything.\n\n[y/N] ", cfg.APIURL)
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
	default:
		fmt.Fprintln(out, "cancelled")
		return nil
	}

	synced, err := upload(ctx, s, client, notice)
	if err != nil {
		return err
	}
	if synced.Accepted+synced.Duplicates > 0 {
		installs, err = client.ReviewInstalls(ctx, project)
		if err != nil {
			return err
		}
	}
	stop := verificationProgress(notice)
	classified, unavailable, err := sendProjectReview(cmd, s, client, project, installs)
	stop()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "done • %d %s classified", classified, Noun(int64(classified), "prompt", "prompts"))
	if unavailable > 0 {
		fmt.Fprintf(out, " • %d %s unavailable or failed verification", unavailable, Noun(unavailable, "prompt", "prompts"))
	}
	fmt.Fprintln(out)
	return nil
}

func sendProjectReview(cmd *cobra.Command, s *store.Store, client *api.Client, project string, installs []api.ReviewInstall) (int, int64, error) {
	ctx := cmd.Context()
	var classified int
	var unavailable int64
	var matched bool
	for _, install := range installs {
		ok, err := s.MatchesLocalReviewAnchor(ctx, project, install.Seq, install.Hash)
		if err != nil {
			return classified, unavailable, err
		}
		if !ok {
			continue
		}
		matched = true
		count, gaps, err := sendReviewSamples(cmd, s, client, project, install.Install, install.Seq)
		classified += count
		unavailable += gaps
		if err != nil {
			return classified, unavailable, err
		}
	}
	if !matched {
		return 0, 0, errors.New("no synced prompt history matches this local cache")
	}
	return classified, unavailable, nil
}

func sendReviewSamples(cmd *cobra.Command, s *store.Store, c *api.Client, project, install string, last int64) (int, int64, error) {
	ctx := cmd.Context()
	salt, err := s.Salt(ctx)
	if err != nil {
		return 0, 0, err
	}
	var classified int
	var unavailable int64
	for {
		plan, err := c.ReviewPlan(ctx, project, install)
		if err != nil {
			return classified, unavailable, err
		}
		if plan.ThroughSeq == 0 {
			return classified, unavailable, nil
		}
		if len(plan.Sequences) > api.MaxReviewPrompts {
			return classified, unavailable, errors.New("server returned too many prompts in a review sample")
		}
		b := api.PromptBatch{ThroughSeq: plan.ThroughSeq, Project: project, Install: install}
		for _, seq := range plan.Sequences {
			row, err := s.LocalReviewPrompt(ctx, project, seq)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return classified, unavailable, err
			}
			p := api.Prompt{Seq: seq}
			if err == nil && len(row.Text) <= api.MaxReviewPromptBytes {
				p.Text = row.Text
				p.Key = prompt.Key(salt, row.SessionID, time.UnixMilli(row.Ts))
			}
			b.Prompts = append(b.Prompts, p)
		}
		result, err := c.SendPrompts(ctx, b)
		if err != nil {
			return classified, unavailable, err
		}
		classified += result.Classified
		unavailable += result.Unavailable
		if result.AnalyzedSeq >= last {
			return classified, unavailable, nil
		}
	}
}

func verificationProgress(out io.Writer) func() {
	f, ok := out.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		fmt.Fprintln(out, "sending prompts...")
		return func() {}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	fmt.Fprint(out, "sending prompts...")
	go func() {
		defer close(done)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				fmt.Fprint(out, "\r\x1b[K")
				return
			case <-tick.C:
				fmt.Fprintf(out, "\rsending prompts... %c", "|/-\\"[i%4])
			}
		}
	}()
	return func() { close(stop); <-done }
}
