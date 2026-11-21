package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const loginTimeout = 5 * time.Minute

var openBrowser = defaultOpenBrowser

func newLoginCmd() *cobra.Command {
	var apiURL string
	var manual, noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "sign in through your browser, or paste your api key with --key",
		Long: `sign in through your browser: sablier opens a page on the server, you allow
the cli, and the api key is saved for you. nothing to copy.

with --key, paste the api key from the dashboard instead (handy over ssh).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			if apiURL != "" {
				c.APIURL = strings.TrimSuffix(apiURL, "/")
			}

			var key, name string
			if manual {
				key, name, err = keyLogin(cmd, c.APIURL)
			} else {
				key, name, err = browserLogin(cmd, c.APIURL, !noBrowser)
			}
			if err != nil {
				return err
			}

			c.APIKey = key
			if err := config.Save(c); err != nil {
				return err
			}
			p, _ := config.Path()
			fmt.Fprintf(cmd.OutOrStdout(), "signed in as %s, saved to %s\n", name, tilde(p))
			return nil
		},
	}
	cmd.Flags().StringVar(&apiURL, "api-url", "", "server url (default: from the config, else "+config.DefaultAPIURL+")")
	cmd.Flags().BoolVar(&manual, "key", false, "paste an api key instead of signing in through the browser")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the sign in link instead of opening it")
	return cmd
}

func keyLogin(cmd *cobra.Command, apiURL string) (string, string, error) {
	key, err := readKey(cmd.InOrStdin(), cmd.ErrOrStderr(), apiURL)
	if err != nil {
		return "", "", err
	}
	if key == "" {
		return "", "", errors.New("no api key given")
	}
	u, err := api.New(apiURL, key).Me(cmd.Context())
	if errors.Is(err, api.ErrUnauthorized) {
		return "", "", fmt.Errorf("invalid api key")
	}
	if err != nil {
		return "", "", err
	}
	return key, u.Name, nil
}

type loginResult struct {
	key, name string
	err       error
}

func browserLogin(cmd *cobra.Command, apiURL string, open bool) (string, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("can't start the local callback server: %w (try sablier login --key)", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	state := randomString(24)
	verifier := randomString(32)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"port":                  {strconv.Itoa(port)},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	authURL := apiURL + "/cli/authorize?" + q.Encode()

	ctx, cancel := context.WithTimeout(cmd.Context(), loginTimeout)
	defer cancel()

	results := make(chan loginResult, 1)
	client := api.New(apiURL, "")
	send := func(r loginResult) {
		select {
		case results <- r:
		default:
		}
	}
	done := func(w http.ResponseWriter, r *http.Request, errCode string) {
		u := apiURL + "/cli/done"
		if errCode != "" {
			u += "?error=" + url.QueryEscape(errCode)
		}
		http.Redirect(w, r, u, http.StatusFound)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		if v.Get("state") != state {
			http.Error(w, "this doesn't match the running sablier login", http.StatusBadRequest)
			return
		}
		if e := v.Get("error"); e != "" {
			done(w, r, e)
			send(loginResult{err: errors.New("sign in was cancelled in the browser")})
			return
		}
		t, err := client.CLIToken(r.Context(), v.Get("code"), verifier)
		if err != nil {
			done(w, r, "failed")
			send(loginResult{err: err})
			return
		}
		done(w, r, "")
		send(loginResult{key: t.APIKey, name: t.Name})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer func() {
		sctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		srv.Shutdown(sctx)
	}()

	out := cmd.ErrOrStderr()
	if open && openBrowser(authURL) == nil {
		fmt.Fprintf(out, "opened your browser to sign in. if nothing showed up, open:\n\n  %s\n\n", authURL)
	} else {
		fmt.Fprintf(out, "open this link to sign in:\n\n  %s\n\n", authURL)
	}
	fmt.Fprintln(out, "waiting for the browser... (ctrl+c to stop, or use sablier login --key)")

	select {
	case res := <-results:
		return res.key, res.name, res.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", "", errors.New("timed out waiting for the browser, run sablier login again")
		}
		return "", "", ctx.Err()
	}
}

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func defaultOpenBrowser(u string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", u)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errors.New("no display")
		}
		c = exec.Command("xdg-open", u)
	}
	c.Stdout, c.Stderr = nil, nil
	return c.Start()
}

// hidden when typed in a terminal
func readKey(in io.Reader, prompt io.Writer, apiURL string) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(prompt, "api key (from %s/app): ", apiURL)
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
