package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/config"
	"github.com/spf13/cobra"
)

func newLoginCmd() *cobra.Command {
	var apiURL string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "sign in through your browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if apiURL != "" {
				cfg.APIURL = strings.TrimSuffix(apiURL, "/")
			}
			return loginDevice(cmd, cfg, noBrowser)
		},
	}
	cmd.Flags().StringVar(&apiURL, "api-url", "", "server url")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the sign-in link without opening a browser")
	return cmd
}

func loginDevice(cmd *cobra.Command, cfg config.Config, noBrowser bool) error {
	base, err := url.Parse(cfg.APIURL)
	if err != nil || base.User != nil || base.Hostname() == "" || base.RawQuery != "" || base.Fragment != "" ||
		(base.Scheme != "https" && !(base.Scheme == "http" && loopbackHost(base.Hostname()))) {
		return errors.New("sign-in requires an https server url or a local http server")
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	client := api.New(cfg.APIURL, "")
	hostname, _ := os.Hostname()
	login, err := client.StartDeviceLogin(ctx, hostname)
	if err != nil {
		return err
	}
	if login.Approval == "" || login.PollToken == "" {
		return errors.New("server returned an incomplete sign-in request")
	}
	link := cfg.APIURL + "/cli/authorize?" + url.Values{"request": {login.Approval}}.Encode()
	fmt.Fprintf(cmd.ErrOrStderr(), "open this link in a browser to sign in:\n%s\nwaiting for approval...\n", link)
	if !noBrowser {
		openLoginBrowser(link)
	}
	return waitForDevice(ctx, cmd, cfg, client, login.PollToken, 5*time.Second)
}

func loopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func waitForDevice(ctx context.Context, cmd *cobra.Command, cfg config.Config, client *api.Client, token string, interval time.Duration) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("sign-in did not complete: %w", ctx.Err())
		case <-time.After(interval):
		}
		result, err := client.PollDeviceLogin(ctx, token)
		if err != nil {
			return err
		}
		switch result.Status {
		case "pending":
			continue
		case "denied":
			return errors.New("sign-in cancelled")
		case "expired":
			return errors.New("sign-in expired, run sablier login again")
		case "approved":
			if !strings.HasPrefix(result.Token, "sbd_") || len(result.Token) != 47 || result.DeviceID == "" {
				return errors.New("invalid device credential from server")
			}
			cfg.APIKey = result.Token
			if err := config.Save(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "signed in to sablier (device %s)\n", result.DeviceID)
			return nil
		default:
			return errors.New("unexpected device login status")
		}
	}
}

func openLoginBrowser(link string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", link)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		command = exec.Command("xdg-open", link)
	}
	go command.Run()
}
