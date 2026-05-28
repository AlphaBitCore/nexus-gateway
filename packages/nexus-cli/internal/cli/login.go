package cli

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

func newLoginCmd(a *App) *cobra.Command {
	var adminKey bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate to the active environment",
		Long: "Logs in via browser OAuth2+PKCE (humans). With --admin-key, reads an " +
			"admin API key from stdin and stores it for a machine profile.",
		// login resolves an env but is the command that creates the credential, so
		// it is exempt from the logged-in guard.
		Annotations: map[string]string{"skipAuth": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if adminKey {
				return a.storeAdminKey(cmd)
			}
			auth := core.NewAuthenticator(a.Env, a.Store, a.HTTP).WithBrowserOpener(a.BrowserOpener)
			if err := auth.LoginBrowser(cmd.Context()); err != nil {
				return err
			}
			a.printf("Logged in to %q.\n", a.Env.Name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&adminKey, "admin-key", false, "store an admin API key (nxk_…) read from stdin instead of browser login")
	return cmd
}

// storeAdminKey reads an admin key from stdin and persists it for the env.
func (a *App) storeAdminKey(cmd *cobra.Command) error {
	fmt.Fprint(a.ErrOut, "Paste admin API key (nxk_…): ")
	sc := bufio.NewScanner(cmd.InOrStdin())
	if !sc.Scan() {
		return fmt.Errorf("%w: no admin key on stdin", errUsage)
	}
	key := strings.TrimSpace(sc.Text())
	if key == "" {
		return fmt.Errorf("%w: empty admin key", errUsage)
	}
	if err := a.Store.Set(a.Env.Name, core.SecretAdminKey, key); err != nil {
		return err
	}
	a.printf("Stored admin key for %q (machine profile).\n", a.Env.Name)
	return nil
}
