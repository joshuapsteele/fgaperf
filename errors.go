package main

// errors.go adds first-run hints around otherwise-correct low-level errors.

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

func configDecodeError(path string, raw []byte, err error) error {
	field, line := unknownYAMLField(err.Error())
	if field == "" {
		return fmt.Errorf("parsing config: %w", err)
	}
	where := ""
	if line > 0 {
		where = fmt.Sprintf(" at line %d", line)
	}
	hint := ""
	if suggestion := nearestString(field, knownYAMLFields()); suggestion != "" {
		hint = fmt.Sprintf("; did you mean `%s`?", suggestion)
	}
	if path == "" {
		path = "config"
	}
	return fmt.Errorf("parsing config %s: unknown field `%s`%s%s", path, field, where, hint)
}

func unknownYAMLField(msg string) (string, int) {
	const marker = "field "
	i := strings.Index(msg, marker)
	j := strings.Index(msg, " not found")
	if i < 0 || j < i {
		return "", 0
	}
	field := strings.Trim(msg[i+len(marker):j], "\"` ")
	line := 0
	if li := strings.LastIndex(msg[:i], "line "); li >= 0 {
		rest := msg[li+len("line "):]
		if colon := strings.IndexByte(rest, ':'); colon >= 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(rest[:colon])); err == nil {
				line = n
			}
		}
	}
	return field, line
}

func knownYAMLFields() []string {
	seen := map[string]bool{}
	var out []string
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			t = t.Elem()
		}
		if t.Kind() == reflect.Map {
			walk(t.Elem())
			return
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue
			}
			tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if tag != "" && tag != "-" && !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeOf(Config{}))
	return out
}

func nearestString(s string, candidates []string) string {
	best := ""
	bestDist := 99
	for _, c := range candidates {
		d := levenshtein(s, c)
		if d < bestDist {
			best, bestDist = c, d
		}
	}
	if bestDist > 3 {
		return ""
	}
	return best
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			cur[j] = minInt(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func minInt(a int, rest ...int) int {
	m := a
	for _, v := range rest {
		if v < m {
			m = v
		}
	}
	return m
}

func friendlyError(err error, cfg *Config) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	var hints []string
	if cfg != nil {
		if looksLikeConnectError(err) {
			hints = append(hints, openFGAReachabilityHint(cfg.OpenFGA.APIURL))
		}
		var he *HTTPError
		if errors.As(err, &he) {
			switch he.StatusCode {
			case 401, 403:
				hints = append(hints, "authentication failed; check `openfga.api_token` (or `openfga.oidc`) and the server's `OPENFGA_AUTHN_METHOD` setting")
			case 404:
				if strings.Contains(he.Path, "/stores/") {
					hints = append(hints, "the recorded store was not found; if `.fgaperf-state.json` is stale, run `fgaperf cleanup -all-stores` or rerun `fgaperf setup`")
				}
			}
		}
	}
	if len(hints) == 0 {
		return msg
	}
	return msg + "\n\n" + yellowErr("hint: "+strings.Join(hints, "\nhint: "))
}

func looksLikeConnectError(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "no such host") ||
		strings.Contains(lower, "network is unreachable") ||
		strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "i/o timeout")
}

func openFGAReachabilityHint(apiURL string) string {
	hint := fmt.Sprintf("OpenFGA not reachable at %s", apiURL)
	u, err := url.Parse(apiURL)
	if err == nil {
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return hint + " — run `docker compose up -d`, then check `docker compose ps` and the published port"
		}
	}
	return hint + " — check the URL, port, network path, and server health"
}

func modelLoadError(path string, err error) string {
	if err == nil {
		return ""
	}
	msg := fmt.Sprintf("%v\nfgaperf looked for a compiled OpenFGA authorization model at %s.", err, path)
	if strings.Contains(err.Error(), "reading model file") {
		msg += "\nCreate one from a DSL model with: `fga model transform --file model.fga > model.json`, then set `model_file` in the config."
	}
	if strings.Contains(err.Error(), "parsing model JSON") {
		msg += "\nThe file must be compiled OpenFGA model JSON; if you have DSL, run: `fga model transform --file model.fga > model.json`."
	}
	return msg
}
