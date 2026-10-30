// Package repoconfig reads the optional .rumil.yaml a repository keeps at its
// root. The file narrows how Rumil reviews that repository: which paths it
// ignores, whether it may request changes, how many inline comments it posts,
// a default focus, and which configured chat provider it uses.
//
// The file is repository-controlled text. It can choose among the providers
// the operator configured and nothing else: it cannot name a base URL, an API
// key, a database, or any other credential, and every key it carries is known
// to this package. It is read from the same ref as RUMIL.md, which is the base
// branch by default, so a pull request cannot change the settings that review
// it.
package repoconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	pathpkg "path"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/joangrigorov/rumil/internal/config"
)

const (
	// FileName is the repository settings file, at the repository root.
	FileName = ".rumil.yaml"
	// MaxBytes caps the file, so a generated document cannot fill the prompt or
	// the parse.
	MaxBytes = 64 << 10

	// maxIgnorePaths and maxDefaultFocus bound the lists; a review reads them
	// into every prompt, so a list is a budget rather than a database.
	maxIgnorePaths  = 50
	maxDefaultFocus = 5
	maxEntryBytes   = 200
	// maxInlineComments is the largest cap a repository may set.
	maxInlineComments = 200
)

// Settings is one .rumil.yaml document, with its values validated.
type Settings struct {
	ResolveThreads    *bool
	RequestChanges    *bool
	IgnorePaths       []string
	DefaultFocus      []string
	MaxInlineComments *int
	// DefaultChat is a configured chat provider's id, empty when the file
	// leaves the choice to the operator.
	DefaultChat string
}

// Effective is what one review actually runs with: the operator's settings,
// narrowed by the repository's file, with the defaults where neither set a
// value.
type Effective struct {
	ResolveThreads    bool
	RequestChanges    bool
	IgnorePaths       []string
	DefaultFocus      string
	MaxInlineComments int
	// ChatProviderID is empty when the operator's selected chat provider runs
	// the review.
	ChatProviderID string
}

// Parse validates one document. models is the operator's provider list, which
// is what makes a repository's provider choice a choice among configured
// providers rather than a new one.
func Parse(data []byte, models config.Models) (*Settings, error) {
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("%s is %d bytes, larger than the %d byte limit", FileName, len(data), MaxBytes)
	}
	if err := rejectDisallowedKeys(data); err != nil {
		return nil, err
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var doc document
	if err := decoder.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty file is a file that asks for nothing.
			return &Settings{}, nil
		}
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must hold one YAML document", FileName)
	}

	settings := &Settings{}
	if doc.Review != nil {
		settings.ResolveThreads = doc.Review.ResolveThreads
		settings.RequestChanges = doc.Review.RequestChanges
		settings.MaxInlineComments = doc.Review.MaxInlineComments
		paths, err := validatePaths(doc.Review.IgnorePaths)
		if err != nil {
			return nil, fmt.Errorf("%s: review.ignore_paths: %w", FileName, err)
		}
		settings.IgnorePaths = paths
		focus, err := validateFocus(doc.Review.DefaultFocus)
		if err != nil {
			return nil, fmt.Errorf("%s: review.default_focus: %w", FileName, err)
		}
		settings.DefaultFocus = focus
		if settings.MaxInlineComments != nil {
			if *settings.MaxInlineComments < 1 || *settings.MaxInlineComments > maxInlineComments {
				return nil, fmt.Errorf("%s: review.max_inline_comments must be between 1 and %d, got %d",
					FileName, maxInlineComments, *settings.MaxInlineComments)
			}
		}
	}
	if doc.Models != nil && strings.TrimSpace(doc.Models.DefaultChat) != "" {
		id := strings.TrimSpace(doc.Models.DefaultChat)
		if _, err := models.ChatProviderByID(id); err != nil {
			return nil, fmt.Errorf("%s: models.default_chat %q is not a chat provider the operator configured: %w",
				FileName, id, err)
		}
		settings.DefaultChat = id
	}
	return settings, nil
}

// Apply resolves the settings a review runs with. The operator's configuration
// is the baseline, the repository's file narrows it, and the defaults fill what
// neither sets. A nil Settings is a repository that keeps no file, which gives
// the operator's settings unchanged.
func (s *Settings) Apply(operator config.Review) Effective {
	effective := Effective{
		ResolveThreads: operator.ThreadsResolvable(),
		RequestChanges: true,
	}
	if s == nil {
		return effective
	}
	if s.ResolveThreads != nil {
		effective.ResolveThreads = *s.ResolveThreads
	}
	if s.RequestChanges != nil {
		effective.RequestChanges = *s.RequestChanges
	}
	if len(s.IgnorePaths) > 0 {
		effective.IgnorePaths = append([]string(nil), s.IgnorePaths...)
	}
	if len(s.DefaultFocus) > 0 {
		effective.DefaultFocus = strings.Join(s.DefaultFocus, "; ")
	}
	if s.MaxInlineComments != nil {
		effective.MaxInlineComments = *s.MaxInlineComments
	}
	effective.ChatProviderID = s.DefaultChat
	return effective
}

// Ignores reports whether path matches one of the effective ignore patterns.
// The patterns are repository-relative: a directory pattern (`docs/**`) covers
// everything below it, and a leading `**/` matches at any depth.
func (e Effective) Ignores(path string) bool {
	for _, pattern := range e.IgnorePaths {
		if matchPattern(pattern, path) {
			return true
		}
	}
	return false
}

func matchPattern(pattern, path string) bool {
	pattern = strings.TrimPrefix(pattern, "./")

	if rest, ok := strings.CutSuffix(pattern, "/**"); ok {
		return path == rest || strings.HasPrefix(path, rest+"/")
	}
	if rest, ok := strings.CutPrefix(pattern, "**/"); ok {
		for {
			if matched, _ := pathpkg.Match(rest, path); matched {
				return true
			}
			slash := strings.IndexByte(path, '/')
			if slash < 0 {
				return false
			}
			path = path[slash+1:]
		}
	}
	matched, _ := pathpkg.Match(pattern, path)
	return matched
}

// validatePaths trims and bounds the ignore list. A pattern is a path in the
// repository, so an absolute path or one that climbs out of the tree is
// refused rather than silently matching nothing.
func validatePaths(paths []string) ([]string, error) {
	if len(paths) > maxIgnorePaths {
		return nil, fmt.Errorf("holds %d patterns, at most %d are allowed", len(paths), maxIgnorePaths)
	}
	out := make([]string, 0, len(paths))
	for i, raw := range paths {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			return nil, fmt.Errorf("entry %d is empty", i+1)
		}
		if len(pattern) > maxEntryBytes {
			return nil, fmt.Errorf("entry %d is %d bytes, at most %d are allowed", i+1, len(pattern), maxEntryBytes)
		}
		if strings.HasPrefix(pattern, "/") {
			return nil, fmt.Errorf("entry %d %q is absolute, and patterns are repository-relative", i+1, pattern)
		}
		for _, part := range strings.Split(pattern, "/") {
			if part == ".." {
				return nil, fmt.Errorf("entry %d %q climbs out of the repository", i+1, pattern)
			}
		}
		out = append(out, pattern)
	}
	return out, nil
}

func validateFocus(focus []string) ([]string, error) {
	if len(focus) > maxDefaultFocus {
		return nil, fmt.Errorf("holds %d entries, at most %d are allowed", len(focus), maxDefaultFocus)
	}
	out := make([]string, 0, len(focus))
	for i, raw := range focus {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return nil, fmt.Errorf("entry %d is empty", i+1)
		}
		if len(entry) > maxEntryBytes {
			return nil, fmt.Errorf("entry %d is %d bytes, at most %d are allowed", i+1, len(entry), maxEntryBytes)
		}
		out = append(out, entry)
	}
	return out, nil
}

// disallowedKeys are the names a repository file may not carry, whatever the
// nesting. Strict decoding already refuses them as unknown fields; naming them
// here gives the person a message about credentials instead of a type error.
var disallowedKeys = map[string]string{
	"api_key":          "credentials belong in the operator's configuration",
	"api_keys":         "credentials belong in the operator's configuration",
	"token":            "credentials belong in the operator's configuration",
	"tokens":           "credentials belong in the operator's configuration",
	"password":         "credentials belong in the operator's configuration",
	"secret":           "credentials belong in the operator's configuration",
	"webhook_secret":   "credentials belong in the operator's configuration",
	"private_key":      "credentials belong in the operator's configuration",
	"private_key_path": "credentials belong in the operator's configuration",
	"dsn":              "the database belongs in the operator's configuration",
	"providers":        "provider definitions belong in the operator's configuration",
}

func rejectDisallowedKeys(data []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		// The strict decoder reports the syntax error with its line.
		return nil
	}
	return walkKeys(&root, "")
}

func walkKeys(node *yaml.Node, prefix string) error {
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := walkKeys(child, prefix); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if reason, bad := disallowedKeys[key]; bad {
				return fmt.Errorf("%s: %s is not allowed: %s", FileName, path, reason)
			}
			if err := walkKeys(node.Content[i+1], path); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := walkKeys(child, prefix); err != nil {
				return err
			}
		}
	}
	return nil
}

type document struct {
	Review *reviewDocument `yaml:"review"`
	Models *modelsDocument `yaml:"models"`
}

type reviewDocument struct {
	ResolveThreads    *bool    `yaml:"resolve_threads"`
	RequestChanges    *bool    `yaml:"request_changes"`
	IgnorePaths       []string `yaml:"ignore_paths"`
	DefaultFocus      []string `yaml:"default_focus"`
	MaxInlineComments *int     `yaml:"max_inline_comments"`
}

type modelsDocument struct {
	DefaultChat string `yaml:"default_chat"`
}
