package store

// Case and accent folding for search.
//
// A search compares a folded copy of a message against a folded copy of what
// was typed, so "Café" is found by "cafe" and by "CAFÉ". Folding is done here,
// in Go, rather than in SQL, because SQLite's own LOWER() only knows ASCII.
//
// Only the Latin blocks are folded. That is a deliberate limit rather than an
// unfinished one: stripping combining marks is right for Latin, where they are
// accents on a letter, and wrong for Devanagari, where the very same Unicode
// category holds vowels that change the word. Scripts outside these blocks are
// left exactly as they were written, which is what makes a substring search
// behave the same in every language the client is translated into.

import "strings"

// foldings maps the accented Latin letters of the Latin-1 Supplement and Latin
// Extended-A and B blocks down to the plain letters underneath them. Letters
// whose fold is more than one letter — ß, æ, œ — expand, which is how a reader
// of those languages would spell them out anyway.
var foldings = map[rune]string{
	'\u00c0': "a", '\u00c1': "a", '\u00c2': "a", '\u00c3': "a", '\u00c4': "a", '\u00c5': "a",
	'\u00c6': "ae", '\u00c7': "c", '\u00c8': "e", '\u00c9': "e", '\u00ca': "e", '\u00cb': "e",
	'\u00cc': "i", '\u00cd': "i", '\u00ce': "i", '\u00cf': "i", '\u00d0': "d", '\u00d1': "n",
	'\u00d2': "o", '\u00d3': "o", '\u00d4': "o", '\u00d5': "o", '\u00d6': "o", '\u00d8': "o",
	'\u00d9': "u", '\u00da': "u", '\u00db': "u", '\u00dc': "u", '\u00dd': "y", '\u00de': "th",
	'\u00df': "ss", '\u00e0': "a", '\u00e1': "a", '\u00e2': "a", '\u00e3': "a", '\u00e4': "a",
	'\u00e5': "a", '\u00e6': "ae", '\u00e7': "c", '\u00e8': "e", '\u00e9': "e", '\u00ea': "e",
	'\u00eb': "e", '\u00ec': "i", '\u00ed': "i", '\u00ee': "i", '\u00ef': "i", '\u00f0': "d",
	'\u00f1': "n", '\u00f2': "o", '\u00f3': "o", '\u00f4': "o", '\u00f5': "o", '\u00f6': "o",
	'\u00f8': "o", '\u00f9': "u", '\u00fa': "u", '\u00fb': "u", '\u00fc': "u", '\u00fd': "y",
	'\u00fe': "th", '\u00ff': "y", '\u0100': "a", '\u0101': "a", '\u0102': "a", '\u0103': "a",
	'\u0104': "a", '\u0105': "a", '\u0106': "c", '\u0107': "c", '\u0108': "c", '\u0109': "c",
	'\u010a': "c", '\u010b': "c", '\u010c': "c", '\u010d': "c", '\u010e': "d", '\u010f': "d",
	'\u0110': "d", '\u0111': "d", '\u0112': "e", '\u0113': "e", '\u0114': "e", '\u0115': "e",
	'\u0116': "e", '\u0117': "e", '\u0118': "e", '\u0119': "e", '\u011a': "e", '\u011b': "e",
	'\u011c': "g", '\u011d': "g", '\u011e': "g", '\u011f': "g", '\u0120': "g", '\u0121': "g",
	'\u0122': "g", '\u0123': "g", '\u0124': "h", '\u0125': "h", '\u0126': "h", '\u0127': "h",
	'\u0128': "i", '\u0129': "i", '\u012a': "i", '\u012b': "i", '\u012c': "i", '\u012d': "i",
	'\u012e': "i", '\u012f': "i", '\u0130': "i", '\u0131': "i", '\u0132': "ij", '\u0133': "ij",
	'\u0134': "j", '\u0135': "j", '\u0136': "k", '\u0137': "k", '\u0138': "k", '\u0139': "l",
	'\u013a': "l", '\u013b': "l", '\u013c': "l", '\u013d': "l", '\u013e': "l", '\u0141': "l",
	'\u0142': "l", '\u0143': "n", '\u0144': "n", '\u0145': "n", '\u0146': "n", '\u0147': "n",
	'\u0148': "n", '\u014a': "n", '\u014b': "n", '\u014c': "o", '\u014d': "o", '\u014e': "o",
	'\u014f': "o", '\u0150': "o", '\u0151': "o", '\u0152': "oe", '\u0153': "oe", '\u0154': "r",
	'\u0155': "r", '\u0156': "r", '\u0157': "r", '\u0158': "r", '\u0159': "r", '\u015a': "s",
	'\u015b': "s", '\u015c': "s", '\u015d': "s", '\u015e': "s", '\u015f': "s", '\u0160': "s",
	'\u0161': "s", '\u0162': "t", '\u0163': "t", '\u0164': "t", '\u0165': "t", '\u0166': "t",
	'\u0167': "t", '\u0168': "u", '\u0169': "u", '\u016a': "u", '\u016b': "u", '\u016c': "u",
	'\u016d': "u", '\u016e': "u", '\u016f': "u", '\u0170': "u", '\u0171': "u", '\u0172': "u",
	'\u0173': "u", '\u0174': "w", '\u0175': "w", '\u0176': "y", '\u0177': "y", '\u0178': "y",
	'\u0179': "z", '\u017a': "z", '\u017b': "z", '\u017c': "z", '\u017d': "z", '\u017e': "z",
	'\u017f': "s", '\u01a0': "o", '\u01a1': "o", '\u01af': "u", '\u01b0': "u", '\u01cd': "a",
	'\u01ce': "a", '\u01cf': "i", '\u01d0': "i", '\u01d1': "o", '\u01d2': "o", '\u01d3': "u",
	'\u01d4': "u", '\u01d5': "u", '\u01d6': "u", '\u01d7': "u", '\u01d8': "u", '\u01d9': "u",
	'\u01da': "u", '\u01db': "u", '\u01dc': "u", '\u01de': "a", '\u01df': "a", '\u01e0': "a",
	'\u01e1': "a", '\u01e6': "g", '\u01e7': "g", '\u01e8': "k", '\u01e9': "k", '\u01ea': "o",
	'\u01eb': "o", '\u01ec': "o", '\u01ed': "o", '\u01f0': "j", '\u01f4': "g", '\u01f5': "g",
	'\u01f8': "n", '\u01f9': "n", '\u01fa': "a", '\u01fb': "a", '\u0200': "a", '\u0201': "a",
	'\u0202': "a", '\u0203': "a", '\u0204': "e", '\u0205': "e", '\u0206': "e", '\u0207': "e",
	'\u0208': "i", '\u0209': "i", '\u020a': "i", '\u020b': "i", '\u020c': "o", '\u020d': "o",
	'\u020e': "o", '\u020f': "o", '\u0210': "r", '\u0211': "r", '\u0212': "r", '\u0213': "r",
	'\u0214': "u", '\u0215': "u", '\u0216': "u", '\u0217': "u", '\u0218': "s", '\u0219': "s",
	'\u021a': "t", '\u021b': "t", '\u021e': "h", '\u021f': "h", '\u0226': "a", '\u0227': "a",
	'\u0228': "e", '\u0229': "e", '\u022a': "o", '\u022b': "o", '\u022c': "o", '\u022d': "o",
	'\u022e': "o", '\u022f': "o", '\u0230': "o", '\u0231': "o", '\u0232': "y", '\u0233': "y",
}

// foldForSearch reduces text to what a search matches against: lower case,
// with Latin accents removed.
func foldForSearch(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	for _, r := range strings.ToLower(in) {
		// Combining marks left over from decomposed input, which is the same
		// accent written as two runes rather than one.
		if r >= 0x0300 && r <= 0x036F {
			continue
		}
		if folded, ok := foldings[r]; ok {
			b.WriteString(folded)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Fold exposes the same reduction to the rest of the server.
//
// Automatic moderation compares a message against a list of words a moderator
// typed, and has to do it the way a search does: one entry has to catch the
// word however it was capitalised or accented, or a word list is defeated by
// pressing shift. Sharing the function rather than writing a second one is
// what keeps the two from disagreeing about what "the same word" means.
func Fold(in string) string { return foldForSearch(in) }
