package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// EventLink stores times in three zones:
//   - *gmt columns: UTC;
//   - *local columns and el_component_status.updatetime: the database's zone;
//   - *system columns and the reporting tables: the "system" zone, whose
//     offset is in el_events.eventtimesystemtz (minutes; +03:30 on 102).
// Times are read as text and converted here, never with EXTRACT on a
// timestamp without time zone (its meaning changed between PostgreSQL versions).

const tsLayout = "2006-01-02 15:04:05.999999999"

// parseNominal parses a timestamp without time zone as text.
func parseNominal(s string) (time.Time, bool) {
	t, err := time.Parse(tsLayout, s)
	return t, err == nil
}

// NodeKey identifies a node by stream and node name.
type NodeKey struct{ Stream, Node string }

// NodeStatus is one row of el_node_log for a node of a current stream version.
type NodeStatus struct {
	Host, NodeType, Status string
	LastStart              float64
}

// Component is one row of el_component_status.
type Component struct {
	Name, Host, Status string
	Updated            float64
	Up                 bool // EventLink rule: status R and updated within nodemanager_grace
}

// NodeEvents is what el_events says about one node.
type NodeEvents struct {
	Count       map[string]uint64  // by event type (E, W, I), since exporter start
	Last        map[string]float64 // newest event per type (seeded)
	LastFailure float64            // newest event with a failure key (seeded)
	ErrKey      string             // newest E event
	ErrMsg      string
}

// StreamFlow is what the reporting tables say about one stream.
type StreamFlow struct {
	LastCollected, LastDistributed float64
	Files, Bytes, Records          [2]uint64 // [collected, distributed], since exporter start
}

// Snapshot is a copy of one database's state for rendering.
type Snapshot struct {
	Name         string
	Ready        bool // first poll finished (successfully or not)
	Up           bool // last poll succeeded
	LastPoll     time.Time
	PollDuration time.Duration
	Errors       map[string]uint64 // failed queries by name
	TZOffset     int               // system zone offset, seconds
	Components   []Component
	Nodes        map[NodeKey]NodeStatus
	Events       map[NodeKey]NodeEvents
	Streams      map[string]StreamFlow
	BudgetHits   uint64
}

// flow follows one reporting table. Counters: each poll the database sums
// the rows of the slice (cut, now − lag], so every row is counted once if it
// is written within reporting_lag of its time. Last times: the newest row
// per stream above the newest time seen so far. Both use the time index.
type flow struct {
	table, tsCol, bytesCol, recCol string
	idx                            int       // 0 collected, 1 distributed
	cut                            time.Time // rows up to here are counted (nominal system time)
	newest                         time.Time // newest row time seen
}

// nextSlice returns the slice to count now, or false when it is empty.
func (f *flow) nextSlice(systemNow time.Time, lag time.Duration) (from, to time.Time, ok bool) {
	to = systemNow.Add(-lag)
	if !to.After(f.cut) {
		return f.cut, f.cut, false
	}
	return f.cut, to, true
}

// poller queries one database.
type poller struct {
	cfg *Config
	db  Database
	log *slog.Logger
	sql *sql.DB

	seeded   bool
	tzOffset int
	eventWM  int64
	flows    [2]*flow
	nodeName map[int64]NodeKey // nodeid → stream/node, all versions
	streams  map[int64]string  // streamid → name

	mu sync.RWMutex
	st Snapshot
}

func newPoller(cfg *Config, d Database, log *slog.Logger) (*poller, error) {
	db, err := sql.Open("postgres", d.dsn(cfg.QueryTimeout))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Hour)
	p := &poller{cfg: cfg, db: d, log: log.With("server", d.Name), sql: db}
	p.flows[0] = &flow{table: "el_reporting_input_details", tsCol: "collectionend", bytesCol: "bytesin", recCol: "streamrecordsin", idx: 0}
	p.flows[1] = &flow{table: "el_reporting_output_details", tsCol: "distributionend", bytesCol: "bytesout", recCol: "streamrecordsout", idx: 1}
	p.st = Snapshot{Name: d.Name, Errors: map[string]uint64{}, Nodes: map[NodeKey]NodeStatus{},
		Events: map[NodeKey]NodeEvents{}, Streams: map[string]StreamFlow{}}
	return p, nil
}

func (p *poller) snapshot() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s := p.st
	s.Errors = copyMap(p.st.Errors)
	s.Components = append([]Component(nil), p.st.Components...)
	s.Nodes = copyMap(p.st.Nodes)
	s.Streams = copyMap(p.st.Streams)
	s.Events = make(map[NodeKey]NodeEvents, len(p.st.Events))
	for k, v := range p.st.Events {
		v.Count, v.Last = copyMap(v.Count), copyMap(v.Last)
		s.Events[k] = v
	}
	return s
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	c := make(map[K]V, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func (p *poller) run(ctx context.Context) {
	p.poll(ctx)
	t := time.NewTicker(p.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

// poll runs all queries in one read-only transaction. A failing query is
// counted and logged; the others still run.
func (p *poller) poll(ctx context.Context) {
	t0 := time.Now()
	ok := true
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("poll panicked", "panic", r)
			ok = false
		}
		p.mu.Lock()
		p.st.Ready, p.st.Up, p.st.LastPoll, p.st.PollDuration = true, ok, time.Now(), time.Since(t0)
		p.mu.Unlock()
	}()

	tx, err := p.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		p.fail("connect", err)
		ok = false
		return
	}
	defer tx.Rollback() // read-only: nothing to commit

	step := func(name string, f func(*sql.Tx) error) {
		if ctx.Err() != nil {
			return
		}
		// A failed statement aborts the transaction; a savepoint keeps the rest usable.
		if _, err := tx.ExecContext(ctx, "SAVEPOINT q"); err != nil {
			p.fail(name, err)
			ok = false
			return
		}
		if err := f(tx); err != nil {
			p.fail(name, err)
			ok = false
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT q")
			return
		}
		_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT q")
	}

	step("timezone", p.queryTimezone)
	step("names", p.queryNames)
	step("components", p.queryComponents)
	step("nodes", p.queryNodes)
	if !p.seeded {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", p.cfg.SeedTimeout.Milliseconds())); err == nil {
			seedOK := true
			wrap := func(f func(*sql.Tx) error) func(*sql.Tx) error {
				return func(tx *sql.Tx) error {
					err := f(tx)
					if err != nil {
						seedOK = false
					}
					return err
				}
			}
			step("seed_events", wrap(p.seedEvents))
			step("seed_reporting", wrap(p.seedReporting))
			p.seeded = seedOK
			_, _ = tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", p.cfg.QueryTimeout.Milliseconds()))
			if seedOK {
				p.log.Info("startup seed finished", "duration", time.Since(t0).Round(time.Millisecond))
			}
		}
	}
	if p.seeded {
		step("events", p.queryEvents)
		step("collected", func(tx *sql.Tx) error { return p.queryFlow(tx, p.flows[0]) })
		step("distributed", func(tx *sql.Tx) error { return p.queryFlow(tx, p.flows[1]) })
	}
}

func (p *poller) fail(query string, err error) {
	p.log.Warn("query failed", "query", query, "error", err)
	p.mu.Lock()
	p.st.Errors[query]++
	p.mu.Unlock()
}

// queryTimezone reads the system zone offset from the newest event, unless
// system_tz_offset is set.
func (p *poller) queryTimezone(tx *sql.Tx) error {
	if p.db.SystemTZOffset != "auto" {
		p.tzOffset, _ = parseOffset(p.db.SystemTZOffset)
	} else {
		var m sql.NullInt64
		err := tx.QueryRow("SELECT eventtimesystemtz FROM el_events ORDER BY eventid DESC LIMIT 1").Scan(&m)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if m.Valid {
			p.tzOffset = int(m.Int64) * 60
		}
	}
	p.mu.Lock()
	p.st.TZOffset = p.tzOffset
	p.mu.Unlock()
	return nil
}

// systemEpoch converts a *system / reporting column value to Unix seconds.
func (p *poller) systemEpoch(t time.Time) float64 {
	return float64(t.Unix()-int64(p.tzOffset)) + float64(t.Nanosecond())/1e9
}

// systemNow is now in the system zone, as a nominal time.
func (p *poller) systemNow() time.Time {
	return time.Now().UTC().Add(time.Duration(p.tzOffset) * time.Second)
}

func (p *poller) queryNames(tx *sql.Tx) error {
	streams := make(map[int64]string)
	rows, err := tx.Query("SELECT streamid, name FROM el_streams")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		streams[id] = name
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Newest version of every node, also of nodes no longer in the current
	// stream version (their old events still need a name).
	names := make(map[int64]NodeKey)
	rows, err = tx.Query(`SELECT DISTINCT ON (n.nodeid) n.nodeid, n.name, COALESCE(sv.streamid, 0)
		FROM el_nodes n LEFT JOIN el_stream_versions sv ON sv.streamversionid = n.streamversionid
		ORDER BY n.nodeid, n.streamversionid DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, sid int64
		var name string
		if err := rows.Scan(&id, &name, &sid); err != nil {
			return err
		}
		names[id] = NodeKey{streamName(streams, sid), name}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	p.streams, p.nodeName = streams, names
	return nil
}

func streamName(streams map[int64]string, id int64) string {
	if id == 0 {
		return ""
	}
	if n, ok := streams[id]; ok {
		return n
	}
	return "id:" + strconv.FormatInt(id, 10)
}

func (p *poller) nodeKey(nodeID, streamID int64) NodeKey {
	if nodeID == 0 {
		return NodeKey{streamName(p.streams, streamID), ""}
	}
	if k, ok := p.nodeName[nodeID]; ok {
		return k
	}
	return NodeKey{streamName(p.streams, streamID), "id:" + strconv.FormatInt(nodeID, 10)}
}

func (p *poller) queryComponents(tx *sql.Tx) error {
	// Same rule as EventLink's el_v_host_nm_status: running = status R and
	// updated within the grace period (EventLink: 2m30s).
	rows, err := tx.Query(`SELECT name, COALESCE(host, ''), COALESCE(TRIM(status), ''),
		COALESCE(EXTRACT(EPOCH FROM updatetime::timestamptz), 0),
		COALESCE(updatetime > LOCALTIMESTAMP(0) - $1::interval, false)
		FROM el_component_status`, fmt.Sprintf("%d seconds", int(p.cfg.NodeManagerGrace.Seconds())))
	if err != nil {
		return err
	}
	defer rows.Close()
	var cs []Component
	for rows.Next() {
		var c Component
		var fresh bool
		if err := rows.Scan(&c.Name, &c.Host, &c.Status, &c.Updated, &fresh); err != nil {
			return err
		}
		c.Up = c.Status == "R" && fresh
		cs = append(cs, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.st.Components = cs
	p.mu.Unlock()
	return nil
}

// nodeTypeNames decodes el_node_log.type: one letter per position.
var nodeTypeNames = []string{"collector", "decoder", "businesslogic", "encoder", "distributor", "blt", "intermediate", "standalone"}

func decodeNodeType(t string) string {
	var parts []string
	for i, c := range t {
		if c != ' ' && i < len(nodeTypeNames) {
			parts = append(parts, nodeTypeNames[i])
		}
	}
	return strings.Join(parts, ",")
}

func (p *poller) queryNodes(tx *sql.Tx) error {
	// Nodes of the current version of every stream (as EventLink's
	// el_v_legacy_node_status, without its count over all events).
	rows, err := tx.Query(`SELECT str.name, n.name, COALESCE(n.host, sv.defaulthost, ''), COALESCE(mon.type, ''),
		COALESCE(mon.status, ''), COALESCE(mon.laststartsystem::text, '')
		FROM el_nodes n
		JOIN el_node_log mon ON mon.nodeid = n.nodeid
		JOIN el_stream_log s ON n.streamversionid = s.currentstreamversionid
		JOIN el_stream_versions sv ON sv.streamversionid = s.currentstreamversionid
		JOIN el_streams str ON str.streamid = sv.streamid
		WHERE COALESCE(str.deleted, 0) = 0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	nodes := make(map[NodeKey]NodeStatus)
	for rows.Next() {
		var stream, node, host, typ, status, start string
		if err := rows.Scan(&stream, &node, &host, &typ, &status, &start); err != nil {
			return err
		}
		ns := NodeStatus{Host: host, NodeType: decodeNodeType(typ), Status: strings.TrimSpace(status)}
		if t, ok := parseNominal(start); ok {
			ns.LastStart = p.systemEpoch(t)
		}
		k := NodeKey{stream, node}
		// Two nodes with the same name in a stream: keep the newest start.
		if old, dup := nodes[k]; !dup || ns.LastStart > old.LastStart {
			nodes[k] = ns
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.st.Nodes = nodes
	p.mu.Unlock()
	return nil
}

func (p *poller) events(k NodeKey) NodeEvents {
	e, ok := p.st.Events[k]
	if !ok {
		e = NodeEvents{Count: map[string]uint64{}, Last: map[string]float64{}}
	}
	return e
}

// seedEvents sets the newest event times of the last seed_days days (no
// counting) and the event watermark.
func (p *poller) seedEvents(tx *sql.Tx) error {
	var wm sql.NullInt64
	if err := tx.QueryRow("SELECT MAX(eventid) FROM el_events").Scan(&wm); err != nil {
		return err
	}
	since := time.Now().UTC().AddDate(0, 0, -p.cfg.SeedDays).Format(tsLayout)
	rows, err := tx.Query(`SELECT MAX(eventid) FROM el_events
		WHERE eventtimegmt > $1::timestamp GROUP BY COALESCE(nodeid, 0), COALESCE(streamid, 0), eventtype
		UNION
		SELECT MAX(eventid) FROM el_events
		WHERE eventtimegmt > $1::timestamp AND key = ANY($2) GROUP BY COALESCE(nodeid, 0), COALESCE(streamid, 0)`,
		since, pq.Array(p.cfg.FailureKeys))
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) > 0 {
		if _, err := p.readEvents(tx, `WHERE eventid = ANY($1) ORDER BY eventid`, false, pq.Array(ids)); err != nil {
			return err
		}
	}
	p.eventWM = wm.Int64
	return nil
}

func (p *poller) queryEvents(tx *sql.Tx) error {
	n, err := p.readEvents(tx, `WHERE eventid > $1 ORDER BY eventid LIMIT $2`, true, p.eventWM, p.cfg.MaxRowsPerPoll)
	if n >= p.cfg.MaxRowsPerPoll {
		p.mu.Lock()
		p.st.BudgetHits++
		p.mu.Unlock()
	}
	return err
}

// readEvents applies events in eventid order; count=false only updates the
// newest times (seed).
func (p *poller) readEvents(tx *sql.Tx, where string, count bool, args ...any) (int, error) {
	rows, err := tx.Query(`SELECT eventid, COALESCE(TRIM(eventtype), ''), COALESCE(key, ''),
		COALESCE(message, ''), COALESCE(nodeid, 0), COALESCE(streamid, 0), COALESCE(eventtimegmt::text, '')
		FROM el_events `+where, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	failure := make(map[string]bool, len(p.cfg.FailureKeys))
	for _, k := range p.cfg.FailureKeys {
		failure[k] = true
	}
	n := 0
	p.mu.Lock()
	defer p.mu.Unlock()
	for rows.Next() {
		var id, nodeID, streamID int64
		var typ, key, msg, gmt string
		if err := rows.Scan(&id, &typ, &key, &msg, &nodeID, &streamID, &gmt); err != nil {
			return n, err
		}
		n++
		if count && id > p.eventWM {
			p.eventWM = id
		}
		k := p.nodeKey(nodeID, streamID)
		e := p.events(k)
		if count {
			e.Count[typ]++
		}
		if t, ok := parseNominal(gmt); ok {
			ts := float64(t.Unix())
			if ts >= e.Last[typ] {
				e.Last[typ] = ts
				if typ == "E" {
					e.ErrKey, e.ErrMsg = key, truncate(msg, p.cfg.MessageMaxLen)
				}
			}
			if failure[key] && ts > e.LastFailure {
				e.LastFailure = ts
			}
		}
		p.st.Events[k] = e
	}
	return n, rows.Err()
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// seedReporting sets the newest collection / distribution per stream of the
// last seed_days days; counting starts now.
func (p *poller) seedReporting(tx *sql.Tx) error {
	now := p.systemNow()
	for _, f := range p.flows {
		since := now.AddDate(0, 0, -p.cfg.SeedDays)
		if err := p.queryNewest(tx, f, since); err != nil {
			return err
		}
		if f.newest.IsZero() {
			f.newest = since // never scan the whole table for "newest"
		}
		f.cut = now
	}
	return nil
}

// queryNewest updates the newest row time per stream above since.
func (p *poller) queryNewest(tx *sql.Tx, f *flow, since time.Time) error {
	rows, err := tx.Query(fmt.Sprintf(`SELECT COALESCE(streamid, 0), MAX(%[1]s)::text FROM %[2]s
		WHERE %[1]s > $1::timestamp GROUP BY 1`, f.tsCol, f.table), since.Format(tsLayout))
	if err != nil {
		return err
	}
	defer rows.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for rows.Next() {
		var sid int64
		var newest string
		if err := rows.Scan(&sid, &newest); err != nil {
			return err
		}
		if t, ok := parseNominal(newest); ok {
			p.setLast(streamName(p.streams, sid), f.idx, p.systemEpoch(t))
			if t.After(f.newest) {
				f.newest = t
			}
		}
	}
	return rows.Err()
}

// setLast is called with p.mu held.
func (p *poller) setLast(stream string, idx int, ts float64) {
	s := p.st.Streams[stream]
	if idx == 0 && ts > s.LastCollected {
		s.LastCollected = ts
	}
	if idx == 1 && ts > s.LastDistributed {
		s.LastDistributed = ts
	}
	p.st.Streams[stream] = s
}

func (p *poller) queryFlow(tx *sql.Tx, f *flow) error {
	if err := p.queryNewest(tx, f, f.newest); err != nil {
		return err
	}
	from, to, ok := f.nextSlice(p.systemNow(), p.cfg.ReportingLag)
	if !ok {
		return nil
	}
	rows, err := tx.Query(fmt.Sprintf(`SELECT COALESCE(streamid, 0), COUNT(*),
		COALESCE(SUM(%[2]s), 0), COALESCE(SUM(%[3]s), 0)
		FROM %[4]s WHERE %[1]s > $1::timestamp AND %[1]s <= $2::timestamp GROUP BY 1`,
		f.tsCol, f.bytesCol, f.recCol, f.table), from.Format(tsLayout), to.Format(tsLayout))
	if err != nil {
		return err
	}
	defer rows.Close()
	type sums struct{ files, bytes, records int64 }
	got := make(map[string]sums)
	for rows.Next() {
		var sid int64
		var s sums
		if err := rows.Scan(&sid, &s.files, &s.bytes, &s.records); err != nil {
			return err
		}
		k := streamName(p.streams, sid)
		g := got[k]
		got[k] = sums{g.files + s.files, g.bytes + s.bytes, g.records + s.records}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	for stream, g := range got {
		s := p.st.Streams[stream]
		s.Files[f.idx] += uint64(max(g.files, 0))
		s.Bytes[f.idx] += uint64(max(g.bytes, 0))
		s.Records[f.idx] += uint64(max(g.records, 0))
		p.st.Streams[stream] = s
	}
	p.mu.Unlock()
	f.cut = to
	return nil
}
