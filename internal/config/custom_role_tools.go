package config

import "unicode/utf8"

// MatchRoleToolPattern reports whether name matches pattern. The entire name
// must match; '*' matches any sequence of Unicode code points and '?' matches
// one Unicode code point. Every other code point is literal.
func MatchRoleToolPattern(pattern, name string) bool {
	patternIndex, nameIndex := 0, 0
	starPatternIndex, starNameIndex := -1, -1

	for nameIndex < len(name) {
		if patternIndex < len(pattern) {
			switch pattern[patternIndex] {
			case '*':
				starPatternIndex = patternIndex
				patternIndex++
				starNameIndex = nameIndex
				continue
			case '?':
				_, size := utf8.DecodeRuneInString(name[nameIndex:])
				patternIndex++
				nameIndex += size
				continue
			}

			patternRune, patternSize := utf8.DecodeRuneInString(pattern[patternIndex:])
			nameRune, nameSize := utf8.DecodeRuneInString(name[nameIndex:])
			if patternRune == nameRune {
				patternIndex += patternSize
				nameIndex += nameSize
				continue
			}
		}

		if starPatternIndex < 0 {
			return false
		}
		_, size := utf8.DecodeRuneInString(name[starNameIndex:])
		starNameIndex += size
		nameIndex = starNameIndex
		patternIndex = starPatternIndex + 1
	}

	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

// RoleToolsAllow reports whether any pattern allows name. An empty pattern
// list allows no tools.
func RoleToolsAllow(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if MatchRoleToolPattern(pattern, name) {
			return true
		}
	}
	return false
}
