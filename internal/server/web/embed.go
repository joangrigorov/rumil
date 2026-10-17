// Package web holds the templates and static files the operator surfaces
// serve. They are embedded in the binary, so an installed Rumil needs no
// working directory and no separate asset step.
package web

import "embed"

// FS is the template and static file tree.
//
//go:embed templates/*.html static/*
var FS embed.FS
