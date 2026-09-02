package gateway

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/voice"
)

// The audio plane as the hub sees it.
//
// Two different things are tracked, and keeping them apart is what makes the
// rest of this file simple. Sitting in a voice channel is presence: it lives on
// the session, it is set by user.move, and it has worked that way since v0.1.
// Holding a live media session is the audio on top of it: it lives in a
// voiceRoom, it is set by voice.connect, and a client with no microphone never
// acquires one. Somebody can be in the channel and not in the room; nobody can
// be in the room and not in the channel.

// voiceRoom is the set of participants of one channel who have media up.
//
// Members are kept in arrival order because that order is the host election:
// in client_host mode the first to arrive relays for the rest, and when they go
// the next one does. There is nothing cleverer to choose on — the server knows
// nothing about anybody's uplink — and arrival order at least has the property
// that the person who has been there longest is the one who stays.
type voiceRoom struct {
	channelID int64
	members   []int64
}

// host is the member that relays, or zero when the room is empty.
func (r *voiceRoom) host() int64 {
	if len(r.members) == 0 {
		return 0
	}
	return r.members[0]
}

// --- configuration ----------------------------------------------------------

// voiceConfig snapshots the audio plane. It is read on every voice frame and
// written only by an administrator, so it lives behind the same lock as the
// other configuration that changes at runtime.
func (h *Hub) voiceConfig() config.Voice {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	cfg := h.cfg.Voice
	cfg.ICEServers = slices.Clone(cfg.ICEServers)
	return cfg
}

// voiceInfo is the audio plane as a client is told about it. It deliberately
// carries no ICE servers: this travels in the unauthenticated server preview.
func (h *Hub) voiceInfo() protocol.Voice {
	cfg := h.voiceConfig()
	return protocol.Voice{
		Enabled:         cfg.Enabled && h.relay != nil,
		Mode:            cfg.Mode,
		SampleRate:      cfg.SampleRate,
		Bitrate:         cfg.Bitrate,
		MinBitrate:      cfg.MinBitrate,
		MaxBitrate:      cfg.MaxBitrate,
		FEC:             cfg.FEC,
		DTX:             cfg.DTX,
		Stereo:          cfg.Stereo,
		MaxParticipants: cfg.MaxParticipants,
	}
}

// iceServers is the STUN and TURN list handed to an authenticated client. The
// credentials in it are the reason it is not part of the public preview.
func (h *Hub) iceServers() []protocol.ICEServer {
	cfg := h.voiceConfig()
	out := make([]protocol.ICEServer, 0, len(cfg.ICEServers))
	for _, srv := range cfg.ICEServers {
		out = append(out, protocol.ICEServer{
			URLs:       slices.Clone(srv.URLs),
			Username:   srv.Username,
			Credential: srv.Credential,
		})
	}
	return out
}

// --- the advertised address -------------------------------------------------

// PublicIP is the address the relay is currently advertising, or empty when it
// is advertising the addresses of its own interfaces.
func (h *Hub) PublicIP() string {
	if held := h.publicIP.Load(); held != nil {
		return *held
	}
	return ""
}

func (h *Hub) storePublicIP(addr string) {
	h.publicIP.Store(&addr)
}

// resolvePublicIP asks the resolver, under a timeout of its own so that a DNS
// server which has stopped answering cannot hold up a startup or a watch tick.
func (h *Hub) resolvePublicIP(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, publicIPTimeout)
	defer cancel()

	addr, err := h.publicAddr.Resolve(ctx)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

// WatchPublicIP keeps the relay's advertised address current until ctx ends.
//
// This is what makes a home server usable. The address a residential
// connection holds is not the operator's to keep: it changes when the provider
// renews the lease, when the router reboots, when the line drops for a minute
// at four in the morning. Everything else survives that — a dynamic DNS record
// is updated by something, clients reconnect, the WebSocket comes back — but
// the relay has already baked the old address into the candidates it offers,
// so voice, and only voice, stays broken until somebody notices and restarts
// the server.
//
// A change costs every live call a renegotiation, which is a second of
// silence. That is the correct price: the alternative is that the calls do not
// work at all.
func (h *Hub) WatchPublicIP(ctx context.Context) {
	if h.publicAddr == nil || h.publicAddr.Static() {
		// A literal cannot change, and a server with no source has nothing to
		// look up. Neither is worth a goroutine and a timer.
		return
	}

	timer := time.NewTimer(publicIPInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		next := publicIPInterval
		resolved, err := h.resolvePublicIP(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			h.log.Debug("could not resolve the address to advertise for voice",
				slog.Any("error", err))
			next = publicIPRetry
		case resolved != h.PublicIP():
			h.applyPublicIP(resolved)
		}
		timer.Reset(next)
	}
}

// applyPublicIP records a new address and rebuilds the relay around it.
//
// Reconfigure tears the rooms down, which is the only way to change what ICE
// candidates say: they were negotiated with the old address and cannot be
// edited afterwards. Clients are told to open a new media session, which is a
// path they exercise every time somebody joins a call.
func (h *Hub) applyPublicIP(resolved string) {
	previous := h.PublicIP()
	h.storePublicIP(resolved)
	h.log.Info("the address advertised for voice changed",
		slog.String("from", previous),
		slog.String("to", resolved),
		slog.String("source", h.publicAddr.Describe()))

	if h.relay == nil {
		return
	}
	if err := h.relay.Reconfigure(relaySettings(h.voiceConfig(), resolved)); err != nil {
		h.log.Error("could not rebuild the audio plane for the new address", slog.Any("error", err))
	}
}

// iceURLs flattens the configured ICE servers down to their URLs, which is all
// the address resolver needs from them.
func iceURLs(servers []config.ICEServer) []string {
	var out []string
	for _, srv := range servers {
		out = append(out, srv.URLs...)
	}
	return out
}

// relaySettings converts the configuration into what the relay needs.
//
// publicIP is the resolved address rather than the configured one. The two
// differ whenever the operator named a hostname or left the field empty for
// STUN to answer, which is the whole of the dynamic-address case: the relay
// substitutes a literal into its candidates and has nothing to resolve one
// with.
func relaySettings(cfg config.Voice, publicIP string) voice.Settings {
	return voice.Settings{
		SampleRate: cfg.SampleRate,
		Bitrate:    cfg.Bitrate,
		MinBitrate: cfg.MinBitrate,
		MaxBitrate: cfg.MaxBitrate,
		FEC:        cfg.FEC,
		DTX:        cfg.DTX,
		Stereo:     cfg.Stereo,
		PublicIP:   publicIP,
		UDPPortMin: cfg.UDPPortMin,
		UDPPortMax: cfg.UDPPortMax,
	}
}

// --- state ------------------------------------------------------------------

// voiceStateOf renders a session's voice state, or reports that it has none.
//
// The mute flag folds in the Speak permission, because from every reader's
// point of view — the interface drawing a crossed-out microphone, the host
// deciding whether to relay somebody — being unable to speak and being muted
// are the same fact.
func (h *Hub) voiceStateOf(s *Session) (protocol.VoiceState, bool) {
	channelID := s.ChannelID()
	if channelID == nil {
		return protocol.VoiceState{}, false
	}
	if ch, ok := h.Channel(*channelID); !ok || ch.Type != protocol.ChannelVoice {
		return protocol.VoiceState{}, false
	}

	base, roleIDs := s.Permissions()
	canSpeak := h.ChannelPermissions(base, roleIDs, *channelID).Has(permissions.Speak)

	v := s.voiceSnapshot()
	state := protocol.VoiceState{
		UserID:    s.UserID(),
		ChannelID: *channelID,
		Connected: v.connected,
		SelfMute:  v.selfMute,
		SelfDeaf:  v.selfDeaf,
		Mute:      v.mute || !canSpeak,
		Deaf:      v.deaf,
	}

	h.voiceMu.Lock()
	if room, ok := h.voiceRooms[*channelID]; ok {
		state.Host = room.host() == s.UserID()
	}
	h.voiceMu.Unlock()

	return state, true
}

// voiceStatesFor lists the voice states a viewer is allowed to see, which is
// what the ready snapshot carries. A channel the viewer cannot see contributes
// nothing, exactly as it contributes no channel and no members.
func (h *Hub) voiceStatesFor(viewer *Session) []protocol.VoiceState {
	sessions := h.Sessions()
	out := make([]protocol.VoiceState, 0, len(sessions))
	for _, s := range sessions {
		if s.UserID() != viewer.UserID() && HidesPresence(s.User().Status) {
			continue
		}
		state, ok := h.voiceStateOf(s)
		if !ok || !h.SessionCanView(viewer, state.ChannelID) {
			continue
		}
		out = append(out, state)
	}
	return out
}

// broadcastVoiceState tells everyone who can see the channel how one
// participant now stands. A user who is hiding tells only themselves, for the
// same reason their channel is masked out of everybody else's view of them.
func (h *Hub) broadcastVoiceState(s *Session) {
	state, ok := h.voiceStateOf(s)
	if !ok {
		return
	}
	event := protocol.Event(protocol.EvVoiceState, protocol.VoiceStateEvent{State: state})

	if HidesPresence(s.User().Status) {
		s.Send(event)
		return
	}
	h.BroadcastChannelEvent(event, state.ChannelID)
}

// --- rooms ------------------------------------------------------------------

// voiceAttach records that a session has media up in a channel and returns the
// room as it stands afterwards.
func (h *Hub) voiceAttach(userID, channelID int64) (host int64, epoch int64, count int) {
	h.voiceMu.Lock()
	defer h.voiceMu.Unlock()

	room, ok := h.voiceRooms[channelID]
	if !ok {
		room = &voiceRoom{channelID: channelID}
		h.voiceRooms[channelID] = room
	}
	if !slices.Contains(room.members, userID) {
		if len(room.members) == 0 {
			// The first arrival is an election, even though there was nobody
			// to run against: the epoch has to move so that signalling from a
			// previous occupancy of this channel is recognisably stale.
			h.voiceEpochs[channelID]++
		}
		room.members = append(room.members, userID)
	}
	return room.host(), h.voiceEpochs[channelID], len(room.members)
}

// voiceMembers lists who has media up in a channel, in arrival order.
func (h *Hub) voiceMembers(channelID int64) []int64 {
	h.voiceMu.Lock()
	defer h.voiceMu.Unlock()
	if room, ok := h.voiceRooms[channelID]; ok {
		return slices.Clone(room.members)
	}
	return nil
}

// voiceRoomState reads a room without changing it.
func (h *Hub) voiceRoomState(channelID int64) (host int64, epoch int64, count int) {
	h.voiceMu.Lock()
	defer h.voiceMu.Unlock()
	if room, ok := h.voiceRooms[channelID]; ok {
		return room.host(), h.voiceEpochs[channelID], len(room.members)
	}
	return 0, 0, 0
}

// voiceDetach removes one participant's media session from a room.
//
// It reports whether the host changed, because that is the one departure the
// rest of the room cannot absorb quietly: in client_host mode everybody was
// connected to the person who just left.
func (h *Hub) voiceDetach(userID, channelID int64) (wasHost bool, remaining []int64, epoch int64) {
	h.voiceMu.Lock()
	defer h.voiceMu.Unlock()

	room, ok := h.voiceRooms[channelID]
	if !ok {
		return false, nil, h.voiceEpochs[channelID]
	}
	index := slices.Index(room.members, userID)
	if index < 0 {
		return false, slices.Clone(room.members), h.voiceEpochs[channelID]
	}

	wasHost = index == 0
	room.members = slices.Delete(room.members, index, index+1)
	if wasHost {
		h.voiceEpochs[channelID]++
	}
	if len(room.members) == 0 {
		delete(h.voiceRooms, channelID)
	}
	return wasHost, slices.Clone(room.members), h.voiceEpochs[channelID]
}

// voiceDropRoom forgets a whole room and returns who was in it.
func (h *Hub) voiceDropRoom(channelID int64) []int64 {
	h.voiceMu.Lock()
	defer h.voiceMu.Unlock()

	room, ok := h.voiceRooms[channelID]
	if !ok {
		return nil
	}
	delete(h.voiceRooms, channelID)
	return room.members
}

// forgetVoiceChannel drops what is remembered about a channel that no longer
// exists. The epoch outlives its room on purpose — a client reconnecting into
// the same channel must never be handed a number it has already seen — so a
// deleted channel is the one moment it is safe to forget.
func (h *Hub) forgetVoiceChannel(channelID int64) {
	h.voiceMu.Lock()
	delete(h.voiceRooms, channelID)
	delete(h.voiceEpochs, channelID)
	h.voiceMu.Unlock()
}

// voiceChannels lists every channel with a live room.
func (h *Hub) voiceChannels() []int64 {
	h.voiceMu.Lock()
	defer h.voiceMu.Unlock()
	out := make([]int64, 0, len(h.voiceRooms))
	for id := range h.voiceRooms {
		out = append(out, id)
	}
	return out
}

// --- teardown ---------------------------------------------------------------

// leaveVoice closes one session's media session in a channel and repairs the
// room around it. It is the single exit: leaving the channel, being moved,
// being disconnected, losing the permission to be there and the relay giving up
// on the transport all end here.
//
// It is safe to call for a session that had no media session at all, which is
// what lets every one of those paths call it unconditionally.
func (h *Hub) leaveVoice(s *Session, channelID int64, notifySelf bool) {
	if channelID == 0 {
		return
	}
	had := s.clearVoiceSession(channelID)
	if h.relay != nil {
		h.relay.Leave(channelID, s.UserID())
	}
	if !had {
		return
	}

	if notifySelf {
		s.Send(protocol.Event(protocol.EvVoiceReset, protocol.VoiceResetEvent{
			ChannelID: channelID,
			Reason:    protocol.ResetFailed,
		}))
	}

	wasHost, remaining, epoch := h.voiceDetach(s.UserID(), channelID)
	h.broadcastVoiceState(s)

	if h.voiceConfig().Mode != protocol.VoiceModeClientHost {
		return
	}

	if !wasHost {
		// An ordinary departure: only the host was connected to them.
		if host := h.sessionOf(remaining, 0); host != nil {
			host.Send(protocol.Event(protocol.EvVoicePeer, protocol.VoicePeerEvent{
				ChannelID: channelID,
				UserID:    s.UserID(),
				Action:    protocol.PeerRemove,
				Epoch:     epoch,
			}))
		}
		return
	}

	// The relay itself left. Everybody was connected to it and to nothing
	// else, so the room starts over: it is cleared, everyone is told to open a
	// new media session, and whoever gets there first hosts the next one. That
	// is a second or so of silence, and it is the honest cost of hosting on a
	// machine that can close its lid.
	h.resetVoiceRoom(channelID, protocol.ResetHostChanged)
}

// resetVoiceRoom empties a room and asks everyone in it to start again.
func (h *Hub) resetVoiceRoom(channelID int64, reason string) {
	members := h.voiceDropRoom(channelID)
	if len(members) == 0 {
		return
	}
	h.voiceMu.Lock()
	// The epoch survives the room so that a client which reconnects instantly
	// cannot be handed a number it has already seen.
	h.voiceEpochs[channelID]++
	h.voiceMu.Unlock()

	event := protocol.Event(protocol.EvVoiceReset, protocol.VoiceResetEvent{
		ChannelID: channelID,
		Reason:    reason,
	})
	for _, userID := range members {
		s, ok := h.SessionForUser(userID)
		if !ok {
			continue
		}
		s.clearVoiceSession(channelID)
		if h.relay != nil {
			h.relay.Leave(channelID, userID)
		}
		s.Send(event)
		h.broadcastVoiceState(s)
	}
	h.log.Info("voice room reset",
		slog.Int64("channel", channelID),
		slog.String("reason", reason),
		slog.Int("members", len(members)))
}

// resetAllVoice starts every room over, which is what a change to the audio
// plane amounts to: the codec parameters live in SDP that was negotiated before
// the change and cannot be edited in place.
func (h *Hub) resetAllVoice(reason string) {
	channels := h.voiceChannels()
	for _, channelID := range channels {
		h.resetVoiceRoom(channelID, reason)
	}
	if h.relay != nil {
		for _, channelID := range channels {
			h.relay.CloseChannel(channelID)
		}
	}
}

// sessionOf returns the live session of the nth entry of a member list.
func (h *Hub) sessionOf(members []int64, index int) *Session {
	if index < 0 || index >= len(members) {
		return nil
	}
	s, ok := h.SessionForUser(members[index])
	if !ok {
		return nil
	}
	return s
}

// onRelayGone turns the relay giving up on a transport into the reset event
// that makes the client open a new session.
func (h *Hub) onRelayGone(channelID, userID int64) {
	s, ok := h.SessionForUser(userID)
	if !ok {
		return
	}
	h.leaveVoice(s, channelID, true)
}
