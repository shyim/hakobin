package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shyim/hakobin/internal/config"
)

func TestValueOrPromptReturnsProvidedValue(t *testing.T) {
	got, err := valueOrPrompt("  explicit  ", "Origin", "default")
	require.NoError(t, err)
	assert.Equal(t, "explicit", got, "an explicit value must be trimmed and used without prompting")
}

func TestValueOrPromptFailsOnNonInteractiveStdin(t *testing.T) {
	// The test runner's stdin is not a terminal, so with no value provided the
	// prompt must error rather than silently returning a placeholder default.
	_, err := valueOrPrompt("", "Origin", "My Organization")
	require.Error(t, err)
}

func TestListOrPromptReturnsProvidedValues(t *testing.T) {
	got, err := listOrPrompt([]string{"amd64", "arm64"}, "Architectures", "amd64")
	require.NoError(t, err)
	assert.Equal(t, []string{"amd64", "arm64"}, got)
}

// TestApplyPublicURLPrecedence asserts an explicitly-set --public-url flag wins
// over an environment-derived cfg.PublicURL, and that an unset flag leaves the
// environment value intact.
func TestApplyPublicURLPrecedence(t *testing.T) {
	newRoot := func() (*cobra.Command, *cobra.Command) {
		root := &cobra.Command{Use: "hakobin"}
		root.PersistentFlags().StringVar(&publicURL, "public-url", "", "")
		child := &cobra.Command{Use: "child"}
		root.AddCommand(child)
		return root, child
	}

	// Flag set: overrides the env value.
	publicURL = ""
	root, child := newRoot()
	require.NoError(t, root.PersistentFlags().Set("public-url", "https://flag.example.com"))
	cfg := &config.Config{PublicURL: "https://env.example.com"}
	applyPublicURL(child, cfg)
	assert.Equal(t, "https://flag.example.com", cfg.PublicURL)

	// Flag not set: env value preserved.
	publicURL = ""
	root2, child2 := newRoot()
	_ = root2
	cfg2 := &config.Config{PublicURL: "https://env.example.com"}
	applyPublicURL(child2, cfg2)
	assert.Equal(t, "https://env.example.com", cfg2.PublicURL)
}
