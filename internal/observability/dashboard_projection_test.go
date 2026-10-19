package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type projectedDashboardHistory struct{ sample HistorySample }

func (h projectedDashboardHistory) Read(context.Context, time.Time, int) ([]HistorySample, error) {
	panic("chart requests must not load full repository history")
}

func (h projectedDashboardHistory) ReadCharts(context.Context, time.Time, int) ([]HistorySample, error) {
	return []HistorySample{h.sample}, nil
}

func TestDashboardUsesProjectedHistoryForChartRefreshes(t *testing.T) {
	sample := HistorySample{At: time.Now().UnixMilli(), Values: map[string]float64{"gortex_runtime_goroutines": 42}}
	handler := DashboardHandler(projectedDashboardHistory{sample: sample}, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/history?scope=charts", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var result dashboardHistoryResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	assert.True(t, result.Available)
	assert.Equal(t, []HistorySample{sample}, result.Samples)
}
