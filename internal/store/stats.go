package store

import (
	"context"
	"fmt"
)

// ServerStats holds summary statistics of the data in the store.
type ServerStats struct {
	RegisteredUsers int
	GuestUsers      int
	TotalUsers      int
	Categories      int
	TextChannels    int
	VoiceChannels   int
	TotalChannels   int
	TotalRoles      int
	TotalMessages   int
}

// Stats collects count metrics across users, channels, roles, and messages.
func (s *Store) Stats(ctx context.Context) (ServerStats, error) {
	var stats ServerStats

	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN username IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN username IS NULL THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM users
	`).Scan(&stats.RegisteredUsers, &stats.GuestUsers, &stats.TotalUsers)
	if err != nil {
		return stats, fmt.Errorf("store: query user stats: %w", err)
	}

	err = s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN type = 'category' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN type = 'text' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN type = 'voice' THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM channels
	`).Scan(&stats.Categories, &stats.TextChannels, &stats.VoiceChannels, &stats.TotalChannels)
	if err != nil {
		return stats, fmt.Errorf("store: query channel stats: %w", err)
	}

	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM roles`).Scan(&stats.TotalRoles)
	if err != nil {
		return stats, fmt.Errorf("store: query role stats: %w", err)
	}

	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&stats.TotalMessages)
	if err != nil {
		return stats, fmt.Errorf("store: query message stats: %w", err)
	}

	return stats, nil
}
