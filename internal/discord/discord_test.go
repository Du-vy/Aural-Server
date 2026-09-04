package discord

import (
	"strings"
	"testing"
	"time"
)

func TestParseWebhookURLAcceptsEveryShapeDiscordHandsOut(t *testing.T) {
	const (
		wantID    = "1234567890123456789"
		wantToken = "aB3-_xYz09"
	)
	cases := []struct {
		name string
		in   string
	}{
		{"the one the copy button gives you",
			"https://discord.com/api/webhooks/1234567890123456789/aB3-_xYz09"},
		{"with an explicit API version",
			"https://discord.com/api/v10/webhooks/1234567890123456789/aB3-_xYz09"},
		{"on the old domain",
			"https://discordapp.com/api/webhooks/1234567890123456789/aB3-_xYz09"},
		{"with a query string somebody copied along",
			"https://discord.com/api/webhooks/1234567890123456789/aB3-_xYz09?wait=true"},
		{"with the whitespace of a paste",
			"  https://discord.com/api/webhooks/1234567890123456789/aB3-_xYz09\n"},
		{"on a test instance",
			"https://ptb.discord.com/api/webhooks/1234567890123456789/aB3-_xYz09"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, token, err := ParseWebhookURL(tc.in)
			if err != nil {
				t.Fatalf("ParseWebhookURL(%q): %v", tc.in, err)
			}
			if id != wantID || token != wantToken {
				t.Fatalf("got %q/%q, want %q/%q", id, token, wantID, wantToken)
			}
		})
	}
}

func TestParseWebhookURLRefusesWhatIsNotOne(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"not a URL at all", "MTIzNDU2Nzg5.GaBcDe.token-looking-thing"},
		{"somebody else's host", "https://example.com/api/webhooks/123456789012345678/tok"},
		{"a Discord URL that is not a webhook", "https://discord.com/channels/123/456"},
		{"missing the token", "https://discord.com/api/webhooks/1234567890123456789"},
		{"an id that is not a number", "https://discord.com/api/webhooks/not-an-id/token"},
		{"an unusable scheme", "ftp://discord.com/api/webhooks/1234567890123456789/tok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseWebhookURL(tc.in); err == nil {
				t.Fatalf("ParseWebhookURL(%q) was accepted", tc.in)
			}
		})
	}
}

func TestRenderContentResolvesEveryKindOfMention(t *testing.T) {
	m := Message{
		Content: "hey <@111111111111111111> and <@!222222222222222222>, see <#333333333333333333> " +
			"about <@&444444444444444444> <:party:555555555555555555> <a:wave:666666666666666666>",
		Mentions: []User{
			{ID: "111111111111111111", Username: "pablo", GlobalName: "Pablo"},
			{ID: "222222222222222222", Username: "ana"},
		},
	}
	got := RenderContent(m, stubResolver{
		roles:    map[string]string{"444444444444444444": "moderators"},
		channels: map[string]string{"333333333333333333": "general"},
	})

	want := "hey @Pablo and @ana, see #general about @moderators :party: :wave:"
	if got != want {
		t.Fatalf("RenderContent:\n got %q\nwant %q", got, want)
	}
}

func TestRenderContentDegradesRatherThanLeavingAHole(t *testing.T) {
	// Nothing resolves: no mentions array, no cache. A reader should still get
	// a sentence that says somebody was named, rather than one with a gap in
	// it or a wall of digits.
	m := Message{Content: "ping <@111111111111111111> and <@&222222222222222222> in <#333333333333333333>"}
	got := RenderContent(m, stubResolver{})

	want := "ping @unknown-user and @unknown-role in #unknown-channel"
	if got != want {
		t.Fatalf("RenderContent:\n got %q\nwant %q", got, want)
	}
}

func TestRenderContentWritesTimestampsAsText(t *testing.T) {
	// 2021-01-01T00:00:00Z. A Discord client renders this in the reader's own
	// zone, which plain text cannot do, so it has to become something fixed.
	m := Message{Content: "starts <t:1609459200:F> and again <t:1609459200:d>"}
	got := RenderContent(m, nil)

	if strings.Contains(got, "<t:") {
		t.Fatalf("a timestamp survived unrendered: %q", got)
	}
	if !strings.Contains(got, "2021-01-01") {
		t.Fatalf("the short style did not render: %q", got)
	}
	if !strings.Contains(got, "1 January 2021") {
		t.Fatalf("the long style did not render: %q", got)
	}
}

func TestEscapeOutboundDefangsWhatDiscordWouldActOn(t *testing.T) {
	got := EscapeOutbound("hey @everyone and @here, also <@123456789012345678> in <#987654321098765432>")

	for _, forbidden := range []string{"@everyone", "@here", "<@1", "<#9"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("%q survived escaping: %q", forbidden, got)
		}
	}
	// The words themselves must survive: this defangs syntax, it does not
	// censor anybody.
	for _, kept := range []string{"everyone", "here", "hey", "also"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("%q was lost: %q", kept, got)
		}
	}
}

func TestTruncateRunesNeverCutsMidCharacter(t *testing.T) {
	if got := TruncateRunes("hello", 10); got != "hello" {
		t.Fatalf("a short string was altered: %q", got)
	}
	if got := TruncateRunes("hello", 5); got != "hello" {
		t.Fatalf("a string exactly at the limit was altered: %q", got)
	}

	// Every rune here is multi-byte; a cut by bytes would produce mojibake.
	got := TruncateRunes("áéíóúñ", 4)
	if n := len([]rune(got)); n != 4 {
		t.Fatalf("TruncateRunes returned %d runes, want 4: %q", n, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a truncated string did not say so: %q", got)
	}
	if !strings.HasPrefix(got, "áéí") {
		t.Fatalf("TruncateRunes cut inside a character: %q", got)
	}
}

func TestDisplayNameFollowsTheOrderDiscordItselfUses(t *testing.T) {
	nick := "Mod Pablo"
	cases := []struct {
		name string
		in   Message
		want string
	}{
		{"the guild nickname wins", Message{
			Author: User{Username: "pablo", GlobalName: "Pablo"},
			Member: &Member{Nick: &nick},
		}, "Mod Pablo"},
		{"then the display name", Message{
			Author: User{Username: "pablo", GlobalName: "Pablo"},
		}, "Pablo"},
		{"then the handle", Message{
			Author: User{Username: "pablo"},
		}, "pablo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.DisplayName(); got != tc.want {
				t.Fatalf("DisplayName: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAvatarURLAlwaysProducesSomethingLoadable(t *testing.T) {
	hash := "abc123"
	animated := "a_abc123"
	guildAvatar := "def456"

	cases := []struct {
		name     string
		in       Message
		contains []string
	}{
		{"the per-guild picture wins", Message{
			GuildID: "999", Author: User{ID: "111", Avatar: &hash},
			Member: &Member{Avatar: &guildAvatar},
		}, []string{"/guilds/999/users/111/avatars/def456.png"}},
		{"an animated avatar is asked for as a gif", Message{
			Author: User{ID: "111", Avatar: &animated},
		}, []string{"/avatars/111/a_abc123.gif"}},
		{"an account with none falls back to a default", Message{
			Author: User{ID: "111", Discriminator: "0"},
		}, []string{"/embed/avatars/"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.AvatarURL(128)
			if !strings.HasPrefix(got, "https://") {
				t.Fatalf("AvatarURL is not absolute: %q", got)
			}
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Fatalf("AvatarURL %q does not contain %q", got, want)
				}
			}
		})
	}
}

func TestDefaultAvatarIndexHandlesBothGenerationsOfAccount(t *testing.T) {
	// A legacy account is indexed by its discriminator, modulo five.
	if got := defaultAvatarIndex(User{ID: "1", Discriminator: "0007"}); got != 2 {
		t.Fatalf("legacy index: got %d, want 2", got)
	}
	// A migrated one has no discriminator and is indexed by its snowflake.
	if got := defaultAvatarIndex(User{ID: "80351110224678912", Discriminator: "0"}); got < 0 || got > 5 {
		t.Fatalf("modern index out of range: %d", got)
	}
	// Nothing parseable at all still has to render.
	if got := defaultAvatarIndex(User{ID: "not-a-number", Discriminator: "0"}); got != 0 {
		t.Fatalf("unparseable id: got %d, want 0", got)
	}
}

func TestRelayableSkipsDiscordsOwnNotices(t *testing.T) {
	if !(Message{Type: MessageTypeDefault}).Relayable() {
		t.Fatal("a plain message is not relayable")
	}
	if !(Message{Type: MessageTypeReply}).Relayable() {
		t.Fatal("a reply is not relayable")
	}
	// 7 is USER_JOIN, 6 is CHANNEL_PINNED_MESSAGE. Neither is something
	// somebody typed into a bridged channel.
	for _, kind := range []int{6, 7, 8, 12} {
		if (Message{Type: kind}).Relayable() {
			t.Fatalf("message type %d was treated as relayable", kind)
		}
	}
}

func TestWritableCoversTheChannelsAWebhookCanPostIn(t *testing.T) {
	for _, kind := range []int{ChannelGuildText, ChannelGuildAnnouncement, ChannelPublicThread} {
		if !(Channel{Type: kind}).Writable() {
			t.Fatalf("channel type %d should be linkable", kind)
		}
	}
	if (Channel{Type: ChannelGuildCategory}).Writable() {
		t.Fatal("a category was offered as a link target")
	}
}

func TestFormatTimestampCoversEveryStyle(t *testing.T) {
	const instant = 1609459200 // 2021-01-01T00:00:00Z
	for _, style := range []string{"t", "T", "d", "D", "f", "F", ""} {
		if got := formatTimestamp(instant, style); got == "" {
			t.Fatalf("style %q rendered nothing", style)
		}
	}
	// R is relative to now, so it is checked against a moving target rather
	// than a fixed string.
	future := time.Now().Add(3 * time.Hour).Unix()
	if got := formatTimestamp(future, "R"); !strings.Contains(got, "from now") {
		t.Fatalf("relative style: got %q", got)
	}
}

// stubResolver answers the two lookups RenderContent makes.
type stubResolver struct {
	roles    map[string]string
	channels map[string]string
}

func (s stubResolver) RoleName(id string) (string, bool) {
	name, ok := s.roles[id]
	return name, ok
}

func (s stubResolver) ChannelName(id string) (string, bool) {
	name, ok := s.channels[id]
	return name, ok
}
