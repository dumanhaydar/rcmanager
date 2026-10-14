// Package version carries the single source of truth for rcmanager's version.
package version

// Version is the rcmanager release. The CLI reports it, and the daemon reports it
// over IPC so a client from a different install can say so rather than
// silently driving an older server.
//
// It is a var, not a const, so a release build can stamp the tag it was built
// from:
//
//	go build -ldflags "-X github.com/dumanhaydar/rcmanager/internal/version.Version=1.2.3"
//
// The value here is what a plain `go build` or `go install` reports.
var Version = "1.0.0"
