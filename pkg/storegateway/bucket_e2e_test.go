// Included-from-location: https://github.com/thanos-io/thanos/blob/main/pkg/store/bucket_e2e_test.go
// Included-from-license: Apache-2.0
// Included-from-copyright: The Thanos Authors.

package storegateway

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/gogo/status"
	"github.com/oklog/ulid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/relabel"
	"github.com/prometheus/prometheus/pkg/timestamp"
	"github.com/stretchr/testify/assert"
	"github.com/thanos-io/thanos/pkg/objstore/filesystem"
	"github.com/weaveworks/common/httpgrpc"
	"google.golang.org/grpc/codes"

	mimir_tsdb "github.com/grafana/mimir/pkg/storage/tsdb"

	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/model"
	"github.com/thanos-io/thanos/pkg/objstore"
	storecache "github.com/thanos-io/thanos/pkg/store/cache"
	"github.com/thanos-io/thanos/pkg/store/labelpb"
	"github.com/thanos-io/thanos/pkg/store/storepb"
)

var (
	minTime            = time.Unix(0, 0)
	maxTime, _         = time.Parse(time.RFC3339, "9999-12-31T23:59:59Z")
	minTimeDuration    = model.TimeOrDurationValue{Time: &minTime}
	maxTimeDuration    = model.TimeOrDurationValue{Time: &maxTime}
	allowAllFilterConf = &FilterConfig{
		MinTime: minTimeDuration,
		MaxTime: maxTimeDuration,
	}
)

type swappableCache struct {
	ptr storecache.IndexCache
}

type customLimiter struct {
	limiter *Limiter
	code    codes.Code
}

func (c *customLimiter) Reserve(num uint64) error {
	err := c.limiter.Reserve(num)
	if err != nil {
		return httpgrpc.Errorf(int(c.code), err.Error())
	}

	return nil
}

func (c *swappableCache) SwapWith(ptr2 storecache.IndexCache) {
	c.ptr = ptr2
}

func (c *swappableCache) StorePostings(ctx context.Context, blockID ulid.ULID, l labels.Label, v []byte) {
	c.ptr.StorePostings(ctx, blockID, l, v)
}

func (c *swappableCache) FetchMultiPostings(ctx context.Context, blockID ulid.ULID, keys []labels.Label) (map[labels.Label][]byte, []labels.Label) {
	return c.ptr.FetchMultiPostings(ctx, blockID, keys)
}

func (c *swappableCache) StoreSeries(ctx context.Context, blockID ulid.ULID, id uint64, v []byte) {
	c.ptr.StoreSeries(ctx, blockID, id, v)
}

func (c *swappableCache) FetchMultiSeries(ctx context.Context, blockID ulid.ULID, ids []uint64) (map[uint64][]byte, []uint64) {
	return c.ptr.FetchMultiSeries(ctx, blockID, ids)
}

type storeSuite struct {
	store            *BucketStore
	minTime, maxTime int64
	cache            *swappableCache

	logger log.Logger
}

func prepareTestBlocks(t testing.TB, now time.Time, count int, dir string, bkt objstore.Bucket,
	series []labels.Labels, extLset labels.Labels) (minTime, maxTime int64) {
	ctx := context.Background()
	logger := log.NewNopLogger()

	for i := 0; i < count; i++ {
		mint := timestamp.FromTime(now)
		now = now.Add(2 * time.Hour)
		maxt := timestamp.FromTime(now)

		if minTime == 0 {
			minTime = mint
		}
		maxTime = maxt

		// Create two blocks per time slot. Only add 10 samples each so only one chunk
		// gets created each. This way we can easily verify we got 10 chunks per series below.
		id1, err := CreateBlock(ctx, dir, series[:4], 10, mint, maxt, extLset, 0, metadata.NoneFunc)
		assert.NoError(t, err)
		id2, err := CreateBlock(ctx, dir, series[4:], 10, mint, maxt, extLset, 0, metadata.NoneFunc)
		assert.NoError(t, err)

		dir1, dir2 := filepath.Join(dir, id1.String()), filepath.Join(dir, id2.String())

		// Replace labels to the meta of the second block.
		meta, err := metadata.ReadFromDir(dir2)
		assert.NoError(t, err)
		meta.Thanos.Labels = map[string]string{"ext2": "value2"}
		assert.NoError(t, meta.WriteToDir(logger, dir2))

		assert.NoError(t, block.Upload(ctx, logger, bkt, dir1, metadata.NoneFunc))
		assert.NoError(t, block.Upload(ctx, logger, bkt, dir2, metadata.NoneFunc))

		assert.NoError(t, os.RemoveAll(dir1))
		assert.NoError(t, os.RemoveAll(dir2))
	}

	return
}

func newCustomChunksLimiterFactory(limit uint64, code codes.Code) ChunksLimiterFactory {
	return func(failedCounter prometheus.Counter) ChunksLimiter {
		return &customLimiter{
			limiter: NewLimiter(limit, failedCounter),
			code:    code,
		}
	}
}

func newCustomSeriesLimiterFactory(limit uint64, code codes.Code) SeriesLimiterFactory {
	return func(failedCounter prometheus.Counter) SeriesLimiter {
		return &customLimiter{
			limiter: NewLimiter(limit, failedCounter),
			code:    code,
		}
	}
}

func prepareStoreWithTestBlocks(t testing.TB, dir string, bkt objstore.Bucket, manyParts bool, chunksLimiterFactory ChunksLimiterFactory, seriesLimiterFactory SeriesLimiterFactory, relabelConfig []*relabel.Config, filterConf *FilterConfig) *storeSuite {
	series := []labels.Labels{
		labels.FromStrings("a", "1", "b", "1"),
		labels.FromStrings("a", "1", "b", "2"),
		labels.FromStrings("a", "2", "b", "1"),
		labels.FromStrings("a", "2", "b", "2"),
		labels.FromStrings("a", "1", "c", "1"),
		labels.FromStrings("a", "1", "c", "2"),
		labels.FromStrings("a", "2", "c", "1"),
		labels.FromStrings("a", "2", "c", "2"),
	}
	extLset := labels.FromStrings("ext1", "value1")

	minTime, maxTime := prepareTestBlocks(t, time.Now(), 3, dir, bkt, series, extLset)

	s := &storeSuite{
		logger:  log.NewNopLogger(),
		cache:   &swappableCache{},
		minTime: minTime,
		maxTime: maxTime,
	}

	metaFetcher, err := block.NewMetaFetcher(s.logger, 20, objstore.WithNoopInstr(bkt), dir, nil, []block.MetadataFilter{
		block.NewTimePartitionMetaFilter(filterConf.MinTime, filterConf.MaxTime),
		block.NewLabelShardedMetaFilter(relabelConfig),
	}, nil)
	assert.NoError(t, err)

	store, err := NewBucketStore(
		objstore.WithNoopInstr(bkt),
		metaFetcher,
		dir,
		chunksLimiterFactory,
		seriesLimiterFactory,
		newGapBasedPartitioner(mimir_tsdb.DefaultPartitionerMaxGapSize, nil),
		20,
		true,
		mimir_tsdb.DefaultPostingOffsetInMemorySampling,
		true,
		true,
		time.Minute,
		WithLogger(s.logger),
		WithIndexCache(s.cache),
		WithFilterConfig(filterConf),
	)
	assert.NoError(t, err)
	defer func() { assert.NoError(t, store.Close()) }()

	s.store = store

	if manyParts {
		s.store.partitioner = naivePartitioner{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	assert.NoError(t, store.SyncBlocks(ctx))
	return s
}

// TODO(bwplotka): Benchmark Series.
//nolint:golint
func testBucketStore_e2e(t *testing.T, ctx context.Context, s *storeSuite) {
	t.Helper()

	mint, maxt := s.store.TimeRange()
	assert.Equal(t, s.minTime, mint)
	assert.Equal(t, s.maxTime, maxt)

	vals, err := s.store.LabelValues(ctx, &storepb.LabelValuesRequest{
		Label: "a",
		Start: timestamp.FromTime(minTime),
		End:   timestamp.FromTime(maxTime),
	})
	assert.NoError(t, err)
	assert.Equal(t, []string{"1", "2"}, vals.Values)

	// TODO(bwplotka): Add those test cases to TSDB querier_test.go as well, there are no tests for matching.
	for i, tcase := range []struct {
		req              *storepb.SeriesRequest
		expected         [][]labelpb.ZLabel
		expectedChunkLen int
	}{
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_RE, Name: "a", Value: "1|2"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
			expectedChunkLen: 3,
			expected: [][]labelpb.ZLabel{
				{{Name: "a", Value: "1"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "2"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "2"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "2"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "2"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
			},
		},
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_RE, Name: "a", Value: "1"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
			expectedChunkLen: 3,
			expected: [][]labelpb.ZLabel{
				{{Name: "a", Value: "1"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
			},
		},
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_NRE, Name: "a", Value: "2"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
			expectedChunkLen: 3,
			expected: [][]labelpb.ZLabel{
				{{Name: "a", Value: "1"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
			},
		},
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_NRE, Name: "a", Value: "not_existing"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
			expectedChunkLen: 3,
			expected: [][]labelpb.ZLabel{
				{{Name: "a", Value: "1"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "2"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "2"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "2"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "2"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
			},
		},
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_NRE, Name: "not_existing", Value: "1"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
			expectedChunkLen: 3,
			expected: [][]labelpb.ZLabel{
				{{Name: "a", Value: "1"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "2"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "2"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "2"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "2"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
			},
		},
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_EQ, Name: "b", Value: "2"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
			expectedChunkLen: 3,
			expected: [][]labelpb.ZLabel{
				{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "2"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
			},
		},
		{
			// Matching by external label should work as well.
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_EQ, Name: "a", Value: "1"},
					{Type: storepb.LabelMatcher_EQ, Name: "ext2", Value: "value2"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
			expectedChunkLen: 3,
			expected: [][]labelpb.ZLabel{
				{{Name: "a", Value: "1"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
			},
		},
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_EQ, Name: "a", Value: "1"},
					{Type: storepb.LabelMatcher_EQ, Name: "ext2", Value: "wrong-value"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
		},
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_NEQ, Name: "a", Value: "2"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
			expectedChunkLen: 3,
			expected: [][]labelpb.ZLabel{
				{{Name: "a", Value: "1"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
			},
		},
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_NEQ, Name: "a", Value: "not_existing"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
			expectedChunkLen: 3,
			expected: [][]labelpb.ZLabel{
				{{Name: "a", Value: "1"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "2"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "2"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "2"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "2"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
			},
		},
		// Regression https://github.com/thanos-io/thanos/issues/833.
		// Problem: Matcher that was selecting NO series, was ignored instead of passed as emptyPosting to Intersect.
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_EQ, Name: "a", Value: "1"},
					{Type: storepb.LabelMatcher_RE, Name: "non_existing", Value: "something"},
				},
				MinTime: mint,
				MaxTime: maxt,
			},
		},
		// Test skip-chunk option.
		{
			req: &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_EQ, Name: "a", Value: "1"},
				},
				MinTime:    mint,
				MaxTime:    maxt,
				SkipChunks: true,
			},
			expectedChunkLen: 0,
			expected: [][]labelpb.ZLabel{
				{{Name: "a", Value: "1"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
				{{Name: "a", Value: "1"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
			},
		},
	} {
		if ok := t.Run(fmt.Sprint(i), func(t *testing.T) {
			srv := newBucketStoreSeriesServer(ctx)

			assert.NoError(t, s.store.Series(tcase.req, srv))
			assert.Equal(t, len(tcase.expected), len(srv.SeriesSet))

			for i, s := range srv.SeriesSet {
				assert.Equal(t, tcase.expected[i], s.Labels)
				assert.Equal(t, tcase.expectedChunkLen, len(s.Chunks))
			}
		}); !ok {
			return
		}
	}
}

func TestBucketStore_e2e(t *testing.T) {
	foreachStore(t, func(t *testing.T, bkt objstore.Bucket) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		dir, err := ioutil.TempDir("", "test_bucketstore_e2e")
		assert.NoError(t, err)
		defer func() { assert.NoError(t, os.RemoveAll(dir)) }()

		s := prepareStoreWithTestBlocks(t, dir, bkt, false, NewChunksLimiterFactory(0), NewSeriesLimiterFactory(0), emptyRelabelConfig, allowAllFilterConf)

		if ok := t.Run("no index cache", func(t *testing.T) {
			s.cache.SwapWith(noopCache{})
			testBucketStore_e2e(t, ctx, s)
		}); !ok {
			return
		}

		if ok := t.Run("with large, sufficient index cache", func(t *testing.T) {
			indexCache, err := storecache.NewInMemoryIndexCacheWithConfig(s.logger, nil, storecache.InMemoryIndexCacheConfig{
				MaxItemSize: 1e5,
				MaxSize:     2e5,
			})
			assert.NoError(t, err)
			s.cache.SwapWith(indexCache)
			testBucketStore_e2e(t, ctx, s)
		}); !ok {
			return
		}

		t.Run("with small index cache", func(t *testing.T) {
			indexCache2, err := storecache.NewInMemoryIndexCacheWithConfig(s.logger, nil, storecache.InMemoryIndexCacheConfig{
				MaxItemSize: 50,
				MaxSize:     100,
			})
			assert.NoError(t, err)
			s.cache.SwapWith(indexCache2)
			testBucketStore_e2e(t, ctx, s)
		})
	})
}

type naivePartitioner struct{}

func (g naivePartitioner) Partition(length int, rng func(int) (uint64, uint64)) (parts []Part) {
	for i := 0; i < length; i++ {
		s, e := rng(i)
		parts = append(parts, Part{Start: s, End: e, ElemRng: [2]int{i, i + 1}})
	}
	return parts
}

// Naive partitioner splits the array equally (it does not combine anything).
// This tests if our, sometimes concurrent, fetches for different parts works.
// Regression test against: https://github.com/thanos-io/thanos/issues/829.
func TestBucketStore_ManyParts_e2e(t *testing.T) {
	foreachStore(t, func(t *testing.T, bkt objstore.Bucket) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		dir, err := ioutil.TempDir("", "test_bucketstore_e2e")
		assert.NoError(t, err)
		defer func() { assert.NoError(t, os.RemoveAll(dir)) }()

		s := prepareStoreWithTestBlocks(t, dir, bkt, true, NewChunksLimiterFactory(0), NewSeriesLimiterFactory(0), emptyRelabelConfig, allowAllFilterConf)

		indexCache, err := storecache.NewInMemoryIndexCacheWithConfig(s.logger, nil, storecache.InMemoryIndexCacheConfig{
			MaxItemSize: 1e5,
			MaxSize:     2e5,
		})
		assert.NoError(t, err)
		s.cache.SwapWith(indexCache)

		testBucketStore_e2e(t, ctx, s)
	})
}

func TestBucketStore_TimePartitioning_e2e(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bkt := objstore.NewInMemBucket()

	dir, err := ioutil.TempDir("", "test_bucket_time_part_e2e")
	assert.NoError(t, err)
	defer func() { assert.NoError(t, os.RemoveAll(dir)) }()

	hourAfter := time.Now().Add(1 * time.Hour)
	filterMaxTime := model.TimeOrDurationValue{Time: &hourAfter}

	// The query will fetch 2 series from 2 blocks, so we do expect to hit a total of 4 chunks.
	expectedChunks := uint64(2 * 2)

	s := prepareStoreWithTestBlocks(t, dir, bkt, false, NewChunksLimiterFactory(expectedChunks), NewSeriesLimiterFactory(0), emptyRelabelConfig, &FilterConfig{
		MinTime: minTimeDuration,
		MaxTime: filterMaxTime,
	})
	assert.NoError(t, s.store.SyncBlocks(ctx))

	mint, maxt := s.store.TimeRange()
	assert.Equal(t, s.minTime, mint)
	assert.Equal(t, filterMaxTime.PrometheusTimestamp(), maxt)

	req := &storepb.SeriesRequest{
		Matchers: []storepb.LabelMatcher{
			{Type: storepb.LabelMatcher_EQ, Name: "a", Value: "1"},
		},
		MinTime: mint,
		MaxTime: timestamp.FromTime(time.Now().AddDate(0, 0, 1)),
	}

	expectedLabels := [][]labelpb.ZLabel{
		{{Name: "a", Value: "1"}, {Name: "b", Value: "1"}, {Name: "ext1", Value: "value1"}},
		{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "ext1", Value: "value1"}},
		{{Name: "a", Value: "1"}, {Name: "c", Value: "1"}, {Name: "ext2", Value: "value2"}},
		{{Name: "a", Value: "1"}, {Name: "c", Value: "2"}, {Name: "ext2", Value: "value2"}},
	}

	s.cache.SwapWith(noopCache{})
	srv := newBucketStoreSeriesServer(ctx)

	assert.NoError(t, s.store.Series(req, srv))
	assert.Equal(t, len(expectedLabels), len(srv.SeriesSet))

	for i, s := range srv.SeriesSet {
		assert.Equal(t, expectedLabels[i], s.Labels)

		// prepareTestBlocks makes 3 chunks containing 2 hour data,
		// we should only get 1, as we are filtering by time.
		assert.Equal(t, 1, len(s.Chunks))
	}
}

func TestBucketStore_Series_ChunksLimiter_e2e(t *testing.T) {
	// The query will fetch 2 series from 6 blocks, so we do expect to hit a total of 12 chunks.
	expectedChunks := uint64(2 * 6)

	cases := map[string]struct {
		maxChunksLimit uint64
		maxSeriesLimit uint64
		expectedErr    string
		code           codes.Code
	}{
		"should succeed if the max chunks limit is not exceeded": {
			maxChunksLimit: expectedChunks,
		},
		"should fail if the max chunks limit is exceeded - ResourceExhausted": {
			maxChunksLimit: expectedChunks - 1,
			expectedErr:    "exceeded chunks limit",
			code:           codes.ResourceExhausted,
		},
		"should fail if the max chunks limit is exceeded - 422": {
			maxChunksLimit: expectedChunks - 1,
			expectedErr:    "exceeded chunks limit",
			code:           422,
		},
		"should fail if the max series limit is exceeded - 422": {
			maxChunksLimit: expectedChunks,
			expectedErr:    "exceeded series limit",
			maxSeriesLimit: 1,
			code:           422,
		},
	}

	for testName, testData := range cases {
		t.Run(testName, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bkt := objstore.NewInMemBucket()

			dir, err := ioutil.TempDir("", "test_bucket_chunks_limiter_e2e")
			assert.NoError(t, err)
			defer func() { assert.NoError(t, os.RemoveAll(dir)) }()

			s := prepareStoreWithTestBlocks(t, dir, bkt, false, newCustomChunksLimiterFactory(testData.maxChunksLimit, testData.code), newCustomSeriesLimiterFactory(testData.maxSeriesLimit, testData.code), emptyRelabelConfig, allowAllFilterConf)
			assert.NoError(t, s.store.SyncBlocks(ctx))

			req := &storepb.SeriesRequest{
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_EQ, Name: "a", Value: "1"},
				},
				MinTime: minTimeDuration.PrometheusTimestamp(),
				MaxTime: maxTimeDuration.PrometheusTimestamp(),
			}

			s.cache.SwapWith(noopCache{})
			srv := newBucketStoreSeriesServer(ctx)
			err = s.store.Series(req, srv)

			if testData.expectedErr == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), testData.expectedErr))
				status, ok := status.FromError(err)
				assert.Equal(t, true, ok)
				assert.Equal(t, testData.code, status.Code())
			}
		})
	}
}

func TestBucketStore_LabelNames_e2e(t *testing.T) {
	foreachStore(t, func(t *testing.T, bkt objstore.Bucket) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		dir, err := ioutil.TempDir("", "test_bucketstore_label_names_e2e")
		assert.NoError(t, err)
		defer func() { assert.NoError(t, os.RemoveAll(dir)) }()

		s := prepareStoreWithTestBlocks(t, dir, bkt, false, NewChunksLimiterFactory(0), NewSeriesLimiterFactory(0), emptyRelabelConfig, allowAllFilterConf)
		s.cache.SwapWith(noopCache{})

		mint, maxt := s.store.TimeRange()
		assert.Equal(t, s.minTime, mint)
		assert.Equal(t, s.maxTime, maxt)

		for name, tc := range map[string]struct {
			req      *storepb.LabelNamesRequest
			expected []string
		}{
			"basic labelNames": {
				req: &storepb.LabelNamesRequest{
					Start: timestamp.FromTime(minTime),
					End:   timestamp.FromTime(maxTime),
				},
				expected: []string{"a", "b", "c", "ext1", "ext2"}, // ext2 is added by the prepareStoreWithTestBlocks function.
			},
			"outside the time range": {
				req: &storepb.LabelNamesRequest{
					Start: timestamp.FromTime(time.Now().Add(-24 * time.Hour)),
					End:   timestamp.FromTime(time.Now().Add(-23 * time.Hour)),
				},
				expected: nil,
			},
			"matcher matching everything": {
				req: &storepb.LabelNamesRequest{
					Start: timestamp.FromTime(minTime),
					End:   timestamp.FromTime(maxTime),
					Matchers: []storepb.LabelMatcher{
						{
							Type:  storepb.LabelMatcher_EQ,
							Name:  "a",
							Value: "1",
						},
					},
				},
				expected: []string{"a", "b", "c", "ext1", "ext2"},
			},
			"b=1 matcher": {
				req: &storepb.LabelNamesRequest{
					Start: timestamp.FromTime(minTime),
					End:   timestamp.FromTime(maxTime),
					Matchers: []storepb.LabelMatcher{
						{
							Type:  storepb.LabelMatcher_EQ,
							Name:  "b",
							Value: "1",
						},
					},
				},
				expected: []string{"a", "b", "ext1"},
			},

			"b='' matcher": {
				req: &storepb.LabelNamesRequest{
					Start: timestamp.FromTime(minTime),
					End:   timestamp.FromTime(maxTime),
					Matchers: []storepb.LabelMatcher{
						{
							Type:  storepb.LabelMatcher_EQ,
							Name:  "b",
							Value: "",
						},
					},
				},
				expected: []string{"a", "c", "ext2"},
			},
			"outside the time range, with matcher": {
				req: &storepb.LabelNamesRequest{
					Start: timestamp.FromTime(time.Now().Add(-24 * time.Hour)),
					End:   timestamp.FromTime(time.Now().Add(-23 * time.Hour)),
					Matchers: []storepb.LabelMatcher{
						{
							Type:  storepb.LabelMatcher_EQ,
							Name:  "a",
							Value: "1",
						},
					},
				},
				expected: nil,
			},
		} {
			t.Run(name, func(t *testing.T) {
				vals, err := s.store.LabelNames(ctx, tc.req)
				assert.NoError(t, err)

				assert.Equal(t, tc.expected, vals.Names)
			})
		}
	})
}

func TestBucketStore_LabelValues_e2e(t *testing.T) {
	foreachStore(t, func(t *testing.T, bkt objstore.Bucket) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		dir, err := ioutil.TempDir("", "test_bucketstore_label_values_e2e")
		assert.NoError(t, err)
		defer func() { assert.NoError(t, os.RemoveAll(dir)) }()

		s := prepareStoreWithTestBlocks(t, dir, bkt, false, NewChunksLimiterFactory(0), NewSeriesLimiterFactory(0), emptyRelabelConfig, allowAllFilterConf)
		s.cache.SwapWith(noopCache{})

		mint, maxt := s.store.TimeRange()
		assert.Equal(t, s.minTime, mint)
		assert.Equal(t, s.maxTime, maxt)

		for name, tc := range map[string]struct {
			req      *storepb.LabelValuesRequest
			expected []string
		}{
			"label a": {
				req: &storepb.LabelValuesRequest{
					Label: "a",
					Start: timestamp.FromTime(minTime),
					End:   timestamp.FromTime(maxTime),
				},
				expected: []string{"1", "2"},
			},
			"label a, outside time range": {
				req: &storepb.LabelValuesRequest{
					Label: "a",
					Start: timestamp.FromTime(time.Now().Add(-24 * time.Hour)),
					End:   timestamp.FromTime(time.Now().Add(-23 * time.Hour)),
				},
				expected: nil,
			},
			"label a, a=1": {
				req: &storepb.LabelValuesRequest{
					Label: "a",
					Start: timestamp.FromTime(minTime),
					End:   timestamp.FromTime(maxTime),
					Matchers: []storepb.LabelMatcher{
						{
							Type:  storepb.LabelMatcher_EQ,
							Name:  "a",
							Value: "1",
						},
					},
				},
				expected: []string{"1"},
			},
			"label a, a=2, c=2": {
				req: &storepb.LabelValuesRequest{
					Label: "a",
					Start: timestamp.FromTime(minTime),
					End:   timestamp.FromTime(maxTime),
					Matchers: []storepb.LabelMatcher{
						{
							Type:  storepb.LabelMatcher_EQ,
							Name:  "a",
							Value: "2",
						},
						{
							Type:  storepb.LabelMatcher_EQ,
							Name:  "c",
							Value: "2",
						},
					},
				},
				expected: []string{"2"},
			},
			"label ext1": {
				req: &storepb.LabelValuesRequest{
					Label: "ext1",
					Start: timestamp.FromTime(minTime),
					End:   timestamp.FromTime(maxTime),
				},
				expected: []string{"value1"},
			},
			"label ext1, c=1": {
				req: &storepb.LabelValuesRequest{
					Label: "ext1",
					Start: timestamp.FromTime(minTime),
					End:   timestamp.FromTime(maxTime),
					Matchers: []storepb.LabelMatcher{
						{
							Type:  storepb.LabelMatcher_EQ,
							Name:  "c",
							Value: "1",
						},
					},
				},
				expected: nil, // ext1 is replaced with ext2 for series with c
			},
		} {
			t.Run(name, func(t *testing.T) {
				vals, err := s.store.LabelValues(ctx, tc.req)
				assert.NoError(t, err)

				assert.Equal(t, tc.expected, emptyToNil(vals.Values))
			})
		}
	})
}

func emptyToNil(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return values
}

func foreachStore(t *testing.T, testFn func(t *testing.T, bkt objstore.Bucket)) {
	t.Parallel()

	// Mandatory Inmem. Not parallel, to detect problem early.
	if ok := t.Run("inmem", func(t *testing.T) {
		testFn(t, objstore.NewInMemBucket())
	}); !ok {
		return
	}

	// Mandatory Filesystem.
	t.Run("filesystem", func(t *testing.T) {
		t.Parallel()

		dir, err := ioutil.TempDir("", "filesystem-foreach-store-test")
		assert.NoError(t, err)
		defer assert.NoError(t, os.RemoveAll(dir))

		b, err := filesystem.NewBucket(dir)
		assert.NoError(t, err)
		testFn(t, b)
	})
}