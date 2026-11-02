package main

import (
	"runtime"
	"runtime/debug"
)

// version is set at build time: go build -ldflags "-X main.version=1.4.0"
var version = "dev"

// BuildInfo identifies the running binary.
type BuildInfo struct {
	Version   string
	Revision  string // git commit, "" when built outside a git checkout
	GoVersion string
}

func currentBuildInfo() BuildInfo {
	bi := BuildInfo{Version: version, GoVersion: runtime.Version()}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				bi.Revision = s.Value
				if len(bi.Revision) > 12 {
					bi.Revision = bi.Revision[:12]
				}
			}
		}
	}
	return bi
}
