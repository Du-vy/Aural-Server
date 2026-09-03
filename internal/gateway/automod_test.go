package gateway

import (
	"strings"
	"testing"

	"github.com/aural-chat/aural-server/internal/protocol"
)

func TestScreenWordsCensorsAcrossCaseAndAccents(t *testing.T) {
	// The list is what a moderator typed; the message is what somebody wrote.
	// One entry has to catch the word however it was spelled, or a word list is
	// defeated by pressing shift.
	words := []string{"idiota"}

	for _, written := range []string{"idiota", "IDIOTA", "Idióta", "ídiota"} {
		masked, found := screenWords("eres un "+written+" total", words, true, protocol.AutoModCensor)
		if !found {
			t.Fatalf("%q was not matched", written)
		}
		if strings.Contains(strings.ToLower(masked), "idiot") {
			t.Fatalf("%q survived censoring: %q", written, masked)
		}
		if !strings.HasPrefix(masked, "eres un ") || !strings.HasSuffix(masked, " total") {
			t.Fatalf("censoring %q disturbed the rest of the sentence: %q", written, masked)
		}
	}
}

func TestScreenWordsLeavesTheRestOfTheSentenceAsItWasWritten(t *testing.T) {
	// The comparison is made on a folded copy, but the replacement has to be
	// made on the original: returning the folded text would quietly strip the
	// accents off every other word.
	masked, found := screenWords("café y tonto y jamón", []string{"tonto"}, true, protocol.AutoModCensor)
	if !found {
		t.Fatal("the listed word was not matched")
	}
	if !strings.Contains(masked, "café") || !strings.Contains(masked, "jamón") {
		t.Fatalf("censoring rewrote the untouched words: %q", masked)
	}
}

func TestScreenWordsHonoursWholeWordMatching(t *testing.T) {
	if _, found := screenWords("classic", []string{"ass"}, true, protocol.AutoModBlock); found {
		t.Fatal("a whole-word rule matched inside a longer word")
	}
	if _, found := screenWords("classic", []string{"ass"}, false, protocol.AutoModBlock); !found {
		t.Fatal("a substring rule missed a match inside a longer word")
	}
}

func TestScreenWordsExpandsFoldingWithoutLosingItsPlace(t *testing.T) {
	// ß folds to "ss", so a folded offset is not an offset into the original.
	// A rule that assumed they were the same would mask the wrong characters.
	masked, found := screenWords("das ist Straße hier", []string{"strasse"}, true, protocol.AutoModCensor)
	if !found {
		t.Fatal("the folded expansion was not matched")
	}
	if !strings.HasPrefix(masked, "das ist ") || !strings.HasSuffix(masked, " hier") {
		t.Fatalf("the mask landed in the wrong place: %q", masked)
	}
}

func TestScreenLinksSparesTheAllowedDomains(t *testing.T) {
	allowed := []string{"github.com"}

	if _, found := screenLinks("see https://github.com/a/b", allowed, protocol.AutoModBlock); found {
		t.Fatal("an allowed domain was refused")
	}
	if _, found := screenLinks("see https://gist.github.com/a", allowed, protocol.AutoModBlock); found {
		t.Fatal("a subdomain of an allowed domain was refused")
	}
	if _, found := screenLinks("see https://evil.example/a", allowed, protocol.AutoModBlock); !found {
		t.Fatal("a domain that is not allowed was let through")
	}
	// A bare host is a link to everybody who reads it, so it counts as one.
	if _, found := screenLinks("go to evil.example/a now", allowed, protocol.AutoModBlock); !found {
		t.Fatal("a link written without a scheme was let through")
	}
	// Not everything with a full stop in it is a host.
	if _, found := screenLinks("hola. que tal", allowed, protocol.AutoModBlock); found {
		t.Fatal("ordinary punctuation was read as a link")
	}
}

func TestShoutingIgnoresShortMessagesAndUncasedScripts(t *testing.T) {
	if shouting("OK", 70, 12) {
		t.Fatal("a short message counted as shouting")
	}
	if !shouting("THIS IS COMPLETELY UNACCEPTABLE", 70, 12) {
		t.Fatal("a message in capitals did not count as shouting")
	}
	if shouting("这是一条很长的中文消息，没有大写字母", 70, 12) {
		t.Fatal("a script with no capitals counted as shouting")
	}
}

func TestCountMentionsSeesBothFormsAndTheBroadcasts(t *testing.T) {
	got := countMentions("hey <@12> and <@!13> and @nombre and @everyone")
	if got != 4 {
		t.Fatalf("counted %d mentions, want 4", got)
	}
}

func TestNormaliseAutoModForcesBlockWhereThereIsNothingToMask(t *testing.T) {
	cfg := protocol.DefaultAutoMod()
	cfg.Mentions.Action = protocol.AutoModCensor
	cfg.Caps.Action = protocol.AutoModCensor
	cfg.Words.Action = protocol.AutoModCensor

	out := normaliseAutoMod(cfg)
	if out.Mentions.Action != protocol.AutoModBlock {
		t.Fatal("a mention rule was left censoring, which it cannot do")
	}
	if out.Caps.Action != protocol.AutoModBlock {
		t.Fatal("a caps rule was left censoring, which it cannot do")
	}
	if out.Words.Action != protocol.AutoModCensor {
		t.Fatal("a word rule lost the action it can actually perform")
	}
}

func TestNormaliseAutoModDropsDuplicatesAndKeepsListsNonNull(t *testing.T) {
	cfg := protocol.DefaultAutoMod()
	cfg.Words.Words = []string{"tonto", "TONTO", "  ", "tónto", "otra"}
	cfg.ExemptRoles = []int64{4, 4, 0, -1, 5}

	out := normaliseAutoMod(cfg)
	if len(out.Words.Words) != 2 {
		t.Fatalf("word list came out as %v, want the two distinct entries", out.Words.Words)
	}
	if len(out.ExemptRoles) != 2 {
		t.Fatalf("exempt roles came out as %v, want [4 5]", out.ExemptRoles)
	}
	if out.Links.AllowedDomains == nil {
		t.Fatal("an empty domain list came back as null rather than an empty array")
	}
}
