// Command rumil is the Rumil pull request reviewer: a read-only GitHub App bot
// that reviews one pull request at a time through an embedded agent session.
package main

import (
	"fmt"
	"os"
)

// Build metadata, injected by goreleaser with -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "rumil:", err)
		os.Exit(1)
	}
}
