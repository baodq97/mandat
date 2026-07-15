// Package buildinfo exposes the version stamped into the binary at release
// time via -ldflags "-X github.com/baodq97/mandat/internal/buildinfo.version=vX.Y.Z".
package buildinfo

import "runtime/debug"

// version is overwritten by the linker on release builds; plain `go build`
// keeps "dev" and falls back to VCS metadata below.
var version = "dev"

// Version reports the stamped release version, or the module version the
// toolchain embedded when nothing was stamped.
func Version() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}
