package cli

import (
	"bufio"
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
	last := summary.LastCachedSeq
	if last == 0 {
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
	install, err := s.InstallID(ctx)
	if err != nil {
		return err
	}
	client := api.New(cfg.APIURL, cfg.APIKey)
	checkpoint, err := client.Checkpoint(ctx, project, install)
	if err != nil {
		return err
	}
	if checkpoint.LastSeq >= last {
		fmt.Fprintln(out, "already up to date")
		return nil
	}
	count, err := s.CountNewPrompts(ctx, project, checkpoint.LastSeq, last)
	if err != nil {
		return err
	}
	notice := cmd.ErrOrStderr()
	fmt.Fprintf(notice, "%d unsent %s • %d total %s\n\n", count, promptNoun(count), summary.Total, promptNoun(summary.Total))
	fmt.Fprintf(notice, "If you continue, sablier will send the full text of your unsent prompts to %s to verify your time. Everything will be deleted as soon as the server is done with it.\n\n[y/N] ", cfg.APIURL)
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

	stop := verificationProgress(notice)
	err = sendNewPrompts(cmd, s, client, project, install, checkpoint.LastSeq, last)
	stop()
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "done")
	return nil
}

func sendNewPrompts(cmd *cobra.Command, s *store.Store, c *api.Client, project, install string, after, last int64) error {
	ctx := cmd.Context()
	for after < last {
		rows, err := s.ProjectPromptBatch(ctx, project, after, last, api.VerifyBatchSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return errors.New("local prompt data changed, run verify again")
		}
		b := api.PromptBatch{Project: project, Install: install, Prompts: make([]api.Prompt, len(rows))}
		for i, row := range rows {
			b.Prompts[i] = api.Prompt{Seq: row.Seq, Text: row.Text}
		}
		checkpoint, err := c.SendPrompts(ctx, b)
		if err != nil {
			return err
		}
		after = checkpoint.LastSeq
	}
	return nil
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

func promptNoun(n int64) string {
	if n == 1 {
		return "prompt"
	}
	return "prompts"
}
