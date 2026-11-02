// addfilter makes the dashboard usable with several EventLink servers in one
// Prometheus: it adds the matcher eventlink=~"$eventlink" to every metric
// selector of every query, adds the "EventLink server" variable and replaces
// texts that name one server.
//
//	go run ./deploy/dashboard/addfilter -in dashboard.json -out package.json
//
// The eventlink label is set by the Prometheus scrape config (one value per
// EventLink server). Without it, eventlink=~".*" (All) still matches.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

const matcher = `eventlink=~"$eventlink"`

// metricPrefixes are the metric names the dashboard queries.
var metricPrefixes = []string{"directory_", "eventlink_", "process_", "go_"}

func isMetric(name string) bool {
	if name == "up" {
		return true
	}
	for _, p := range metricPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdent(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// skipString returns the index after the string literal starting at i.
func skipString(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' && q != '`' {
			j++
			continue
		}
		if s[j] == q {
			return j + 1
		}
	}
	return len(s)
}

// AddMatcher adds matcher to every selector of a PromQL expression: to
// {…} blocks and to bare metric names. Strings, function names and Grafana
// variables ($x, ${x}) are left alone.
func AddMatcher(expr string) string {
	var b strings.Builder
	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			j := skipString(expr, i)
			b.WriteString(expr[i:j])
			i = j
		case c == '$':
			j := i + 1
			if j < len(expr) && expr[j] == '{' {
				for j < len(expr) && expr[j] != '}' {
					j++
				}
				j++
			} else {
				for j < len(expr) && isIdent(expr[j]) {
					j++
				}
			}
			b.WriteString(expr[i:min(j, len(expr))])
			i = j
		case c == '{':
			// A selector's label matchers: insert ours first.
			j := i + 1
			for j < len(expr) && expr[j] != '}' {
				if expr[j] == '"' || expr[j] == '\'' || expr[j] == '`' {
					j = skipString(expr, j)
					continue
				}
				j++
			}
			inner := strings.TrimSpace(expr[i+1 : min(j, len(expr))])
			switch {
			case strings.Contains(inner, matcher):
				b.WriteString("{" + inner + "}")
			case inner == "":
				b.WriteString("{" + matcher + "}")
			default:
				b.WriteString("{" + matcher + "," + inner + "}")
			}
			i = j + 1
		case isIdentStart(c) && (i == 0 || !isIdent(expr[i-1])):
			j := i
			for j < len(expr) && isIdent(expr[j]) {
				j++
			}
			name := expr[i:j]
			b.WriteString(name)
			k := j
			for k < len(expr) && expr[k] == ' ' {
				k++
			}
			next := byte(0)
			if k < len(expr) {
				next = expr[k]
			}
			if isMetric(name) && next != '(' && next != '{' {
				b.WriteString("{" + matcher + "}")
			}
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// texts that name one server or one day on 102.
var replacements = [][2]string{
	{"172.29.11.102", "<eventlink-server>"},
	{" On 2026-10-31 it shut down at 04:01:21 (BufferMonitor is not working).", ""},
}

func walk(v any, inTemplating bool) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch {
			case k == "expr":
				if s, ok := val.(string); ok {
					x[k] = AddMatcher(s)
					continue
				}
			case k == "query" && inTemplating:
				if s, ok := val.(string); ok && x["type"] == nil {
					x[k] = AddMatcher(s) // query.query of a variable
					continue
				}
				if m, ok := val.(map[string]any); ok {
					if q, ok := m["query"].(string); ok && x["type"] == "query" {
						m["query"] = AddMatcher(q)
					}
					continue
				}
			}
			x[k] = walk(val, inTemplating || k == "templating")
		}
		return x
	case []any:
		for i := range x {
			x[i] = walk(x[i], inTemplating)
		}
		return x
	case string:
		for _, r := range replacements {
			x = strings.ReplaceAll(x, r[0], r[1])
		}
		return x
	}
	return v
}

var serverVariable = map[string]any{
	"name":        "eventlink",
	"label":       "EventLink server",
	"description": "Value of the eventlink label set in the Prometheus scrape config, one per EventLink server.",
	"type":        "query",
	"datasource":  map[string]any{"type": "prometheus", "uid": "${ds}"},
	"query":       map[string]any{"query": "label_values(up{job=~\"directory-exporter|eventlink-trace-exporter|eventlink-db-exporter\"}, eventlink)", "refId": "eventlink"},
	"definition":  "label_values(up{job=~\"directory-exporter|eventlink-trace-exporter|eventlink-db-exporter\"}, eventlink)",
	"includeAll":  true,
	"allValue":    ".*",
	"multi":       false,
	"refresh":     2,
	"sort":        1,
	"current":     map[string]any{"text": "All", "value": "$__all"},
	"hide":        0,
}

func main() {
	in := flag.String("in", "", "dashboard JSON")
	out := flag.String("out", "", "output JSON")
	flag.Parse()
	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var d map[string]any
	if err := dec.Decode(&d); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	walk(d, false)
	// The server variable goes right after the data source.
	if t, ok := d["templating"].(map[string]any); ok {
		if list, ok := t["list"].([]any); ok {
			var nl []any
			for _, v := range list {
				nl = append(nl, v)
				if m, ok := v.(map[string]any); ok && m["name"] == "ds" {
					nl = append(nl, serverVariable)
				}
			}
			t["list"] = nl
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
