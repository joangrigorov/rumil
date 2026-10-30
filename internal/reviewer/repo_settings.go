package reviewer

import (
	"context"
	"fmt"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/repoconfig"
)

// readRepositorySettings reads the optional .rumil.yaml at ref and validates
// it against the operator's providers. found is false when the repository keeps
// no file, which is not an error; an invalid file is, because falling back to
// the operator's settings would review a repository against rules it wrote to
// replace.
func readRepositorySettings(ctx context.Context, client *ghapp.Client, ref string, models config.Models) (*repoconfig.Settings, bool, error) {
	text, found, err := client.RepositoryFile(ctx, repoconfig.FileName, ref, repoconfig.MaxBytes)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", repoconfig.FileName, err)
	}
	if !found {
		return nil, false, nil
	}
	settings, err := repoconfig.Parse([]byte(text), models)
	if err != nil {
		return nil, true, fmt.Errorf("reading the repository's review settings: %w", err)
	}
	return settings, true, nil
}
