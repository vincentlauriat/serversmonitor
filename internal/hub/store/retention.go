package store

import (
	"fmt"
	"time"
)

// Retention says how long each resolution is kept. samples_1d is kept forever.
type Retention struct {
	Raw, TenMin, Hour time.Duration
}

func (s *Store) RetentionFromSettings() Retention {
	return Retention{
		Raw:    time.Duration(s.SettingInt("retention_raw_hours", 24)) * time.Hour,
		TenMin: time.Duration(s.SettingInt("retention_10m_days", 30)) * 24 * time.Hour,
		Hour:   time.Duration(s.SettingInt("retention_1h_days", 365)) * 24 * time.Hour,
	}
}

type aggStep struct {
	src, dst string
	size     time.Duration
	fromRaw  bool
}

var aggSteps = []aggStep{
	{"samples", "samples_10m", 10 * time.Minute, true},
	{"samples_10m", "samples_1h", time.Hour, false},
	{"samples_1h", "samples_1d", 24 * time.Hour, false},
}

// Aggregate rolls raw samples up into 10m, 1h and 1d windows. Only complete
// windows are written; rewriting a window gives the same result, so it is safe
// to call at any time, including twice.
func (s *Store) Aggregate(now time.Time) error {
	for _, st := range aggSteps {
		if err := s.aggregateStep(st, now); err != nil {
			return fmt.Errorf("aggregate %s: %w", st.dst, err)
		}
	}
	return nil
}

func (s *Store) aggregateStep(st aggStep, now time.Time) error {
	// Catch-up start: the newest window already in dst (recomputed, harmless), else the oldest source row.
	var startStr *string
	if err := s.db.QueryRow(`SELECT max(at) FROM ` + st.dst).Scan(&startStr); err != nil {
		return err
	}
	if startStr == nil {
		if err := s.db.QueryRow(`SELECT min(at) FROM ` + st.src).Scan(&startStr); err != nil {
			return err
		}
		if startStr == nil {
			return nil // nothing to aggregate
		}
	}
	start, err := parseTime(*startStr)
	if err != nil {
		return err
	}
	start = start.Truncate(st.size)
	for w := start; !w.Add(st.size).After(now); w = w.Add(st.size) {
		if err := s.aggregateWindow(st, w); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) aggregateWindow(st aggStep, w time.Time) error {
	from, to := fmtTime(w), fmtTime(w.Add(st.size))
	cols := `(host_id, at, cpu_avg, cpu_min, cpu_max, mem_used_avg, mem_used_min, mem_used_max, mem_total, swap_used_avg,
		load1_avg, load5_avg, load15_avg, net_sent_bps_avg, net_sent_bps_max, net_recv_bps_avg, net_recv_bps_max, disks, temps)`
	var sel string
	if st.fromRaw {
		sel = `SELECT host_id, ?, avg(cpu), min(cpu), max(cpu), avg(mem_used), min(mem_used), max(mem_used), max(mem_total), avg(swap_used),
			avg(load1), avg(load5), avg(load15), avg(net_sent_bps), max(net_sent_bps), avg(net_recv_bps), max(net_recv_bps),`
	} else {
		sel = `SELECT host_id, ?, avg(cpu_avg), min(cpu_min), max(cpu_max), avg(mem_used_avg), min(mem_used_min), max(mem_used_max), max(mem_total), avg(swap_used_avg),
			avg(load1_avg), avg(load5_avg), avg(load15_avg), avg(net_sent_bps_avg), max(net_sent_bps_max), avg(net_recv_bps_avg), max(net_recv_bps_max),`
	}
	// Disks and temps are lists: carry the most recent non-null JSON of the window.
	sel += `
		(SELECT disks FROM ` + st.src + ` s2 WHERE s2.host_id = s.host_id AND s2.at >= ? AND s2.at < ? AND s2.disks IS NOT NULL ORDER BY s2.at DESC LIMIT 1),
		(SELECT temps FROM ` + st.src + ` s3 WHERE s3.host_id = s.host_id AND s3.at >= ? AND s3.at < ? AND s3.temps IS NOT NULL ORDER BY s3.at DESC LIMIT 1)
		FROM ` + st.src + ` s WHERE s.at >= ? AND s.at < ? GROUP BY s.host_id`
	_, err := s.db.Exec(`INSERT OR REPLACE INTO `+st.dst+cols+sel, from, from, to, from, to, from, to)
	return err
}

// Purge deletes rows older than the retention of each table. samples_1d is kept forever.
func (s *Store) Purge(now time.Time, r Retention) error {
	steps := []struct {
		table string
		keep  time.Duration
	}{
		{"samples", r.Raw}, {"container_samples", r.Raw}, {"samples_10m", r.TenMin}, {"samples_1h", r.Hour},
	}
	for _, st := range steps {
		if _, err := s.db.Exec(`DELETE FROM `+st.table+` WHERE at < ?`, fmtTime(now.Add(-st.keep))); err != nil {
			return fmt.Errorf("purge %s: %w", st.table, err)
		}
	}
	return nil
}
