//go:build tools

// Package tools pins the build-time tooling dependencies for the provider so
// `go mod` keeps them in go.mod/go.sum even though no non-test code imports
// them. The build tag keeps this file out of normal builds.
//
// Currently it pins tfplugindocs, the HashiCorp tool that renders the
// generated provider documentation under docs/ from the schema plus the
// templates/ and examples/ directories.
//
// Run generation with `make docs`, which invokes tfplugindocs directly rather
// than via `go generate` — the `tools` build tag must not leak into the
// provider build that tfplugindocs performs internally, or that build fails on
// this very package.
package tools

import (
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
)
