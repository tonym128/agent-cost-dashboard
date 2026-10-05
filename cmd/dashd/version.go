package main

import "fmt"

// Release stamping.
//
// The Makefile and .goreleaser.yml both pass these three names with
// `-ldflags -X main.version=... -X main.commit=... -X main.date=...`. These are
// the symbols those flags look for; while they were absent from the package the
// linker accepted every one of them and discarded it, so a released binary
// reported nothing about itself. The defaults here are what an unstamped `go
// build` gets, and match the ARGs in the Dockerfile so a container build and a
// local build look the same when nobody passed anything.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// versionString is the one line `dashd -version` prints.
func versionString() string {
	return fmt.Sprintf("dashd %s (commit %s, built %s)", version, commit, date)
}
