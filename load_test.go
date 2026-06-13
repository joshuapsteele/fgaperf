package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "deadline exceeded" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&HTTPError{StatusCode: 429}, "4xx"},
		{&HTTPError{StatusCode: 503}, "5xx"},
		{fakeTimeoutErr{}, "timeout"},
		{&json.SyntaxError{}, "decode"},
		{errors.New("connection refused"), "connection"},
		{fmt.Errorf("wrapped: %w", &HTTPError{StatusCode: 500}), "5xx"},
	}
	for _, c := range cases {
		if got := classifyErr(c.err); got != c.want {
			t.Errorf("classifyErr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// Without weights the picker must stay uniform over entries; with weights the
// per-target request share must follow the weights even when group sizes
// differ.
func TestCorpusPickerWeights(t *testing.T) {
	corpus := &Corpus{Entries: []CorpusEntry{
		{Target: "a#r", Object: "a:1"},
		{Target: "a#r", Object: "a:2"},
		{Target: "a#r", Object: "a:3"},
		{Target: "b#r", Object: "b:1"},
	}}
	rng := rand.New(rand.NewSource(1))

	uniform := newCorpusPicker(corpus)
	counts := map[string]int{}
	for i := 0; i < 40000; i++ {
		counts[uniform.pick(rng).Target]++
	}
	if frac := float64(counts["a#r"]) / 40000; frac < 0.72 || frac > 0.78 {
		t.Errorf("uniform picker: a#r share = %.3f, want ~0.75 (uniform over entries)", frac)
	}

	corpus.Weights = map[string]float64{"a#r": 1, "b#r": 3}
	weighted := newCorpusPicker(corpus)
	counts = map[string]int{}
	for i := 0; i < 40000; i++ {
		counts[weighted.pick(rng).Target]++
	}
	if frac := float64(counts["b#r"]) / 40000; frac < 0.72 || frac > 0.78 {
		t.Errorf("weighted picker: b#r share = %.3f, want ~0.75 (weight 3 of 4)", frac)
	}
}

func TestMismatchRecorderDedupsAndCaps(t *testing.T) {
	r := newMismatchRecorder()
	e := &CorpusEntry{User: "user:1", Relation: "viewer", Object: "doc:1", Target: "doc#viewer", Expected: true}
	r.record(e, false)
	r.record(e, false) // duplicate
	if len(r.out) != 1 {
		t.Fatalf("got %d records, want 1 after dedup", len(r.out))
	}
	if r.out[0].Expected != true || r.out[0].Observed != false {
		t.Errorf("record = %+v", r.out[0])
	}
	eWithContext := *e
	eWithContext.Context = map[string]any{"required_scope": "admin"}
	r.record(&eWithContext, false)
	if len(r.out) != 2 {
		t.Fatalf("same tuple with different request context should not dedup; got %d records", len(r.out))
	}
	for i := 0; i < 2*maxMismatchRecords; i++ {
		r.record(&CorpusEntry{User: fmt.Sprintf("user:%d", i), Relation: "viewer", Object: "doc:1"}, false)
	}
	if len(r.out) != maxMismatchRecords {
		t.Errorf("got %d records, want cap %d", len(r.out), maxMismatchRecords)
	}
}

func TestBatchCheckItemErrorsAreReported(t *testing.T) {
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"c0": map[string]any{"allowed": false},
				"c1": map[string]any{"error": map[string]any{"message": "boom"}},
			},
		})
	})
	defer srv.Close()

	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Load.BatchSize = 2
	cfg.Load.VerifyResults = true
	corpus := &Corpus{StoreID: "store", ModelID: "model", Entries: []CorpusEntry{
		{User: "user:1", Relation: "viewer", Object: "doc:1", Target: "doc#viewer", Expected: true},
		{User: "user:2", Relation: "viewer", Object: "doc:2", Target: "doc#viewer", Expected: true},
	}}

	s := doBatch(client, newCorpusPicker(corpus), newMismatchRecorder(), corpus, cfg, rand.New(rand.NewSource(1)))
	// The HTTP call succeeded, so the sample must NOT be flagged as a transport
	// error (that flag excludes its latency from the percentiles). The item
	// error is surfaced via ItemErrors / ItemErrMsg instead.
	if s.Err {
		t.Fatal("an item-level error must not mark the whole batch as a transport error")
	}
	if s.ItemErrors != 1 {
		t.Fatalf("ItemErrors = %d, want 1", s.ItemErrors)
	}
	if !strings.Contains(s.ItemErrMsg, "boom") {
		t.Fatalf("ItemErrMsg = %q, want item error detail", s.ItemErrMsg)
	}
	if !s.Mismatch {
		t.Fatal("successful batch items should still be verified for mismatches")
	}
}

// A batch-check whose HTTP round trip succeeded carries a real service latency
// even when an individual item reports an application-level error. That latency
// must stay in the percentile populations; only a transport failure (no valid
// latency) is excluded.
func TestBatchCheckItemErrorLatencyCounted(t *testing.T) {
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"c0": map[string]any{"allowed": true},
				"c1": map[string]any{"error": map[string]any{"message": "boom"}},
			},
		})
	})
	defer srv.Close()

	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Load.BatchSize = 2
	corpus := &Corpus{StoreID: "store", ModelID: "model", Entries: []CorpusEntry{
		{User: "user:1", Relation: "viewer", Object: "doc:1", Target: "doc#viewer", Expected: true},
		{User: "user:2", Relation: "viewer", Object: "doc:2", Target: "doc#viewer", Expected: true},
	}}

	s := doBatch(client, newCorpusPicker(corpus), newMismatchRecorder(), corpus, cfg, rand.New(rand.NewSource(1)))
	if s.ItemErrors != 1 {
		t.Fatalf("ItemErrors = %d, want 1", s.ItemErrors)
	}
	st := Summarize([]Sample{s})
	if st.Count != 1 {
		t.Fatalf("partially-errored batch dropped from the latency population: Count = %d, want 1", st.Count)
	}
	if st.Errors != 0 {
		t.Fatalf("item error counted as a latency-excluding error: Errors = %d, want 0", st.Errors)
	}
	if st.P50 != s.Latency {
		t.Fatalf("batch latency %v not reflected in percentiles (P50 = %v)", s.Latency, st.P50)
	}
}

func TestWarmupMismatchesExcludedFromMeasuredCounts(t *testing.T) {
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]bool{"allowed": false})
	})
	defer srv.Close()

	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Load.Concurrency = 1
	cfg.Load.Warmup = 40 * time.Millisecond
	cfg.Load.Duration = 40 * time.Millisecond
	cfg.Load.VerifyResults = true
	corpus := &Corpus{StoreID: "store", ModelID: "model", Entries: []CorpusEntry{
		{User: "user:1", Relation: "viewer", Object: "doc:1", Target: "doc#viewer", Expected: true},
	}}
	res, err := RunLoad(client, corpus, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	measured := res.loadStats().overall.Stats().Count
	if res.Mismatches != int64(measured) {
		t.Fatalf("reported mismatches = %d, want measured-sample count %d", res.Mismatches, measured)
	}
}

// A fixed-rate run issues at the offered rate during warmup too. On a cold
// server, workers can fall a full buffer behind precisely during warmup and
// drop slots — but those drops are a warmup artifact, not a measured-phase
// capacity signal, so they must stay out of DroppedSlots (which the report
// presents as a measured-phase quantity). A run that saturates the *measured*
// phase must still report a non-zero count, so the gate is "warmup excluded",
// not "never counts". These cases lean on timing; margins are generous, and the
// ~1s buffer-fill floor (buffer cap == rate) is inherent to the drop mechanism.
func TestFixedRateDroppedSlotsExcludeWarmup(t *testing.T) {
	corpus := &Corpus{StoreID: "store", ModelID: "model", Entries: []CorpusEntry{
		{User: "user:1", Relation: "viewer", Object: "doc:1", Target: "doc#viewer", Expected: true},
	}}

	t.Run("warmup-only slowness is not counted", func(t *testing.T) {
		var slow atomic.Bool
		slow.Store(true)
		client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
			if slow.Load() {
				time.Sleep(250 * time.Millisecond) // unsustainable at 100 req/s with 1 worker
			}
			json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
		})
		defer srv.Close()
		// Server turns instant well before the measured window starts, so the
		// single worker drains the warmup backlog and the measured phase is clean.
		time.AfterFunc(1200*time.Millisecond, func() { slow.Store(false) })

		cfg, err := LoadConfigFile("")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Load.Rate = 100
		cfg.Load.Concurrency = 1
		cfg.Load.Warmup = 1700 * time.Millisecond
		cfg.Load.Duration = 400 * time.Millisecond

		res, err := RunLoad(client, corpus, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.DroppedSlots != 0 {
			t.Fatalf("DroppedSlots = %d, want 0 (warmup-phase drops must be excluded)", res.DroppedSlots)
		}
	})

	t.Run("measured-phase slowness is counted", func(t *testing.T) {
		client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(250 * time.Millisecond) // slow for the whole run
			json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
		})
		defer srv.Close()

		cfg, err := LoadConfigFile("")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Load.Rate = 100
		cfg.Load.Concurrency = 1
		cfg.Load.Warmup = 100 * time.Millisecond
		cfg.Load.Duration = 1300 * time.Millisecond

		res, err := RunLoad(client, corpus, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.DroppedSlots == 0 {
			t.Fatal("DroppedSlots = 0, want > 0 (sustained measured-phase saturation must still count)")
		}
	})
}

// Response latency must be summarized independently of service latency so the
// fixed-rate report can show queueing delay.
func TestSummarizeResponseUsesRespLatency(t *testing.T) {
	samples := []Sample{
		{Latency: 1 * time.Millisecond, RespLatency: 100 * time.Millisecond, Items: 1},
		{Latency: 2 * time.Millisecond, RespLatency: 200 * time.Millisecond, Items: 1},
	}
	svc := Summarize(samples)
	resp := SummarizeResponse(samples)
	if svc.Max != 2*time.Millisecond {
		t.Errorf("service max = %v, want 2ms", svc.Max)
	}
	if resp.Max != 200*time.Millisecond {
		t.Errorf("response max = %v, want 200ms", resp.Max)
	}
}

// The raw sample export must round-trip through gzip, and a second writer in
// append mode must concatenate (so a sweep's steps share one file).
func TestSampleWriterGzipRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl.gz")
	now := time.Now()
	sw, err := newSampleWriter(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.write(Sample{Target: "document#viewer", Latency: 3 * time.Millisecond, Completed: now, Items: 1, Conditioned: true}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := sw.write(Sample{Target: "document#editor", Latency: 5 * time.Millisecond, Completed: now, Items: 1, Err: true, ErrClass: "5xx"}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	// Append a second stream, as a later sweep step would.
	sw2, err := newSampleWriter(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw2.write(Sample{Target: "document#viewer", Latency: 4 * time.Millisecond, Completed: now, Items: 1}, 2000); err != nil {
		t.Fatal(err)
	}
	if err := sw2.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f) // multistream by default: reads both appended streams
	if err != nil {
		t.Fatal(err)
	}
	var recs []SampleRecord
	sc := bufio.NewScanner(gz)
	for sc.Scan() {
		var r SampleRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	if recs[0].Target != "document#viewer" || recs[0].LatencyNs != int64(3*time.Millisecond) || !recs[0].Conditioned || recs[0].OfferedRate != 1000 {
		t.Errorf("record 0 wrong: %+v", recs[0])
	}
	if !recs[1].Err || recs[1].ErrClass != "5xx" {
		t.Errorf("record 1 should carry the error class: %+v", recs[1])
	}
	if recs[2].OfferedRate != 2000 {
		t.Errorf("appended record should carry the new rate: %+v", recs[2])
	}
}

func TestSampleWriterReportsEncodeErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	sw, err := newSampleWriter(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sw.write(Sample{Target: "document#viewer", Latency: time.Millisecond, Completed: time.Now(), Items: 1}, 0); err == nil {
		t.Fatal("write to closed sample file succeeded")
	}
}

func TestRunLoadOpensSampleFileBeforeWorkers(t *testing.T) {
	var calls atomic.Int64
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
	})
	defer srv.Close()

	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Load.Concurrency = 4
	cfg.Load.Warmup = 0
	cfg.Load.Duration = 100 * time.Millisecond
	cfg.Load.SampleFile = filepath.Join(t.TempDir(), "missing", "samples.jsonl")
	corpus := &Corpus{StoreID: "store", ModelID: "model", Entries: []CorpusEntry{
		{User: "user:1", Relation: "viewer", Object: "doc:1", Target: "doc#viewer", Expected: true},
	}}

	if _, err := RunLoad(client, corpus, cfg, nil); err == nil {
		t.Fatal("RunLoad succeeded with an unwritable sample_file")
	}
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("workers started before sample_file open failed; saw %d requests", got)
	}
}

func TestRunLoadAggregatesWithoutRetainingSamples(t *testing.T) {
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
	})
	defer srv.Close()

	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Load.Concurrency = 1
	cfg.Load.Warmup = 0
	cfg.Load.Duration = 30 * time.Millisecond
	cfg.Load.SampleFile = filepath.Join(t.TempDir(), "samples.jsonl")
	corpus := &Corpus{StoreID: "store", ModelID: "model", Entries: []CorpusEntry{
		{User: "user:1", Relation: "viewer", Object: "doc:1", Target: "doc#viewer", Expected: true},
	}}

	res, err := RunLoad(client, corpus, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Samples) != 0 {
		t.Fatalf("RunLoad retained %d raw samples, want 0", len(res.Samples))
	}
	st := res.loadStats().overall.Stats()
	if st.Count == 0 {
		t.Fatal("aggregate stats recorded no successful samples")
	}

	f, err := os.Open(cfg.Load.SampleFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != res.loadStats().TotalSamples() {
		t.Fatalf("sample_file lines = %d, want aggregate sample count %d", lines, res.loadStats().TotalSamples())
	}
}
