package store_sqlite

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLikePatternEscapesLiteralMetacharacters(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	for _, test := range []struct {
		name, needle, exact, decoy string
	}{
		{"percent", "100%", "item100%value", "item100value"},
		{"underscore", "foo_bar", "xfoo_bary", "xfooXbary"},
		{"backslash", `a\b`, `xa\by`, "xaby"},
		{"mixed_unicode", `кофе_%\🍫`, `xкофе_%\🍫y`, "xкофеXanything🍫y"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pattern := "%" + escapeLikePattern(test.needle) + "%"
			for candidate, expected := range map[string]bool{test.exact: true, test.decoy: false} {
				var matches bool
				require.NoError(t, db.QueryRowContext(context.Background(), `SELECT ? LIKE ? ESCAPE '\'`, candidate, pattern).Scan(&matches))
				assert.Equal(t, expected, matches, candidate)
			}
		})
	}
}

func TestLikePatternConcurrentEscaping(t *testing.T) {
	input := `path\100%_value`
	want := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(input, `\`, `\\`), `%`, `\%`), `_`, `\_`)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 100 {
				assert.Equal(t, want, escapeLikePattern(input))
			}
		})
	}
	workers.Wait()
}

func BenchmarkEscapeLikePattern(b *testing.B) {
	for _, input := range []string{"PerfWorkloadTarget001", `path\100%_value`} {
		b.Run(input, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = escapeLikePattern(input)
			}
		})
	}
}
