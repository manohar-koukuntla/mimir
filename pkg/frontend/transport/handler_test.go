// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/frontend/transport/handler_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/concurrency"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/user"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestWriteError(t *testing.T) {
	for _, test := range []struct {
		status int
		err    error
	}{
		{http.StatusInternalServerError, errors.New("unknown")},
		{http.StatusGatewayTimeout, context.DeadlineExceeded},
		{StatusClientClosedRequest, context.Canceled},
		{http.StatusBadRequest, httpgrpc.Errorf(http.StatusBadRequest, "")},
	} {
		t.Run(test.err.Error(), func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, test.err)
			require.Equal(t, test.status, w.Result().StatusCode)
		})
	}
}

func TestHandler_ServeHTTP(t *testing.T) {
	for _, tt := range []struct {
		name            string
		cfg             HandlerConfig
		expectedMetrics int
	}{
		{
			name:            "test handler with stats enabled",
			cfg:             HandlerConfig{QueryStatsEnabled: true},
			expectedMetrics: 4,
		},
		{
			name:            "test handler with stats disabled",
			cfg:             HandlerConfig{QueryStatsEnabled: false},
			expectedMetrics: 0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			roundTripper := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("{}")),
				}, nil
			})

			reg := prometheus.NewPedanticRegistry()
			handler := NewHandler(tt.cfg, roundTripper, log.NewNopLogger(), reg)

			ctx := user.InjectOrgID(context.Background(), "12345")
			req := httptest.NewRequest("GET", "/", nil)
			req = req.WithContext(ctx)
			resp := httptest.NewRecorder()

			handler.ServeHTTP(resp, req)
			_, _ = io.ReadAll(resp.Body)
			require.Equal(t, resp.Code, http.StatusOK)

			count, err := promtest.GatherAndCount(
				reg,
				"cortex_query_seconds_total",
				"cortex_query_fetched_series_total",
				"cortex_query_fetched_chunk_bytes_total",
				"cortex_query_fetched_chunks_total",
			)

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedMetrics, count)
		})
	}
}

func TestHandler_FailedRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name                string
		cfg                 HandlerConfig
		expectedMetrics     int
		path                string
		expectQueryParamLog bool
		queryErr            error
	}{
		{
			name:                "Failed round trip with context cancelled",
			cfg:                 HandlerConfig{QueryStatsEnabled: false},
			expectedMetrics:     0,
			path:                "/api/v1/query?query=up&time=2015-07-01T20:10:51.781Z",
			expectQueryParamLog: true,
			queryErr:            context.Canceled,
		},
		{
			name:                "Failed round trip with no query params",
			cfg:                 HandlerConfig{QueryStatsEnabled: true},
			expectedMetrics:     4,
			path:                "/api/v1/query",
			expectQueryParamLog: false,
			queryErr:            context.Canceled,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			roundTripper := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return nil, test.queryErr
			})

			reg := prometheus.NewPedanticRegistry()
			logs := &concurrency.SyncBuffer{}
			logger := log.NewLogfmtLogger(logs)
			handler := NewHandler(test.cfg, roundTripper, logger, reg)

			ctx := user.InjectOrgID(context.Background(), "12345")
			req := httptest.NewRequest("GET", test.path, nil)
			req = req.WithContext(ctx)
			resp := httptest.NewRecorder()

			handler.ServeHTTP(resp, req)
			require.Equal(t, StatusClientClosedRequest, resp.Code)

			count, err := promtest.GatherAndCount(
				reg,
				"cortex_query_seconds_total",
				"cortex_query_fetched_series_total",
				"cortex_query_fetched_chunk_bytes_total",
				"cortex_query_fetched_chunks_total",
			)

			require.NoError(t, err)

			assert.Contains(t, strings.TrimSpace(logs.String()), "sharded_queries")
			assert.Contains(t, strings.TrimSpace(logs.String()), "status")
			if test.expectQueryParamLog {
				assert.Contains(t, strings.TrimSpace(logs.String()), "param_query")
			}
			assert.Equal(t, test.expectedMetrics, count)
		})
	}
}
