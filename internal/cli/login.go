package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newLoginCmd() *cobra.Command {
	var apiURL string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "save your api key, after checking it with the server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			if apiURL != "" {
				c.APIURL = strings.TrimSuffix(apiURL, "/")
			}

			key, err := readKey(cmd.InOrStdin(), cmd.ErrOrStderr(), c.APIURL)
			if err != nil {
				return err
			}
			if key == "" {
				return errors.New("no api key given")
			}

			u, err := api.New(c.APIURL, key).Me(cmd.Context())
			if errors.Is(err, api.ErrUnauthorized) {
				return fmt.Errorf("invalid api key")
			}
			if err != nil {
				return err
			}

			c.APIKey = key
			if err := config.Save(c); err != nil {
				return err
			}
			p, _ := config.Path()
			fmt.Fprintf(cmd.OutOrStdout(), "signed in as %s, saved to %s\n", u.Name, tilde(p))
			return nil
		},
	}
	cmd.Flags().StringVar(&apiURL, "api-url", "", "server url (default: from the config, else "+config.DefaultAPIURL+")")
	return cmd
}

// hidden when typed in a terminal
func readKey(in io.Reader, prompt io.Writer, apiURL string) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(prompt, "api key (from %s/app/settings): ", apiURL)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
