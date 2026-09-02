package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/voice"
)

// maxSignalTracks bounds the media-id map a host may attach to an offer. It is
// generous next to any channel a relaying browser could actually carry, and it
// is here so that a map the server never reads still cannot be made unbounded.
const maxSignalTracks = 128

// The voice ops.
//
// Every one of them starts from the same question — is the caller sitting in a
// voice channel this server will carry audio for — so that question is asked
// once, by requireVoiceChannel, and the handlers below are about what happens
// after it.

// requireVoiceChannel resolves the channel a voice op applies to: the one the
// caller is already in, having got there through user.move.
//
// Membership of the channel is not re-derived from permissions here, because
// user.move already checked them and eviction already handles losing them. What
// is checked is that the channel still exists, is still a voice channel, and is
// the one the caller named — a client that has drifted out of step with the
// server is told so rather than quietly acting on the wrong room.
func (h *Hub) requireVoiceChannel(s *Session, channelID int64) (int64, *protocol.Error) {
	cfg := h.voiceConfig()
	if !cfg.Enabled || h.relay == nil {
		return 0, protocol.Errorf(protocol.ErrVoiceDisabled, "this server does not carry voice")
	}

	current := s.ChannelID()
	if current == nil {
		return 0, protocol.Errorf(protocol.ErrBadRequest, "join the voice channel before opening audio in it")
	}
	if channelID != 0 && channelID != *current {
		return 0, protocol.Errorf(protocol.ErrConflict, "you are not in that voice channel")
	}
	ch, ok := h.Channel(*current)
	if !ok {
		return 0, protocol.Errorf(protocol.ErrNotFound, "no such channel")
	}
	if ch.Type != protocol.ChannelVoice {
		return 0, protocol.Errorf(protocol.ErrBadRequest, "that is not a voice channel")
	}
	return *current, nil
}

// handleVoiceConnect opens a media session in the channel the caller is in.
//
// It is also the way back from every failure: a reset, a lost transport, a host
// that went away all end with the client calling this again. Making it the one
// entry point means the recovery path is the path everybody takes on every
// call, which is the only way to be confident it works.
func handleVoiceConnect(_ context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.VoiceConnectRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if !s.signals.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are opening voice sessions too quickly")
	}

	channelID, failure := s.hub.requireVoiceChannel(s, req.ChannelID)
	if failure != nil {
		return nil, failure
	}
	cfg := s.hub.voiceConfig()

	// Opening a session twice over replaces the first, so the participant is
	// taken out of the room before being let back in. Without this a client
	// that reconnected after a network blip would be counted twice and, in
	// client_host mode, could elect itself over the host it just lost.
	if previous := s.voiceChannel(); previous != 0 {
		s.hub.leaveVoice(s, previous, false)
	}

	if cfg.MaxParticipants > 0 {
		if _, _, count := s.hub.voiceRoomState(channelID); count >= cfg.MaxParticipants {
			return nil, protocol.Errorf(protocol.ErrConflict, "that channel is carrying as much audio as this server allows")
		}
	}

	result := protocol.VoiceConnectResult{
		ChannelID:  channelID,
		Mode:       cfg.Mode,
		ICEServers: s.hub.iceServers(),
		Voice:      s.hub.voiceInfo(),
	}

	switch cfg.Mode {
	case protocol.VoiceModeServerHost:
		if req.SDP == "" {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "a server-hosted session needs your offer")
		}
		// The relay starts gathering the moment it has an answer, so its first
		// candidates can reach the client before this very reply does: both
		// travel on the same socket, and the reply is only sent once this
		// handler returns. A client therefore has to hold a signalling frame it
		// is not ready for rather than discard it — which it has to do anyway,
		// since the same race exists between two peers in client_host mode.
		answer, err := s.hub.relay.Join(channelID, s.UserID(), req.SDP, func(sig voice.Signal) {
			s.Send(protocol.Event(protocol.EvVoiceSignal, protocol.VoiceSignalEvent{
				FromUserID: protocol.ServerPeer,
				ChannelID:  channelID,
				Kind:       sig.Kind,
				SDP:        sig.SDP,
				Candidate:  sig.Candidate,
			}))
		})
		if err != nil {
			s.log.Warn("open server-hosted voice session",
				slog.Int64("channel", channelID), slog.Any("error", err))
			return nil, protocol.Errorf(protocol.ErrVoiceFailed, "that voice session could not be opened")
		}
		result.SDP = answer

	case protocol.VoiceModeClientHost:
		if req.SDP != "" {
			return nil, protocol.Errorf(protocol.ErrBadRequest,
				"a client-hosted session is offered by the host, not by the server")
		}

	default:
		return nil, protocol.Errorf(protocol.ErrVoiceDisabled, "this server does not carry voice")
	}

	if !s.openVoiceSession(channelID) {
		s.hub.relay.Leave(channelID, s.UserID())
		return nil, protocol.Errorf(protocol.ErrConflict, "you already hold a voice session elsewhere")
	}
	host, epoch, _ := s.hub.voiceAttach(s.UserID(), channelID)

	// A moderated mute survives reconnecting, so the relay has to be told about
	// it again on the session that replaces the one it was applied to.
	state, _ := s.hub.voiceStateOf(s)
	s.hub.applyVoiceEnforcement(s, channelID, state.Muted(), state.Deafened())

	if cfg.Mode == protocol.VoiceModeClientHost {
		result.HostUserID = &host
		result.HostEpoch = epoch
		if host != s.UserID() {
			// The host dials; the arrival waits. Telling the host is what
			// starts that, and it is the only frame the arrival is owed.
			if hostSession, ok := s.hub.SessionForUser(host); ok {
				hostSession.Send(protocol.Event(protocol.EvVoicePeer, protocol.VoicePeerEvent{
					ChannelID: channelID,
					UserID:    s.UserID(),
					Action:    protocol.PeerAdd,
					Epoch:     epoch,
				}))
			}
		}
	}

	result.Participants = s.hub.voiceParticipants(s, channelID)
	s.hub.broadcastVoiceState(s)
	if cfg.Mode == protocol.VoiceModeClientHost {
		s.hub.broadcastVoiceHost(channelID, host, epoch)
	}

	s.log.Info("voice session opened",
		slog.Int64("channel", channelID),
		slog.String("mode", cfg.Mode))
	return result, nil
}

// handleVoiceLeave closes the media session without leaving the channel, which
// is what a client does when its microphone is taken away or when it decides to
// sit in the channel silently.
func handleVoiceLeave(_ context.Context, s *Session, _ json.RawMessage) (any, *protocol.Error) {
	if channelID := s.voiceChannel(); channelID != 0 {
		s.hub.leaveVoice(s, channelID, false)
	}
	return struct{}{}, nil
}

// handleVoiceSignal moves one SDP or ICE frame towards its peer.
//
// In server_host mode the only peer is the relay. In client_host mode a frame
// may only travel between a participant and the channel's host: the topology is
// a star, and a client that tries to signal a third party is trying to build
// something the server has not agreed to carry.
func handleVoiceSignal(_ context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.VoiceSignalRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if !s.signals.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are signalling too quickly")
	}
	switch req.Kind {
	case protocol.SignalOffer, protocol.SignalAnswer, protocol.SignalCandidate, protocol.SignalEnd:
	default:
		return nil, protocol.Errorf(protocol.ErrBadRequest, "unknown signalling frame")
	}
	// The track map is relayed unread, so its size is the one thing about it
	// worth checking: a channel cannot hold more people than the server can.
	if len(req.Tracks) > maxSignalTracks {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "that track map names more media than a channel can hold")
	}

	channelID := s.voiceChannel()
	if channelID == 0 {
		return nil, protocol.Errorf(protocol.ErrConflict, "you have no voice session open")
	}
	cfg := s.hub.voiceConfig()

	if cfg.Mode == protocol.VoiceModeServerHost {
		if req.TargetID != protocol.ServerPeer {
			return nil, protocol.Errorf(protocol.ErrBadRequest,
				"a server-hosted session signals the server, not another client")
		}
		if s.hub.relay == nil {
			return nil, protocol.Errorf(protocol.ErrVoiceDisabled, "this server does not carry voice")
		}
		err := s.hub.relay.Accept(channelID, s.UserID(), voice.Signal{
			Kind:      req.Kind,
			SDP:       req.SDP,
			Candidate: req.Candidate,
		})
		switch {
		case errors.Is(err, voice.ErrNoSession):
			// The session was torn down between the frame being sent and
			// arriving, which is ordinary. The reset the client already has
			// tells it what to do; there is nothing to report here.
			return struct{}{}, nil
		case err != nil:
			s.log.Debug("apply voice signal", slog.Any("error", err))
			return nil, protocol.Errorf(protocol.ErrVoiceFailed, "that signalling frame could not be applied")
		}
		return struct{}{}, nil
	}

	// client_host.
	if req.TargetID == protocol.ServerPeer {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"a client-hosted session signals the host, not the server")
	}
	host, _, _ := s.hub.voiceRoomState(channelID)
	if req.TargetID != host && s.UserID() != host {
		return nil, protocol.Errorf(protocol.ErrForbidden,
			"in this mode audio goes through the host, so signalling does too")
	}

	target, ok := s.hub.SessionForUser(req.TargetID)
	if !ok || target.voiceChannel() != channelID {
		return nil, protocol.Errorf(protocol.ErrNotFound, "that peer is not in this voice session")
	}
	target.Send(protocol.Event(protocol.EvVoiceSignal, protocol.VoiceSignalEvent{
		FromUserID: s.UserID(),
		ChannelID:  channelID,
		Kind:       req.Kind,
		SDP:        req.SDP,
		Candidate:  req.Candidate,
		Tracks:     req.Tracks,
	}))
	return struct{}{}, nil
}

// handleVoiceState sets the caller's own mute and deafen.
func handleVoiceState(_ context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.VoiceStateRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if req.SelfMute == nil && req.SelfDeaf == nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "nothing to update")
	}

	// Muting yourself works whether or not audio is up: a client mutes before
	// it connects, and the interface has to be able to say so.
	v := s.setSelfVoice(req.SelfMute, req.SelfDeaf)

	state, ok := s.hub.voiceStateOf(s)
	if !ok {
		return protocol.VoiceStateEvent{State: protocol.VoiceState{UserID: s.UserID()}}, nil
	}
	s.hub.applyVoiceEnforcement(s, v.channelID, state.Muted(), state.Deafened())
	s.hub.broadcastVoiceState(s)
	return protocol.VoiceStateEvent{State: state}, nil
}

// handleVoiceModerate mutes or deafens somebody else.
func handleVoiceModerate(_ context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.VoiceModerateRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if req.Mute == nil && req.Deaf == nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "nothing to update")
	}

	base, _ := s.Permissions()
	if req.Mute != nil && !base.Has(permissions.MuteUsers) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to mute users")
	}
	if req.Deaf != nil && !base.Has(permissions.DeafenUsers) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to deafen users")
	}
	if req.UserID == s.UserID() {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "moderate somebody else; your own mute is voice.state")
	}

	target, ok := s.hub.SessionForUser(req.UserID)
	if !ok {
		return nil, protocol.Errorf(protocol.ErrNotFound, "that user is not connected")
	}
	if failure := s.hub.requireOutranks(s, req.UserID); failure != nil {
		return nil, failure
	}
	// Muting somebody in a channel you cannot see would be acting on a room you
	// are not in, and the reply would confirm they are in it.
	channelID := target.ChannelID()
	if channelID == nil || !s.hub.SessionCanView(s, *channelID) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "that user is not in a voice channel")
	}

	target.setModeratedVoice(req.Mute, req.Deaf)
	state, ok := s.hub.voiceStateOf(target)
	if !ok {
		return nil, protocol.Errorf(protocol.ErrNotFound, "that user is not in a voice channel")
	}
	s.hub.applyVoiceEnforcement(target, target.voiceChannel(), state.Muted(), state.Deafened())
	s.hub.broadcastVoiceState(target)

	s.log.Info("voice moderation",
		slog.Int64("by", s.UserID()),
		slog.Int64("user", req.UserID),
		slog.Bool("mute", state.Mute),
		slog.Bool("deaf", state.Deaf))
	return protocol.VoiceStateEvent{State: state}, nil
}

// handleVoiceSpeaking fans out a speaking transition.
//
// The client decides when it is speaking, because in client_host mode the
// server never sees the audio and could not decide it there. Reporting it the
// same way in both modes means one code path and one source of truth, at the
// cost of trusting a client about something it can only lie about to its own
// channel, and about itself.
func handleVoiceSpeaking(_ context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.VoiceSpeakingRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if !s.speaking.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are toggling speaking too quickly")
	}

	channelID := s.voiceChannel()
	if channelID == 0 {
		return struct{}{}, nil
	}

	speaking := req.Speaking
	if state, ok := s.hub.voiceStateOf(s); ok && state.Muted() {
		// A muted participant is not speaking, whatever their client thinks.
		speaking = false
	}
	if !s.setSpeaking(speaking) {
		return struct{}{}, nil
	}

	if HidesPresence(s.User().Status) {
		return struct{}{}, nil
	}
	s.hub.BroadcastChannelEvent(protocol.Event(protocol.EvVoiceSpeaking, protocol.VoiceSpeakingEvent{
		UserID:    s.UserID(),
		ChannelID: channelID,
		Speaking:  speaking,
	}), channelID)
	return struct{}{}, nil
}

// --- helpers ----------------------------------------------------------------

// applyVoiceEnforcement tells the relay what it must not forward.
//
// It is the half of muting that does not depend on the client agreeing. In
// client_host mode there is no relay to tell: the host enforces it, and a host
// that does not is a client running modified code — which is worth saying
// plainly rather than pretending otherwise.
func (h *Hub) applyVoiceEnforcement(s *Session, channelID int64, muted, deafened bool) {
	if h.relay == nil || channelID == 0 {
		return
	}
	h.relay.SetMuted(channelID, s.UserID(), muted)
	h.relay.SetDeafened(channelID, s.UserID(), deafened)
}

// voiceParticipants is the voice state of everybody in a channel, as one viewer
// may see it. It is what a joining client is handed so it does not have to
// reconstruct the room from events sent before it was listening.
func (h *Hub) voiceParticipants(viewer *Session, channelID int64) []protocol.VoiceState {
	sessions := h.Sessions()
	out := make([]protocol.VoiceState, 0, len(sessions))
	for _, other := range sessions {
		if other.UserID() != viewer.UserID() && HidesPresence(other.User().Status) {
			continue
		}
		state, ok := h.voiceStateOf(other)
		if !ok || state.ChannelID != channelID {
			continue
		}
		out = append(out, state)
	}
	return out
}

// broadcastVoiceHost announces who relays a client-hosted channel.
func (h *Hub) broadcastVoiceHost(channelID, host int64, epoch int64) {
	event := protocol.VoiceHostEvent{ChannelID: channelID, Epoch: epoch}
	if host != 0 {
		event.HostUserID = &host
	}
	h.BroadcastChannelEvent(protocol.Event(protocol.EvVoiceHost, event), channelID)
}
