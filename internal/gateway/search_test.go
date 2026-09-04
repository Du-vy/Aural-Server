package gateway_test

import (
	"context"
	"testing"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// seedMessages writes a run of messages straight to the store. Search tests
// need more history than the send limiter allows over the wire, and none of
// them are testing the act of posting.
func seedMessages(t *testing.T, st *store.Store, channelID, userID int64, contents ...string) []store.Message {
	t.Helper()
	ctx := context.Background()
	out := make([]store.Message, 0, len(contents))
	for _, content := range contents {
		m, err := st.CreateMessage(ctx, channelID, nil, userID, content, nil)
		if err != nil {
			t.Fatalf("seed message %q: %v", content, err)
		}
		out = append(out, m)
	}
	return out
}

func contents(hits []protocol.MessageSearchHit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.Message.Content)
	}
	return out
}

func TestSearchFindsTermsInAnyOrder(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	seedMessages(t, h.store, channel.ID, ready.User.ID,
		"the quick brown fox",
		"a brown bear, quick on its feet",
		"nothing to see here")

	result := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "quick brown"})

	if result.Total != 2 {
		t.Fatalf("total: got %d, want 2; hits %v", result.Total, contents(result.Hits))
	}
	// Newest first is the default order.
	if result.Hits[0].Message.Content != "a brown bear, quick on its feet" {
		t.Fatalf("newest first: got %q", result.Hits[0].Message.Content)
	}
}

func TestSearchIgnoresCaseAndAccents(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	seedMessages(t, h.store, channel.ID, ready.User.ID, "Un CAFÉ con leche")

	for _, query := range []string{"cafe", "CAFÉ", "café con", "cafe con leche"} {
		result := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
			protocol.MessageSearchRequest{Query: query})
		if result.Total != 1 {
			t.Fatalf("query %q: got %d matches, want 1", query, result.Total)
		}
	}
}

func TestSearchMatchesInsideWords(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	// A word index would tokenize this run as one token and find nothing for a
	// fragment of it. A substring search is what makes every script work alike.
	seedMessages(t, h.store, channel.ID, ready.User.ID, "你好世界", "deployment pipeline")

	for query, want := range map[string]string{"世界": "你好世界", "ploy": "deployment pipeline"} {
		result := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
			protocol.MessageSearchRequest{Query: query})
		if result.Total != 1 || result.Hits[0].Message.Content != want {
			t.Fatalf("query %q: got %v, want %q", query, contents(result.Hits), want)
		}
	}
}

func TestSearchTreatsQuotesAsAPhrase(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	seedMessages(t, h.store, channel.ID, ready.User.ID,
		"ship the release today",
		"today we release the ship")

	loose := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "release ship"})
	if loose.Total != 2 {
		t.Fatalf("loose terms: got %d, want 2", loose.Total)
	}

	phrase := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: `"the release"`})
	if phrase.Total != 1 || phrase.Hits[0].Message.Content != "ship the release today" {
		t.Fatalf("phrase: got %v", contents(phrase.Hits))
	}
}

func TestSearchWildcardsAreLiteral(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	seedMessages(t, h.store, channel.ID, ready.User.ID, "battery at 100%", "battery at 90 percent")

	result := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "100%"})
	if result.Total != 1 {
		t.Fatalf("a percent sign is a character, not a wildcard: got %v", contents(result.Hits))
	}
}

func TestSearchFiltersByAuthorAndChannel(t *testing.T) {
	h := newHarness(t, nil)

	alice, aliceReady := h.admin("Alice")
	channel := textChannel(t, aliceReady)
	other := ok[protocol.ChannelEvent](alice, protocol.OpChannelCreate,
		protocol.ChannelCreateRequest{Name: "second", Type: protocol.ChannelText}).Channel

	bob := h.dial()
	bobReady := bob.guest("Bob")

	seedMessages(t, h.store, channel.ID, aliceReady.User.ID, "alice here in the first")
	seedMessages(t, h.store, other.ID, aliceReady.User.ID, "alice here in the second")
	seedMessages(t, h.store, channel.ID, bobReady.User.ID, "bob here in the first")

	byAuthor := ok[protocol.MessageSearchResult](alice, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "here", AuthorIDs: []int64{bobReady.User.ID}})
	if byAuthor.Total != 1 || byAuthor.Hits[0].Message.Content != "bob here in the first" {
		t.Fatalf("from: got %v", contents(byAuthor.Hits))
	}

	byChannel := ok[protocol.MessageSearchResult](alice, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "here", ChannelIDs: []int64{other.ID}})
	if byChannel.Total != 1 || byChannel.Hits[0].Message.Content != "alice here in the second" {
		t.Fatalf("in: got %v", contents(byChannel.Hits))
	}

	both := ok[protocol.MessageSearchResult](alice, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{
			Query:      "here",
			ChannelIDs: []int64{channel.ID},
			AuthorIDs:  []int64{aliceReady.User.ID},
		})
	if both.Total != 1 || both.Hits[0].Message.Content != "alice here in the first" {
		t.Fatalf("in + from: got %v", contents(both.Hits))
	}
}

func TestSearchFiltersByHasLink(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	seedMessages(t, h.store, channel.ID, ready.User.ID,
		"look at https://example.com/thing",
		"look at this instead")

	result := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "look", Has: []string{protocol.HasLink}})
	if result.Total != 1 || result.Hits[0].Message.Content != "look at https://example.com/thing" {
		t.Fatalf("has:link got %v", contents(result.Hits))
	}
}

func TestSearchFiltersByDate(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	seeded := seedMessages(t, h.store, channel.ID, ready.User.ID, "an older thought", "a newer thought")
	// Everything was written in the same second, so the boundary has to be
	// moved rather than waited for.
	if _, err := h.store.DB().ExecContext(context.Background(),
		`UPDATE messages SET created_at = ? WHERE id = ?`, seeded[0].CreatedAt-86_400, seeded[0].ID); err != nil {
		t.Fatalf("age the first message: %v", err)
	}

	recent := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "thought", After: seeded[1].CreatedAt})
	if recent.Total != 1 || recent.Hits[0].Message.Content != "a newer thought" {
		t.Fatalf("after: got %v", contents(recent.Hits))
	}

	old := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "thought", Before: seeded[1].CreatedAt})
	if old.Total != 1 || old.Hits[0].Message.Content != "an older thought" {
		t.Fatalf("before: got %v", contents(old.Hits))
	}
}

func TestSearchSortsAndPages(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	seedMessages(t, h.store, channel.ID, ready.User.ID,
		"target one", "target two", "target three")

	oldest := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "target", Sort: protocol.SortOldest, Limit: 2})
	if len(oldest.Hits) != 2 || oldest.Total != 3 {
		t.Fatalf("first page: got %d of %d", len(oldest.Hits), oldest.Total)
	}
	if oldest.Hits[0].Message.Content != "target one" {
		t.Fatalf("oldest first: got %v", contents(oldest.Hits))
	}

	second := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "target", Sort: protocol.SortOldest, Limit: 2, Offset: 2})
	if len(second.Hits) != 1 || second.Hits[0].Message.Content != "target three" {
		t.Fatalf("second page: got %v", contents(second.Hits))
	}
}

func TestSearchRelevancePrefersThePhraseThenRepetition(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	seedMessages(t, h.store, channel.ID, ready.User.ID,
		"build failed, and the build failed again",
		"the failed build is the one to look at",
		"build something that has not failed")

	result := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "build failed", Sort: protocol.SortRelevance})
	if result.Total != 3 {
		t.Fatalf("total: got %d, want 3", result.Total)
	}
	// The phrase as written wins, then the message that says the words most.
	if result.Hits[0].Message.Content != "build failed, and the build failed again" {
		t.Fatalf("relevance order: got %v", contents(result.Hits))
	}
}

func TestSearchCarriesTheLinesAroundAHit(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	seedMessages(t, h.store, channel.ID, ready.User.ID, "what broke?", "the needle broke", "ah, right")

	result := ok[protocol.MessageSearchResult](c, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "needle"})
	if len(result.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(result.Hits))
	}
	hit := result.Hits[0]
	if hit.Before == nil || hit.Before.Content != "what broke?" {
		t.Fatalf("the line before: %+v", hit.Before)
	}
	if hit.After == nil || hit.After.Content != "ah, right" {
		t.Fatalf("the line after: %+v", hit.After)
	}
}

func TestSearchNeverReachesIntoAnInvisibleChannel(t *testing.T) {
	h := newHarness(t, nil)

	admin, adminReady := h.admin("Admin")
	secret := ok[protocol.ChannelEvent](admin, protocol.OpChannelCreate,
		protocol.ChannelCreateRequest{Name: "secret", Type: protocol.ChannelText}).Channel
	seedMessages(t, h.store, secret.ID, adminReady.User.ID, "the treasure is buried here")

	// Hide it from everyone, which is every guest.
	ok[protocol.ChannelEvent](admin, protocol.OpChannelUpdate, protocol.ChannelUpdateRequest{
		ChannelID: secret.ID,
		Overwrites: []protocol.Overwrite{{
			RoleID: everyoneRole(t, adminReady),
			Allow:  permissions.None.String(),
			Deny:   permissions.ViewChannel.String(),
		}},
	})

	bob := h.dial()
	bob.guest("Bob")

	blind := ok[protocol.MessageSearchResult](bob, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "treasure"})
	if blind.Total != 0 {
		t.Fatalf("a hidden channel must not be searchable: got %v", contents(blind.Hits))
	}

	// Naming the channel outright must not change that, and must not confirm
	// that the channel is there at all.
	named := ok[protocol.MessageSearchResult](bob, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "treasure", ChannelIDs: []int64{secret.ID}})
	if named.Total != 0 {
		t.Fatalf("naming a hidden channel must not reach into it: got %v", contents(named.Hits))
	}

	seeing := ok[protocol.MessageSearchResult](admin, protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "treasure"})
	if seeing.Total != 1 {
		t.Fatalf("the admin should still find it: got %d", seeing.Total)
	}
}

func TestSearchRejectsAnEmptyQuery(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.guest("Alice")

	c.fails(protocol.OpMessageSearch, protocol.MessageSearchRequest{}, protocol.ErrBadRequest)
	c.fails(protocol.OpMessageSearch, protocol.MessageSearchRequest{Query: "   "}, protocol.ErrBadRequest)
	c.fails(protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "x", Has: []string{"sticker"}}, protocol.ErrBadRequest)
	c.fails(protocol.OpMessageSearch,
		protocol.MessageSearchRequest{Query: "x", Sort: "sideways"}, protocol.ErrBadRequest)
}

func TestSearchIsRateLimited(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.guest("Alice")

	limited := false
	for range 12 {
		reply := c.do(protocol.OpMessageSearch, protocol.MessageSearchRequest{Query: "anything"})
		if reply.Op == protocol.OpError && reply.Error.Code == protocol.ErrRateLimited {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("searching should be throttled")
	}
}

func TestHistoryPagesForwardsAndAround(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	ready := c.guest("Alice")
	channel := textChannel(t, ready)

	seeded := seedMessages(t, h.store, channel.ID, ready.User.ID,
		"a", "b", "c", "d", "e", "f", "g", "h", "i")

	around := ok[protocol.MessageHistoryResult](c, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID, Around: seeded[4].ID, Limit: 4})
	if len(around.Messages) != 4 {
		t.Fatalf("around: got %d messages, want 4", len(around.Messages))
	}
	// The anchor is in the page, with history either side of it.
	if around.Messages[2].Content != "e" {
		t.Fatalf("around should centre on the anchor: %v", around.Messages)
	}
	if !around.HasMore || !around.HasMoreAfter {
		t.Fatalf("a window in the middle has history on both sides: %+v", around)
	}

	forward := ok[protocol.MessageHistoryResult](c, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID, After: seeded[6].ID, Limit: 10})
	if len(forward.Messages) != 2 || forward.Messages[0].Content != "h" {
		t.Fatalf("after: got %d messages starting %q", len(forward.Messages), forward.Messages[0].Content)
	}
	if forward.HasMoreAfter {
		t.Fatal("the last page is the present and must say so")
	}

	c.fails(protocol.OpMessageHistory, protocol.MessageHistoryRequest{
		ChannelID: channel.ID, Before: seeded[1].ID, After: seeded[2].ID,
	}, protocol.ErrBadRequest)
}
