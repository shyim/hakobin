package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"hakobin/internal/config"
	"hakobin/internal/openpgp"
	"hakobin/internal/rpm"
	"hakobin/internal/storage"
)

var (
	// rpm init/upload/list/remove shared flags
	rpmRepo string
	rpmArch string

	// rpm upload flags
	rpmUploadForce bool

	// rpm list flags
	rpmListPack string

	// rpm remove flags
	rpmRemoveEpoch    string
	rpmRemoveVer      string
	rpmRemoveRel      string
	rpmRemoveArch     string
	rpmRemoveRepoArch string
	rpmRemoveForce    bool
)

var rpmCmd = &cobra.Command{
	Use:   "rpm",
	Short: "Manage RPM YUM/DNF repositories",
}

var rpmInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new RPM YUM/DNF repository",
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

		signingKeys := openpgp.LoadSigningKeys(signingKeys, trustedKeys)
		manager := rpm.NewRpmRepositoryManager(cfg, store)

		req := &rpm.RpmInitRequest{
			Repo:        rpmRepo,
			Arch:        rpmArch,
			SigningKeys: signingKeys,
		}

		return manager.Init(ctx, req)
	},
}

var rpmUploadCmd = &cobra.Command{
	Use:   "upload [rpm_files...]",
	Short: "Upload one or more .rpm packages",
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

		signingKeys := openpgp.LoadSigningKeys(signingKeys, trustedKeys)
		manager := rpm.NewRpmRepositoryManager(cfg, store)

		req := &rpm.RpmUploadRequest{
			RpmFiles:    args,
			Repo:        rpmRepo,
			Arch:        rpmArch,
			Force:       rpmUploadForce,
			SigningKeys: signingKeys,
		}

		return manager.Upload(ctx, req)
	},
}

var rpmListCmd = &cobra.Command{
	Use:   "list [package_name]",
	Short: "List packages in an RPM repository",
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

		manager := rpm.NewRpmRepositoryManager(cfg, store)

		var filterPackage *string
		if len(args) > 0 {
			filterPackage = &args[0]
		}
		if rpmListPack != "" {
			filterPackage = &rpmListPack
		}

		req := &rpm.RpmListRequest{
			Repo:        rpmRepo,
			Arch:        rpmArch,
			PackageName: filterPackage,
		}

		return manager.List(ctx, req)
	},
}

var rpmRemoveCmd = &cobra.Command{
	Use:   "remove [package]",
	Short: "Remove a package from an RPM repository",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.FromEnv()
		if cfg.PublicURL == "" && publicURL != "" {
			cfg.PublicURL = publicURL
		}
		if err := cfg.RequireS3(); err != nil {
			return err
		}

		if rpmRemoveVer == "" {
			return fmt.Errorf("required flag(s) \"version\" not set")
		}
		if rpmRemoveRel == "" {
			return fmt.Errorf("required flag(s) \"release\" not set")
		}
		if rpmRemoveArch == "" {
			return fmt.Errorf("required flag(s) \"arch\" not set")
		}

		ctx := context.Background()
		store, err := storage.NewS3Store(ctx, cfg)
		if err != nil {
			return err
		}

		signingKeys := openpgp.LoadSigningKeys(signingKeys, trustedKeys)
		manager := rpm.NewRpmRepositoryManager(cfg, store)

		req := &rpm.RpmRemoveRequest{
			Package:     args[0],
			Epoch:       rpmRemoveEpoch,
			Version:     rpmRemoveVer,
			Release:     rpmRemoveRel,
			Arch:        rpmRemoveArch,
			Repo:        rpmRepo,
			RepoArch:    rpmRemoveRepoArch,
			Force:       rpmRemoveForce,
			SigningKeys: signingKeys,
		}

		return manager.Remove(ctx, req)
	},
}

var rpmRotateKeyCmd = &cobra.Command{
	Use:   "rotate-key",
	Short: "Refresh RPM signing keys and re-sign packages and repository metadata",
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

		signingKeys := openpgp.LoadSigningKeys(signingKeys, trustedKeys)
		if signingKeys.Active == nil {
			return fmt.Errorf("rotate-key requires an active private signing key from GPG_PRIVATE_KEY, --signing-key, or ./signing-key.gpg")
		}

		manager := rpm.NewRpmRepositoryManager(cfg, store)
		return manager.RotateKey(ctx, signingKeys)
	},
}

func init() {
	// shared flags
	rpmCmd.PersistentFlags().StringVar(&rpmRepo, "repo", "stable", "RPM repository name")
	rpmCmd.PersistentFlags().StringVar(&rpmArch, "arch", "x86_64", "RPM repository architecture")

	// upload flags
	rpmUploadCmd.Flags().BoolVarP(&rpmUploadForce, "force", "f", false, "Force upload even if package exists")

	// list flags
	rpmListCmd.Flags().StringVarP(&rpmListPack, "package", "p", "", "Filter by package name")

	// remove flags
	rpmRemoveCmd.Flags().StringVar(&rpmRemoveEpoch, "epoch", "0", "Package epoch")
	rpmRemoveCmd.Flags().StringVarP(&rpmRemoveVer, "version", "v", "", "Package version (required)")
	rpmRemoveCmd.Flags().StringVarP(&rpmRemoveRel, "release", "r", "", "Package release (required)")
	rpmRemoveCmd.Flags().StringVar(&rpmRemoveArch, "arch", "", "Package architecture (required)")
	rpmRemoveCmd.Flags().StringVar(&rpmRemoveRepoArch, "repo-arch", "x86_64", "Target repository architecture")
	rpmRemoveCmd.Flags().BoolVarP(&rpmRemoveForce, "force", "f", false, "Skip confirmation prompt")

	rpmCmd.AddCommand(rpmInitCmd)
	rpmCmd.AddCommand(rpmUploadCmd)
	rpmCmd.AddCommand(rpmListCmd)
	rpmCmd.AddCommand(rpmRemoveCmd)
	rpmCmd.AddCommand(rpmRotateKeyCmd)

	rootCmd.AddCommand(rpmCmd)
}
