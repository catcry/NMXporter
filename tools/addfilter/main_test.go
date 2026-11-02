package main

import "testing"

func TestAddMatcher(t *testing.T) {
	m := `eventlink=~"$eventlink"`
	for in, want := range map[string]string{
		`time() - max(directory_last_scan_timestamp_seconds)`:                                        `time() - max(directory_last_scan_timestamp_seconds{` + m + `})`,
		`(min(up{job="directory-exporter"}) or vector(0))`:                                           `(min(up{` + m + `,job="directory-exporter"}) or vector(0))`,
		`sum by (stream) (rate(eventlink_latency_match_total{stream=~"$stream",method="name"}[1h]))`: `sum by (stream) (rate(eventlink_latency_match_total{` + m + `,stream=~"$stream",method="name"}[1h]))`,
		// Selector without a metric name; metric names inside the regex string stay.
		`max by (stream,node) ({__name__=~"directory_a|eventlink_b",stream=~"$stream"})`: `max by (stream,node) ({` + m + `,__name__=~"directory_a|eventlink_b",stream=~"$stream"})`,
		// Variables, label names and functions are not selectors.
		`count(x_metric > $delay) or vector(0)`:                                                     `count(x_metric > $delay) or vector(0)`,
		`label_replace(directory_file_count, "node", "$1", "type", "([^/]+)/.*")`:                   `label_replace(directory_file_count{` + m + `}, "node", "$1", "type", "([^/]+)/.*")`,
		`histogram_quantile(0.95, sum by (le) (rate(directory_stream_latency_seconds_bucket[1h])))`: `histogram_quantile(0.95, sum by (le) (rate(directory_stream_latency_seconds_bucket{` + m + `}[1h])))`,
		`group_left(node_type) directory_node_info`:                                                 `group_left(node_type) directory_node_info{` + m + `}`,
		`label_values(directory_file_count, stream)`:                                                `label_values(directory_file_count{` + m + `}, stream)`,
	} {
		if got := AddMatcher(in); got != want {
			t.Errorf("AddMatcher(%s)\n got %s\nwant %s", in, got, want)
		}
	}
	// Idempotent.
	once := AddMatcher(`up{job="x"}`)
	if AddMatcher(once) != once {
		t.Errorf("second run changed %s to %s", once, AddMatcher(once))
	}
}
