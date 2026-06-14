package main

import (
	"math"
	"testing"
)

const promFixture = `# HELP openfga_request_duration_ms duration
# TYPE openfga_request_duration_ms histogram
openfga_request_duration_ms_bucket{grpc_method="check",le="1"} 50
openfga_request_duration_ms_bucket{grpc_method="check",le="5"} 90
openfga_request_duration_ms_bucket{grpc_method="check",le="+Inf"} 100
openfga_request_duration_ms_sum{grpc_method="check"} 250
openfga_request_duration_ms_count{grpc_method="check"} 100
openfga_request_duration_ms_bucket{grpc_method="batch_check",le="1"} 10
openfga_request_duration_ms_bucket{grpc_method="batch_check",le="5"} 10
openfga_request_duration_ms_bucket{grpc_method="batch_check",le="+Inf"} 10
openfga_request_duration_ms_sum{grpc_method="batch_check"} 5
openfga_request_duration_ms_count{grpc_method="batch_check"} 10
# TYPE openfga_datastore_query_count histogram
openfga_datastore_query_count_bucket{grpc_method="check",le="1"} 20
openfga_datastore_query_count_bucket{grpc_method="check",le="+Inf"} 100
openfga_datastore_query_count_sum{grpc_method="check"} 300
openfga_datastore_query_count_count{grpc_method="check"} 100
# TYPE openfga_check_cache_hit_count counter
openfga_check_cache_hit_count 40
# TYPE openfga_check_cache_total_count counter
openfga_check_cache_total_count 80
# TYPE unrelated_metric counter
unrelated_metric{foo="bar"} 7
`

func TestParsePrometheusAggregatesLabelSets(t *testing.T) {
	s := parsePrometheus(promFixture)
	h := s.Histograms["openfga_request_duration_ms"]
	if h == nil {
		t.Fatal("request_duration histogram not parsed")
	}
	// check + batch_check label sets summed
	if h.Count != 110 || h.Sum != 255 {
		t.Errorf("count/sum = %v/%v, want 110/255", h.Count, h.Sum)
	}
	if h.Buckets[1] != 60 || h.Buckets[5] != 100 || h.Buckets[math.Inf(1)] != 110 {
		t.Errorf("buckets = %v", h.Buckets)
	}
	if s.Counters["openfga_check_cache_hit_count"] != 40 {
		t.Errorf("cache hits = %v, want 40", s.Counters["openfga_check_cache_hit_count"])
	}
	if _, ok := s.Histograms["unrelated_metric"]; ok {
		t.Error("unrelated families must be ignored")
	}
}

func TestSnapshotDiffAndSummary(t *testing.T) {
	before := parsePrometheus(promFixture)
	after := parsePrometheus(promFixture)
	// simulate 100 more requests, all in the 1-5ms bucket, 600 more datastore queries
	h := after.Histograms["openfga_request_duration_ms"]
	h.Buckets[5] += 100
	h.Buckets[math.Inf(1)] += 100
	h.Sum += 300
	h.Count += 100
	q := after.Histograms["openfga_datastore_query_count"]
	q.Buckets[math.Inf(1)] += 100
	q.Sum += 600
	q.Count += 100
	after.Counters["openfga_check_cache_hit_count"] += 10

	sm := buildServerMetrics(before, after)
	if sm.RequestDuration.Count != 100 {
		t.Errorf("diffed request count = %v, want 100", sm.RequestDuration.Count)
	}
	if sm.RequestDuration.Mean != 3 {
		t.Errorf("mean = %v, want 3", sm.RequestDuration.Mean)
	}
	// all 100 diffed requests sit in the (1,5] bucket: p50 interpolates to 3
	if sm.RequestDuration.P50 != 3 {
		t.Errorf("p50 = %v, want 3", sm.RequestDuration.P50)
	}
	if sm.DatastoreQueryCount.Mean != 6 {
		t.Errorf("datastore queries per request = %v, want 6", sm.DatastoreQueryCount.Mean)
	}
	if sm.TotalDatastoreQueries != 600 {
		t.Errorf("total datastore queries = %v, want 600", sm.TotalDatastoreQueries)
	}
	if sm.CheckCacheHits != 10 || sm.CheckCacheTotal != 0 {
		t.Errorf("cache diff = %v/%v, want 10/0", sm.CheckCacheHits, sm.CheckCacheTotal)
	}
}

func TestDSQueryDiff(t *testing.T) {
	before := parsePrometheus(promFixture)
	after := parsePrometheus(promFixture)
	q := after.Histograms["openfga_datastore_query_count"]
	q.Buckets[math.Inf(1)] += 40
	q.Sum += 240 // 40 more checks, 240 more datastore queries -> 6 per check
	q.Count += 40

	sum, count := dsQueryDiff(before, after)
	if sum != 240 || count != 40 {
		t.Fatalf("dsQueryDiff = %v/%v, want 240/40", sum, count)
	}
	if got := sum / count; got != 6 {
		t.Errorf("queries per check = %v, want 6", got)
	}
}

// A snapshot with no datastore-query family must diff to (0, 0), not panic.
func TestDSQueryDiffMissingFamily(t *testing.T) {
	s := &snapshot{Histograms: map[string]*histogram{}, Counters: map[string]float64{}}
	if sum, count := dsQueryDiff(s, s); sum != 0 || count != 0 {
		t.Errorf("dsQueryDiff over empty snapshots = %v/%v, want 0/0", sum, count)
	}
}

func TestHistQuantileInfBucketReportsHighestFiniteBound(t *testing.T) {
	h := &histogram{Buckets: map[float64]float64{1: 0, 5: 10, math.Inf(1): 100}}
	if got := histQuantile(h, 0.99); got != 5 {
		t.Errorf("p99 in +Inf bucket = %v, want highest finite bound 5", got)
	}
}
