package main

import (
	"slices"
	"strings"
)

// semanticLinefeedThreshold is the sentence-length trigger past which splitLongSentence looks for a clause boundary to break the line early, implementing the semantic-linefeeds convention (.agents/rules/400-documentation.md) for generated godoc prose.
//
// 400-documentation.md states "roughly 120 characters" as a guideline for hand-written prose,
// but this generator instead targets 90.
// That value (any threshold in [84, 91] scores identically) was chosen empirically to reproduce commit 6ce060c's hand-reflowed gem/s1.go, s2.go, s5.go, s6.go, s9.go.
// It reproduces 393 of the 398 wrapped Description/Body/Exception paragraphs in that commit exactly, the maximum a single length threshold can reach.
// The remaining 5 are inconsistencies in the hand edit itself, not evidence of a different rule;
// in one case two independent sentences were left on one line,
// and in the others a sentence past 120 chars was left unsplit despite an eligible boundary.
// The regeneration commit documents each one.
const semanticLinefeedThreshold = 90

// clauseBoundaries are the token forms splitLongSentence treats as a legal place to break an over-long sentence, mirroring 400-documentation.md's definition of a clause boundary:
// a semicolon, a colon, a coordinating conjunction, or the start of a relative clause.
// An em dash is part of that definition too but is omitted here.
// No message or exception text under tools/gemgen/data/messages carries one today,
// so the "break before the token" branch it would need has nothing to exercise it.
// Add it back, with a test, the first time a description actually needs it.
var clauseBoundaries = []string{";", ":", " and ", " but ", " so ", " which ", " that ", " where "}

// wrapParagraph splits s into godoc comment lines using the semantic-linefeeds convention:
// one sentence per line, except a sentence longer than semanticLinefeedThreshold is broken once at the first eligible clause boundary (see splitLongSentence).
// Callers pass one already-composed paragraph, e.g. "Exception: " + m.Exception,
// so wrapParagraph only wraps -- it never adds the label or a trailing period.
func wrapParagraph(s string) []string {
	sentences := splitSentences(s)
	if len(sentences) == 0 {
		return []string{s}
	}

	lines := make([]string, 0, len(sentences))
	for _, sentence := range sentences {
		lines = append(lines, splitLongSentence(sentence)...)
	}

	return lines
}

// splitSentences breaks s at sentence-ending periods: a '.' followed by whitespace or end-of-string, and not adjacent to another '.'.
// That adjacency check excludes both a mid-sentence ellipsis (the "..." in "L[n]{ <svids>... }.") and an abbreviation like "i.e.," or "e.g.," -- each of those periods is immediately followed by another character or by another period, never by whitespace on its own.
// Go's RE2-based regexp package cannot express "not preceded by / not followed by" with a lookaround,
// so this is a manual byte scan instead of a regexp.
func splitSentences(s string) []string {
	var out []string

	start := 0
	for i := range len(s) {
		if s[i] != '.' {
			continue
		}

		if (i > 0 && s[i-1] == '.') || (i+1 < len(s) && s[i+1] == '.') {
			continue // ellipsis or abbreviation period, not a sentence end
		}

		atEnd := i+1 == len(s)
		if !atEnd && !isBlank(s[i+1]) {
			continue // e.g. "i.e.," -- the period is followed by ',', not whitespace
		}

		if sentence := strings.TrimSpace(s[start : i+1]); sentence != "" {
			out = append(out, sentence)
		}

		j := i + 1
		for j < len(s) && isBlank(s[j]) {
			j++
		}
		start = j
	}

	if rest := strings.TrimSpace(s[start:]); rest != "" {
		out = append(out, rest)
	}

	return out
}

// isBlank reports whether b is a space or tab, the whitespace forms that can separate two sentences in generated godoc prose.
func isBlank(b byte) bool {
	return b == ' ' || b == '\t'
}

// splitLongSentence breaks one sentence into at most two lines when it is longer than semanticLinefeedThreshold characters and a clauseBoundaries token occurs at or after that offset;
// otherwise it returns the sentence unchanged.
// Only the first eligible token is used:
// the leftmost occurrence at or past the threshold, scanning all token forms together.
// The remainder is never itself re-split, matching every multi-line case in the hand-reflowed source this reproduces -- e.g. S2F49's opaque-body paragraph, whose second line stays one 122-character line.
func splitLongSentence(sentence string) []string {
	if len(sentence) <= semanticLinefeedThreshold {
		return []string{sentence}
	}

	for _, b := range findBoundaries(sentence) {
		if b.pos < semanticLinefeedThreshold {
			continue
		}

		if b.tok == ";" || b.tok == ":" {
			return []string{
				strings.TrimSpace(sentence[:b.pos+1]),
				strings.TrimSpace(sentence[b.pos+1:]),
			}
		}

		return []string{
			strings.TrimSpace(sentence[:b.pos]),
			strings.TrimSpace(sentence[b.pos:]),
		}
	}

	return []string{sentence}
}

// boundary is one clauseBoundaries token occurrence within a sentence, at byte offset pos.
type boundary struct {
	pos int
	tok string
}

// findBoundaries returns every clauseBoundaries occurrence in s, sorted by position,
// so splitLongSentence can pick the leftmost one at or after the threshold regardless of which token form it is.
func findBoundaries(s string) []boundary {
	var found []boundary

	for _, tok := range clauseBoundaries {
		start := 0
		for {
			idx := strings.Index(s[start:], tok)
			if idx == -1 {
				break
			}

			pos := start + idx
			found = append(found, boundary{pos: pos, tok: tok})
			start = pos + 1
		}
	}

	slices.SortFunc(found, func(a, b boundary) int { return a.pos - b.pos })

	return found
}
