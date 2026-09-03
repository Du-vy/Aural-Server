package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// MetaAutoMod holds the automatic moderation rules, as the JSON object the
// protocol carries them in.
//
// One row rather than a table of rules: the rules constrain one another — a
// global exemption list plus a per-rule one — and they are read as a set on
// every message and written whole by one settings screen. A table would buy
// nothing but the ability to half-apply an edit.
const MetaAutoMod = "automod"

// MetaDeviceSalt is the per-install secret a client folds into the device
// identifier it presents.
//
// It is what keeps that identifier local to this server: the same machine
// produces a different value on every server it connects to, so the value can
// be used to enforce a ban here and is worthless as a way of following
// somebody between servers.
const MetaDeviceSalt = "device_salt"

// AutoMod reads the stored rules into out, which the caller has already filled
// with its defaults. A server that has never configured them keeps those.
func (s *Store) AutoMod(ctx context.Context, out any) error {
	raw, err := s.Meta(ctx, MetaAutoMod)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		// A row that will not decode is a server whose rules were written by a
		// build that is gone, or edited by hand. Falling back to the defaults
		// the caller came in with is the safe reading: it moderates nothing
		// rather than moderating something nobody configured.
		return fmt.Errorf("store: decode automod rules: %w", err)
	}
	return nil
}

// SetAutoMod writes the rules whole.
func (s *Store) SetAutoMod(ctx context.Context, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("store: encode automod rules: %w", err)
	}
	return s.SetMeta(ctx, MetaAutoMod, string(raw))
}
