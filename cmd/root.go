package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/shyim/hakobin/internal/config"
)

var (
	signingKeys []string
	trustedKeys []string
	publicURL   string
)

var rootCmd = &cobra.Command{
	Use:   "hakobin",
	Short: "Hakobin Package manages DEB and RPM repositories on S3-compatible storage",
}

func Execute() {
	// Cobra otherwise prints the error and usage itself; silence both so we emit
	// a single diagnostic to stderr (not stdout, which scripts consume).
	rootCmd.SilenceErrors = true
	rootCmd.SilenceUsage = true
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// applyPublicURL applies the --public-url flag to cfg. An explicitly-set flag
// always wins over HAKOBIN_PUBLIC_URL (normal CLI precedence); if the flag was
// not set, the environment value already in cfg is left untouched.
func applyPublicURL(cmd *cobra.Command, cfg *config.Config) {
	if cmd.Root().PersistentFlags().Changed("public-url") {
		cfg.PublicURL = publicURL
	}
}

func init() {
	rootCmd.PersistentFlags().StringSliceVar(&signingKeys, "signing-key", nil, "Path to GPG private key(s) for signing")
	rootCmd.PersistentFlags().StringSliceVar(&trustedKeys, "trusted-key", nil, "Path to GPG public key(s) that are trusted")
	rootCmd.PersistentFlags().StringVar(&publicURL, "public-url", "", "Public URL base of the repository (or HAKOBIN_PUBLIC_URL env)")
}
