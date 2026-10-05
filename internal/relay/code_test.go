package relay

import (
	"strings"
	"testing"
)

func TestNewRoomCodeShape(t *testing.T) {
	for i := 0; i < 200; i++ {
		code, err := newRoomCode()
		if err != nil {
			t.Fatalf("newRoomCode: %v", err)
		}
		if len(code) != codeLen+codeGroupCount-1 {
			t.Fatalf("code %q length = %d, want %d", code, len(code), codeLen+codeGroupCount-1)
		}
		if code[codeGroupLen] != '-' {
			t.Fatalf("code %q missing group separator", code)
		}
		for _, r := range strings.ReplaceAll(code, "-", "") {
			if !strings.ContainsRune(codeAlphabet, r) {
				t.Fatalf("code %q contains %q, which is outside the alphabet", code, r)
			}
		}
	}
}

// The alphabet's whole job is to be unambiguous when read aloud or copied
// by hand, so guard the exclusions rather than trusting the literal.
func TestCodeAlphabetExcludesAmbiguousGlyphs(t *testing.T) {
	for _, excluded := range "AEIOUY01ILO" {
		if strings.ContainsRune(codeAlphabet, excluded) {
			t.Errorf("alphabet must not contain %q", excluded)
		}
	}
	seen := make(map[rune]bool, len(codeAlphabet))
	for _, r := range codeAlphabet {
		if seen[r] {
			t.Errorf("alphabet contains duplicate %q", r)
		}
		seen[r] = true
	}
}

func TestNormalizeCode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already canonical", "7K2-9QX", "7K2-9QX"},
		{"lowercase", "7k2-9qx", "7K2-9QX"},
		{"spaces instead of dash", "7k2 9qx", "7K2-9QX"},
		{"no separator", "7K29QX", "7K2-9QX"},
		{"surrounding whitespace", "  7K2-9QX  ", "7K2-9QX"},
		{"too short", "7K2", ""},
		{"too long", "7K2-9QX-B", ""},
		{"contains excluded glyph", "7K2-9QO", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeCode(tc.in); got != tc.want {
				t.Errorf("normalizeCode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewRoomCodeIsNotRepetitive(t *testing.T) {
	const samples = 500
	seen := make(map[string]bool, samples)
	for i := 0; i < samples; i++ {
		code, err := newRoomCode()
		if err != nil {
			t.Fatalf("newRoomCode: %v", err)
		}
		if seen[code] {
			t.Fatalf("duplicate code %q within %d samples — generator is not random", code, samples)
		}
		seen[code] = true
	}
}

func TestNewIDLength(t *testing.T) {
	id, err := newID()
	if err != nil {
		t.Fatalf("newID: %v", err)
	}
	if len(id) != 16 {
		t.Errorf("newID length = %d, want 16", len(id))
	}
}
