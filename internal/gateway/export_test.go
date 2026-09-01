package gateway

import "context"

// SweepOrphanedFiles exposes the startup sweep to this package's external
// tests. The case worth pinning is not that it deletes: it is what it spares.
// A picture is reachable only through its own table, so a keep set built from
// the attachments table alone would take every avatar on the server with it.
func (s *Server) SweepOrphanedFiles(ctx context.Context) { s.sweepOrphanedFiles(ctx) }
