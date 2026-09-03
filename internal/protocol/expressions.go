package protocol

// The two kinds of expression a server carries.
const (
	ExpressionEmoji   = "emoji"
	ExpressionSticker = "sticker"
)

// Expression is one custom emoji or sticker.
//
// An emoji is written into a message as `:name:` and rendered inline; a
// sticker is sent instead of a message and rendered whole. They are one type
// because they are one namespace with one management screen behind it.
type Expression struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// URL is relative to the server, exactly as an attachment's is, so a client
	// reaching this server by address, by hostname or through a proxy all build
	// the same working link from the address they already hold.
	URL string `json:"url"`
	// Animated is what tells a client it may want to hold this one still until
	// it is hovered, and what stops it trying to resize it as a static image.
	Animated  bool   `json:"animated"`
	Size      string `json:"size"`
	CreatorID *int64 `json:"creatorId"`
	CreatedAt int64  `json:"createdAt"`
}

// Sound is one soundboard clip.
type Sound struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Emoji string `json:"emoji"`
	URL   string `json:"url"`
	// DurationMs is what the panel shows and what a client uses to decide when
	// the button stops looking pressed.
	DurationMs int    `json:"durationMs"`
	Volume     int    `json:"volume"`
	Size       string `json:"size"`
	CreatorID  *int64 `json:"creatorId"`
	CreatedAt  int64  `json:"createdAt"`
}

// ExpressionUpdateRequest renames one. The picture itself is never edited:
// replacing it is a delete and an upload, which is what keeps a URL that a
// client has cached pointing at what it cached.
type ExpressionUpdateRequest struct {
	ExpressionID int64  `json:"expressionId"`
	Name         string `json:"name"`
}

type ExpressionDeleteRequest struct {
	ExpressionID int64 `json:"expressionId"`
}

type ExpressionEvent struct {
	Expression Expression `json:"expression"`
}

type ExpressionDeletedEvent struct {
	ExpressionID int64  `json:"expressionId"`
	Kind         string `json:"kind"`
}

type SoundUpdateRequest struct {
	SoundID int64   `json:"soundId"`
	Name    *string `json:"name,omitempty"`
	Emoji   *string `json:"emoji,omitempty"`
	Volume  *int    `json:"volume,omitempty"`
}

type SoundDeleteRequest struct {
	SoundID int64 `json:"soundId"`
}

// SoundPlayRequest plays a clip at the voice channel the caller is sitting in.
// The channel is not named: it is wherever they are, and playing a sound into
// a room you are not in is not a thing anybody should be able to do.
type SoundPlayRequest struct {
	SoundID int64 `json:"soundId"`
}

type SoundEvent struct {
	Sound Sound `json:"sound"`
}

type SoundDeletedEvent struct {
	SoundID int64 `json:"soundId"`
}

// SoundPlayedEvent reaches everybody in the channel. Each client fetches the
// clip and mixes it into its own output rather than the sound being injected
// into somebody's microphone, which is what makes it sound the same to
// everybody and work identically whoever is relaying the call.
type SoundPlayedEvent struct {
	SoundID   int64 `json:"soundId"`
	UserID    int64 `json:"userId"`
	ChannelID int64 `json:"channelId"`
}
