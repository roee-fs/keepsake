// package store, not store_test: pgRegex is unexported.
package store

import "testing"

func TestPgRegexTranslatesWordBoundariesOnly(t *testing.T) {
	for in, want := range map[string]string{
		`\bfoo\b`:       `\yfoo\y`,
		`\Bfoo`:         `\Yfoo`,
		`\d+\b`:         `\d+\y`,
		`\\b`:           `\\b`,
		`[\b]x`:         `[\b]x`,
		`[]a]\b`:        `[]a]\y`,
		`[^]a]\b`:       `[^]a]\y`,
		`[[:alpha:]]\b`: `[[:alpha:]]\y`,
		`[a[.-.]z]\b`:   `[a[.-.]z]\y`,
		`***=\b`:        `***=\b`,
		`(?q)\b`:        `(?q)\b`,
		`***:(?ie)\b`:   `***:(?ie)\b`,
		`(?i)\b`:        `(?i)\y`,
		`(?:a)\b`:       `(?:a)\y`,
		`日本\b`:          `日本\y`,
		`trailing\`:     `trailing\`,
		`no boundaries`: `no boundaries`,
	} {
		if got := pgRegex(in); got != want {
			t.Errorf("pgRegex(%q) = %q, want %q", in, got, want)
		}
	}
}
