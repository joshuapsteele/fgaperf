package main

// replay.go builds the corpus from a real check log instead of synthesizing
// one from the model. Teams with an OpenFGA request log or app audit trail can
// replay *that* distribution so the load mix matches production and probe
// synthesis is bypassed (corpus_source: replay, replay.file).
//
// The log is JSONL — one check request per line:
//
//	{"user":"user:1","relation":"viewer","object":"document:1"}
//	{"user":"user:2","relation":"editor","object":"document:9","context":{"scope":"write"}}
//	{"user":"user:3","relation":"viewer","object":"document:1","contextual_tuples":[{"user":"user:3","relation":"member","object":"group:g1"}]}
//
// Unknown fields are ignored, so a raw OpenFGA request log (which also carries
// store IDs, timestamps, consistency, etc.) can be fed in directly.
//
// Principle #2 (probe-then-replay) is preserved: each *distinct* log entry is
// executed once at HIGHER_CONSISTENCY to learn ground truth, exactly as the
// probe does for synthesized candidates — outcomes are never predicted
// statically. The corpus then carries the log's natural per-target frequencies
// as load weights (reused by corpusPicker), so the load-phase traffic mix
// follows the log's target distribution. Within a target, replay picks
// uniformly over the distinct checks observed for it.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

// replayLine is the on-disk JSONL shape. user, relation, and object are
// required; contextual_tuples and context are optional. Extra fields in the
// log are ignored.
type replayLine struct {
	User             string         `json:"user"`
	Relation         string         `json:"relation"`
	Object           string         `json:"object"`
	ContextualTuples []TupleKey     `json:"contextual_tuples,omitempty"`
	Context          map[string]any `json:"context,omitempty"`
}

// replayLog is the parsed log: distinct check entries (first-seen order) plus
// the natural per-target counts that weight the load mix.
type replayLog struct {
	distinct   []CorpusEntry      // one per unique (user, relation, object, contextual_tuples, context)
	weights    map[string]float64 // target -> total well-formed log lines for that target
	total      int                // well-formed lines
	skipped    int                // malformed/invalid lines skipped
	errSamples []string           // first few skip reasons, for the operator
}

// maxReplayLineBytes bounds a single JSONL line. Real request logs can carry
// large context maps, so this is generous; a line over the cap is skipped (not
// fatal) like any other malformed line.
const maxReplayLineBytes = 16 << 20 // 16 MiB

// parseReplayLog reads a JSONL check log, deduplicating entries and tallying
// per-target counts. Blank lines are ignored; malformed or incomplete lines
// are counted and skipped (with a few sample reasons retained), never fatal.
// conditioned[target] tags each entry's CEL reachability for the report.
func parseReplayLog(r io.Reader, conditioned map[string]bool) (*replayLog, error) {
	log := &replayLog{weights: map[string]float64{}}
	distinctIdx := map[string]int{}
	br := bufio.NewReader(r)
	lineNo := 0
	skip := func(reason string) {
		log.skipped++
		if len(log.errSamples) < 3 {
			log.errSamples = append(log.errSamples, fmt.Sprintf("line %d: %s", lineNo, reason))
		}
	}
	for {
		line, tooLong, err := readReplayLine(br, maxReplayLineBytes)
		if err == io.EOF && len(line) == 0 && !tooLong {
			break
		}
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading replay log: %w", err)
		}
		atEOF := err == io.EOF
		lineNo++
		if tooLong {
			skip(fmt.Sprintf("line exceeds %d bytes", maxReplayLineBytes))
			if atEOF {
				break
			}
			continue
		}
		raw := bytes.TrimSpace(line)
		if len(raw) == 0 {
			if atEOF {
				break
			}
			continue // blank lines are not errors
		}
		var ln replayLine
		if err := json.Unmarshal(raw, &ln); err != nil {
			skip("malformed JSON: " + err.Error())
			if atEOF {
				break
			}
			continue
		}
		if ln.User == "" || ln.Relation == "" || ln.Object == "" {
			skip("missing required user/relation/object")
			if atEOF {
				break
			}
			continue
		}
		objType := typeOfObject(ln.Object)
		if objType == "" {
			skip(fmt.Sprintf("object %q has no type prefix (want type:id)", ln.Object))
			if atEOF {
				break
			}
			continue
		}
		target := objType + "#" + ln.Relation
		log.total++
		log.weights[target]++
		e := CorpusEntry{
			User:             ln.User,
			Relation:         ln.Relation,
			Object:           ln.Object,
			ContextualTuples: ln.ContextualTuples,
			Context:          ln.Context,
			Target:           target,
			Conditioned:      conditioned[target],
			Contextual:       len(ln.ContextualTuples) > 0,
		}
		key := e.key()
		if _, ok := distinctIdx[key]; ok {
			continue
		}
		distinctIdx[key] = len(log.distinct)
		log.distinct = append(log.distinct, e)
		if atEOF {
			break
		}
	}
	return log, nil
}

func readReplayLine(r *bufio.Reader, maxBytes int) ([]byte, bool, error) {
	var line []byte
	contentLen := 0
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			contentLen += replayLineContentLen(chunk)
			if contentLen > maxBytes {
				tooLong = true
				line = nil
			}
			if !tooLong {
				line = append(line, chunk...)
			}
		}
		switch err {
		case nil:
			return line, tooLong, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(chunk) == 0 {
				return nil, false, io.EOF
			}
			return line, tooLong, io.EOF
		default:
			return nil, false, err
		}
	}
}

func replayLineContentLen(chunk []byte) int {
	n := len(chunk)
	if n > 0 && chunk[n-1] == '\n' {
		n--
		if n > 0 && chunk[n-1] == '\r' {
			n--
		}
	}
	return n
}

// BuildReplayCorpus reads cfg.Replay.File, learns ground truth for each
// distinct entry, and assembles a corpus whose load weights reproduce the
// log's per-target frequency. The Analysis supplies CEL-reachability tags and
// churn templates (the same metadata BuildCorpus records); the seeded store
// referenced by storeID is what the ground-truth checks run against, so the
// log's user/object IDs must exist in that store for outcomes to be meaningful.
func BuildReplayCorpus(client *FGAClient, a *Analysis, cfg *Config, storeID, modelID string) (*Corpus, error) {
	f, err := os.Open(cfg.Replay.File)
	if err != nil {
		return nil, fmt.Errorf("opening replay log %q: %w", cfg.Replay.File, err)
	}
	defer f.Close()

	log, err := parseReplayLog(f, a.Conditioned)
	if err != nil {
		return nil, err
	}
	if log.skipped > 0 {
		fmt.Fprintln(os.Stderr, yellowErr(fmt.Sprintf("replay: skipped %d malformed line(s) in %s", log.skipped, cfg.Replay.File)))
		for _, s := range log.errSamples {
			fmt.Fprintln(os.Stderr, yellowErr("replay:   "+s))
		}
	}
	if len(log.distinct) == 0 {
		return nil, fmt.Errorf("replay log %q yielded no usable checks (%d lines skipped); each line needs user, relation, and a type:id object", cfg.Replay.File, log.skipped)
	}
	fmt.Printf("replay: %d log lines (%d distinct checks across %d targets)\n", log.total, len(log.distinct), len(log.weights))

	valid := classifyCandidates(client, storeID, modelID, log.distinct, cfg.Probe.Concurrency, "replay")
	// Keep entries in a stable target order, matching BuildCorpus, so the
	// persisted corpus is deterministic for a given log + store.
	sort.SliceStable(valid, func(i, j int) bool { return valid[i].Target < valid[j].Target })

	c := &Corpus{StoreID: storeID, ModelID: modelID, Entries: valid}
	c.Stats = c.TargetStats()
	// Weights are the log's natural per-target counts. corpusPicker draws a
	// target proportionally to its weight, then an entry uniformly within it,
	// so the load-phase target mix follows the log. Orphan weights (targets
	// whose entries all errored out) are harmless: corpusPicker only consults
	// weights for targets that have surviving entries.
	c.Weights = log.weights
	c.ChurnTemplates = churnTemplates(a)
	return c, nil
}
