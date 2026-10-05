# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- The `go.mod` module path said `tonym128/agent-cost-dashboard`, which did not
  match the git remote, so `go install` and any `go get` of this module failed.
  Corrected to `github.com/tonym128/agent-cost-dashboard`.
- The `LICENSE` named Duncan Ogilvie, the author of the `modernc.org/sqlite`
  dependency, rather than the author of this project.