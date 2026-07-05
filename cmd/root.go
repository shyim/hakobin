package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
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
	if err := rootCmd.Execute(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringSliceVar(&signingKeys, "signing-key", nil, "Path to GPG private key(s) for signing")
	rootCmd.PersistentFlags().StringSliceVar(&trustedKeys, "trusted-key", nil, "Path to GPG public key(s) that are trusted")
	rootCmd.PersistentFlags().StringVar(&publicURL, "public-url", "", "Public URL base of the repository (or HAKOBIN_PUBLIC_URL env)")
}
