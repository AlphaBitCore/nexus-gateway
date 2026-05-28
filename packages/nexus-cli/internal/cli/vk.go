package cli

import (
	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// newVKCmd builds `nexus vk create`, the way a CLI operator obtains a Virtual
// Key they own (VK secrets are stored hashed, so the only way to get a usable
// key is to create one and capture the once-returned plaintext).
func newVKCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vk",
		Short: "Manage your Virtual Keys",
	}
	cmd.AddCommand(newVKCreateCmd(a))
	return cmd
}

func newVKCreateCmd(a *App) *cobra.Command {
	var name string
	var store bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a personal Virtual Key you own (prints the secret once)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			vk, err := a.client().CreateVK(cmd.Context(), name)
			if err != nil {
				return err
			}
			if store {
				if err := a.Store.Set(a.Env.Name, core.SecretVKSecret, vk.Key); err != nil {
					return err
				}
				a.Env.LastVKID, a.Env.LastVKName = vk.ID, vk.Name
				a.Cfg.SetEnv(a.Env)
				if err := a.Cfg.Save(); err != nil {
					return err
				}
			}
			if a.isJSON() {
				return a.renderJSON(vk)
			}
			a.printf("Created Virtual Key %q (%s)\n", vk.Name, vk.ID)
			a.printf("secret (shown once): %s\n", vk.Key)
			if store {
				a.printf("stored as this env's Virtual Key (keychain).\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "nexus-cli", "name for the new Virtual Key")
	cmd.Flags().BoolVar(&store, "store", true, "store the secret in the keychain + remember it for this env")
	return cmd
}
