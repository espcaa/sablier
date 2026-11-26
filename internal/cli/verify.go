package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
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
	notice := cmd.ErrOrStderr()
	fmt.Fprintf(notice, "%d cached %s • %d total %s\n\n", summary.Cached, Noun(summary.Cached, "prompt", "prompts"), summary.Total, Noun(summary.Total, "prompt", "prompts"))
	fmt.Fprintf(notice, "sablier will send a sample of your prompts in clear text to %s. The server checks them against their synced hashes, labels them with a decision model and keeps only the labels.\n\n[y/N] ", cfg.APIURL)
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		fmt.Fprintln(out, "cancelled")
		return nil
	}

	client := api.New(cfg.APIURL, cfg.APIKey)
	if _, err := upload(ctx, s, client, notice); err != nil {
		return err
	}
	stop := verificationProgress(notice)
	total, err := sendProjectReview(ctx, s, client, project)
	stop()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "done • %d %s classified", total.Classified, Noun(int64(total.Classified), "prompt", "prompts"))
	if total.Unavailable > 0 {
		fmt.Fprintf(out, " • %d %s failed verification", total.Unavailable, Noun(int64(total.Unavailable), "prompt", "prompts"))
	}
	fmt.Fprintln(out)
	return nil
}

// reviews every server install whose latest prompt matches this cache, a cache can span installs after a switch
func sendProjectReview(ctx context.Context, s *store.Store, client *api.Client, project string) (api.ReviewResult, error) {
	var total api.ReviewResult
	installs, err := client.ReviewInstalls(ctx, project)
	if err != nil {
		return total, err
	}
	salt, err := s.Salt(ctx)
	if err != nil {
		return total, err
	}
	matched := false
	for _, install := range installs {
		ok, err := s.MatchesLocalReviewAnchor(ctx, project, install.Seq, install.Hash)
		if err != nil {
			return total, err
		}
		if !ok {
			continue
		}
		matched = true
		if err := reviewInstall(ctx, s, client, salt, project, install.Install, &total); err != nil {
			return total, err
		}
	}
	if !matched {
		return total, errors.New("no synced prompt history matches this local cache")
	}
	return total, nil
}

// sends server-chosen samples until the install has nothing left to review
func reviewInstall(ctx context.Context, s *store.Store, client *api.Client, salt []byte, project, install string, total *api.ReviewResult) error {
	for {
		plan, err := client.ReviewPlan(ctx, project, install)
		if err != nil || plan.ThroughSeq == 0 {
			return err
		}
		b, err := reviewBatch(ctx, s, salt, project, install, plan)
		if err != nil {
			return err
		}
		result, err := client.SendPrompts(ctx, b)
		if err != nil {
			return err
		}
		total.Classified += result.Classified
		total.Unavailable += result.Unavailable
	}
}

// prompts missing locally, too long or past the request budget are sent without text, the server counts them as unavailable
func reviewBatch(ctx context.Context, s *store.Store, salt []byte, project, install string, plan api.ReviewPlan) (api.PromptBatch, error) {
	b := api.PromptBatch{ThroughSeq: plan.ThroughSeq, Project: project, Install: install}
	budget := api.VerifyBodyLimit - 64<<10
	if len(plan.Sequences) > api.MaxReviewPrompts {
		return b, errors.New("server asked for too many prompts")
	}
	for _, seq := range plan.Sequences {
		p := api.Prompt{Seq: seq}
		row, err := s.LocalReviewPrompt(ctx, project, seq)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return b, err
		}
		if err == nil && len(row.Text) <= api.MaxReviewPromptBytes {
			// escaping can grow text a lot, so measure the encoded size
			encoded, _ := json.Marshal(row.Text)
			if len(encoded) <= budget {
				budget -= len(encoded)
				p.Text, p.Key = row.Text, prompt.Key(salt, row.SessionID, time.UnixMilli(row.Ts))
			}
		}
		b.Prompts = append(b.Prompts, p)
	}
	return b, nil
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
