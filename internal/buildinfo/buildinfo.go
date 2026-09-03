// Package buildinfo carries identifiers stamped into the binary.
package buildinfo

// Version is the release of the server software. Overridden at build time with
//
//	go build -ldflags "-X github.com/aural-chat/aural-server/internal/buildinfo.Version=x.y.z"
var Version = "0.6.2"

// Name is the product name reported to clients.
const Name = "aural-server"
