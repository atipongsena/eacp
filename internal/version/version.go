// Package version holds the build's version. A release sets it with
//
//	-ldflags "-X github.com/atipongsena/eacp/internal/version.Version=vX.Y.Z"
package version

// Version is "dev" in every build that is not a release.
var Version = "dev"
