package main

import "runtime/debug"

// buildVersion is the release version: stamped at link time by release
// builds, otherwise the module version `go install …@vX.Y.Z` recorded.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
