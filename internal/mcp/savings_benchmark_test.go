package mcp

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/tokens"
)

// Compare the prior repeat-read accounting path (sampling the same file again)
// with the once-per-session baseline. The fixture and tokenizer are warmed;
// persistence is disabled to isolate accounting rather than filesystem sync.
func BenchmarkRepeatedSummaryAccounting(b *testing.B) {
	for _, mode := range []string{"previous_recalibration", "once_per_session"} {
		b.Run(mode, func(b *testing.B) {
			srv, _, _ := newRetrievalSavingsServer(b, 1)
			ctx := WithSessionID(context.Background(), "benchmark")
			stats := srv.tokenStatsFor(ctx)
			stats.persistent = nil
			srv.recordFileBaselineSavings(ctx, "get_file_summary", "myrepo/mod0.go", "go", "summary")
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if mode == "once_per_session" {
					srv.recordFileBaselineSavings(ctx, "get_file_summary", "myrepo/mod0.go", "go", "summary")
					continue
				}
				// The pre-fix implementation calibrated and credited on every call.
				abs, err := srv.resolveGraphPath(ctx, "myrepo/mod0.go")
				require.NoError(b, err)
				info, err := os.Stat(abs)
				require.NoError(b, err)
				returned := tokens.CachedCountInt64("summary")
				fullFile := int64(tokens.EstimateFromSample(int(info.Size()), fileHeadSample(abs)))
				stats.creditFile(abs)
				stats.recordContext(ctx, srv.fileAttributionNode("myrepo/mod0.go", "go"), "get_file_summary", returned, fullFile)
			}
		})
	}
}
