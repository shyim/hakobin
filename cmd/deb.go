package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"hakobin/internal/config"
	"hakobin/internal/openpgp"
	"hakobin/internal/repository"
	"hakobin/internal/storage"
)

var (
	// deb init flags
	debOrigin        string
	debLabel         string
	debDescription   string
	debDistributions []string
	debComponents    []string
	debArchitectures []string
	debKeyName       string
	debKeyEmail      string
	debKeyExpiration uint32

	// deb upload flags
	debUploadDist  string
	debUploadComp  string
	debUploadForce bool

	// deb list flags
	debListDist string
	debListComp string
	debListPack string

	// deb remove flags
	debRemoveVer   string
	debRemoveArch  string
	debRemoveDist  string
	debRemoveComp  string
	debRemoveForce bool
)

var debCmd = &cobra.Command{
	Use:   "deb",
	Short: "Manage Debian APT repositories",
}

var debInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new APT repository",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.FromEnv()
		if cfg.PublicURL == "" && publicURL != "" {
			cfg.PublicURL = publicURL
		}
		if err := cfg.RequireS3(); err != nil {
			return err
		}

		origin, err := valueOrPrompt(debOrigin, "Repository Origin", "My Organization")
		if err != nil {
			return err
		}
		label, err := valueOrPrompt(debLabel, "Repository Label", "My APT Repository")
		if err != nil {
			return err
		}
		description, err := valueOrPrompt(debDescription, "Repository Description", "APT repository for my packages")
		if err != nil {
			return err
		}
		dists, err := listOrPrompt(debDistributions, "Distributions", "stable")
		if err != nil {
			return err
		}
		comps, err := listOrPrompt(debComponents, "Components", "main")
		if err != nil {
			return err
		}
		archs, err := listOrPrompt(debArchitectures, "Architectures", "amd64,all")
		if err != nil {
			return err
		}
		keyName, err := valueOrPrompt(debKeyName, "Key Name", "Repository Signing Key")
		if err != nil {
			return err
		}
		keyEmail, err := valueOrPrompt(debKeyEmail, "Key Email", "admin@example.com")
		if err != nil {
			return err
		}

		ctx := context.Background()
		store, err := storage.NewS3Store(ctx, cfg)
		if err != nil {
			return err
		}

		manager := repository.NewRepositoryManager(cfg, store)
		req := &repository.InitRequest{
			Metadata: repository.RepoMetadata{
				Origin:        origin,
				Label:         label,
				Description:   description,
				Distributions: dists,
				Components:    comps,
				Architectures: archs,
			},
			KeyName:            keyName,
			KeyEmail:           keyEmail,
			KeyExpirationYears: debKeyExpiration,
		}

		return manager.Init(ctx, req)
	},
}

var debUploadCmd = &cobra.Command{
	Use:   "upload [deb_files...]",
	Short: "Upload one or more .deb packages",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.FromEnv()
		if cfg.PublicURL == "" && publicURL != "" {
			cfg.PublicURL = publicURL
		}
		if err := cfg.RequireS3(); err != nil {
			return err
		}

		ctx := context.Background()
		store, err := storage.NewS3Store(ctx, cfg)
		if err != nil {
			return err
		}

		keys, err := openpgp.LoadSigningKeys(signingKeys, trustedKeys)
		if err != nil {
			return err
		}
		manager := repository.NewRepositoryManager(cfg, store)

		req := &repository.UploadRequest{
			DebFiles:     args,
			Distribution: debUploadDist,
			Component:    debUploadComp,
			Force:        debUploadForce,
			SigningKeys:  keys,
		}

		return manager.Upload(ctx, req)
	},
}

var debListCmd = &cobra.Command{
	Use:   "list [package_name]",
	Short: "List packages in the APT repository",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.FromEnv()
		if cfg.PublicURL == "" && publicURL != "" {
			cfg.PublicURL = publicURL
		}
		if err := cfg.RequireS3(); err != nil {
			return err
		}

		ctx := context.Background()
		store, err := storage.NewS3Store(ctx, cfg)
		if err != nil {
			return err
		}

		manager := repository.NewRepositoryManager(cfg, store)

		var filterPackage *string
		if len(args) > 0 {
			filterPackage = &args[0]
		}
		if debListPack != "" {
			filterPackage = &debListPack
		}

		var filterDist *string
		if debListDist != "" {
			filterDist = &debListDist
		}
		var filterComp *string
		if debListComp != "" {
			filterComp = &debListComp
		}

		return manager.ListPackages(ctx, filterDist, filterComp, filterPackage)
	},
}

var debRemoveCmd = &cobra.Command{
	Use:   "remove [package]",
	Short: "Remove a package from the APT repository",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.FromEnv()
		if cfg.PublicURL == "" && publicURL != "" {
			cfg.PublicURL = publicURL
		}
		if err := cfg.RequireS3(); err != nil {
			return err
		}

		if debRemoveVer == "" {
			return fmt.Errorf("required flag(s) \"version\" not set")
		}
		if debRemoveArch == "" {
			return fmt.Errorf("required flag(s) \"architecture\" not set")
		}

		ctx := context.Background()
		store, err := storage.NewS3Store(ctx, cfg)
		if err != nil {
			return err
		}

		keys, err := openpgp.LoadSigningKeys(signingKeys, trustedKeys)
		if err != nil {
			return err
		}
		manager := repository.NewRepositoryManager(cfg, store)

		req := &repository.RemoveRequest{
			Package:      args[0],
			Version:      debRemoveVer,
			Architecture: debRemoveArch,
			Distribution: debRemoveDist,
			Component:    debRemoveComp,
			Force:        debRemoveForce,
			SigningKeys:  keys,
		}

		return manager.Remove(ctx, req)
	},
}

var debRotateKeyCmd = &cobra.Command{
	Use:   "rotate-key",
	Short: "Refresh APT signing keys and re-sign repository metadata",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.FromEnv()
		if cfg.PublicURL == "" && publicURL != "" {
			cfg.PublicURL = publicURL
		}
		if err := cfg.RequireS3(); err != nil {
			return err
		}

		ctx := context.Background()
		store, err := storage.NewS3Store(ctx, cfg)
		if err != nil {
			return err
		}

		keys, err := openpgp.LoadSigningKeys(signingKeys, trustedKeys)
		if err != nil {
			return err
		}
		if keys.Active == nil {
			return fmt.Errorf("rotate-key requires an active private signing key from GPG_PRIVATE_KEY, --signing-key, or ./signing-key.gpg")
		}

		manager := repository.NewRepositoryManager(cfg, store)
		return manager.RotateKey(ctx, keys)
	},
}

func init() {
	// init flags
	debInitCmd.Flags().StringVar(&debOrigin, "origin", "", "Repository Origin")
	debInitCmd.Flags().StringVar(&debLabel, "label", "", "Repository Label")
	debInitCmd.Flags().StringVar(&debDescription, "description", "", "Repository Description")
	debInitCmd.Flags().StringSliceVar(&debDistributions, "distributions", nil, "Comma-separated list of distributions")
	debInitCmd.Flags().StringSliceVar(&debComponents, "components", nil, "Comma-separated list of components")
	debInitCmd.Flags().StringSliceVar(&debArchitectures, "architectures", nil, "Comma-separated list of architectures")
	debInitCmd.Flags().StringVar(&debKeyName, "key-name", "", "Signing key user name")
	debInitCmd.Flags().StringVar(&debKeyEmail, "key-email", "", "Signing key user email")
	debInitCmd.Flags().Uint32Var(&debKeyExpiration, "key-expiration-years", 2, "Key expiration period in years")

	// upload flags
	debUploadCmd.Flags().StringVarP(&debUploadDist, "distribution", "d", "stable", "Distribution to upload to")
	debUploadCmd.Flags().StringVarP(&debUploadComp, "component", "c", "main", "Component to upload to")
	debUploadCmd.Flags().BoolVarP(&debUploadForce, "force", "f", false, "Force upload even if package exists")

	// list flags
	debListCmd.Flags().StringVarP(&debListDist, "distribution", "d", "", "Filter by distribution")
	debListCmd.Flags().StringVarP(&debListComp, "component", "c", "", "Filter by component")
	debListCmd.Flags().StringVarP(&debListPack, "package", "p", "", "Filter by package name")

	// remove flags
	debRemoveCmd.Flags().StringVarP(&debRemoveVer, "version", "v", "", "Package version (required)")
	debRemoveCmd.Flags().StringVarP(&debRemoveArch, "architecture", "a", "", "Package architecture (required)")
	debRemoveCmd.Flags().StringVarP(&debRemoveDist, "distribution", "d", "stable", "Distribution of the package")
	debRemoveCmd.Flags().StringVarP(&debRemoveComp, "component", "c", "main", "Component of the package")
	debRemoveCmd.Flags().BoolVarP(&debRemoveForce, "force", "f", false, "Skip confirmation prompt")

	debCmd.AddCommand(debInitCmd)
	debCmd.AddCommand(debUploadCmd)
	debCmd.AddCommand(debListCmd)
	debCmd.AddCommand(debRemoveCmd)
	debCmd.AddCommand(debRotateKeyCmd)

	rootCmd.AddCommand(debCmd)
}
