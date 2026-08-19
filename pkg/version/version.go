// Package version provides build-time version information for the operator.
package version

import (
	"fmt"

	"k8s.io/apimachinery/pkg/version"
)

// These variables are populated at build time via ldflags.
// Example: go build -ldflags "-X github.com/openshift/external-secrets-operator/pkg/version.commitFromGit=$(git rev-parse HEAD)".
var (
	// commitFromGit is the source version that generated this build.
	// Set via -ldflags during build.
	commitFromGit string

	// versionFromGit is the version tag that generated this build.
	// Set via -ldflags during build.
	versionFromGit string

	// majorFromGit is the major version component.
	// Set via -ldflags during build.
	majorFromGit string

	// minorFromGit is the minor version component.
	// Set via -ldflags during build.
	minorFromGit string

	// buildDate is the build timestamp in ISO8601 format.
	// Set via -ldflags during build using: $(date -u +'%Y-%m-%dT%H:%M:%SZ').
	buildDate string
)

// Get returns the overall codebase version information.
// It's used for detecting what code a binary was built from.
func Get() version.Info {
	return version.Info{
		Major:      majorFromGit,
		Minor:      minorFromGit,
		GitCommit:  commitFromGit,
		GitVersion: versionFromGit,
		BuildDate:  buildDate,
	}
}

// String returns a human-readable version string.
// Format: "vX.Y.Z (commit: abc1234, built: 2024-01-01T00:00:00Z)".
func String() string {
	v := Get()
	commit := v.GitCommit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	return fmt.Sprintf("%s (commit: %s, built: %s)", v.GitVersion, commit, v.BuildDate)
}
