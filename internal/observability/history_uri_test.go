package observability

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHistoryDSNAbsoluteFileURI(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"Windows drive", "C:/Users/test user/metric #雪%?.db", "/C:/Users/test user/metric #雪%?.db"},
		{"POSIX", "/tmp/test user/metric #雪%?.db", "/tmp/test user/metric #雪%?.db"},
		{"UNC", "//server/share/metric #雪%.db", "//server/share/metric #雪%.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri, err := url.Parse(historyDSN(tc.path))
			require.NoError(t, err)
			assert.Equal(t, "file", uri.Scheme)
			assert.Empty(t, uri.Host, "a drive letter must not become SQLite URI authority")
			assert.Empty(t, uri.Opaque, "a file URI needs an absolute path")
			assert.Equal(t, tc.want, uri.Path)
			assert.True(t, strings.HasPrefix(historyDSN(tc.path), "file:///"))
			assert.Empty(t, uri.Fragment)
			assert.ElementsMatch(t, []string{"busy_timeout(2000)", "secure_delete(ON)", "temp_store(MEMORY)"}, uri.Query()["_pragma"])
		})
	}
}

func TestHistoryURIUsesLiteralEscapedFilename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metric history #雪%.db")
	history, err := OpenHistory(path)
	require.NoError(t, err)
	sample := HistorySample{At: time.Now().UnixMilli(), Values: map[string]float64{"gortex_test_total": 3}}
	require.NoError(t, history.Append(t.Context(), sample))
	require.NoError(t, history.Close())
	assert.FileExists(t, path)
	reopened, err := OpenHistory(path)
	require.NoError(t, err)
	defer func() { assert.NoError(t, reopened.Close()) }()
	samples, err := reopened.Read(t.Context(), time.Now().Add(-time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, samples, 1)
	assert.Equal(t, sample, samples[0])
}
