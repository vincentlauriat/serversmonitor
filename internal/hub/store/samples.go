package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

var ErrStale = errors.New("store: sample older than last received")

// SampleRow is one raw sample as stored. Pointer fields are nil when the agent
// did not collect the metric; they are never defaulted to zero.
type SampleRow struct {
	HostID     int64
	At         time.Time
	CPU        *float64
	MemUsed    *int64
	MemTotal   *int64
	SwapUsed   *int64
	SwapTotal  *int64
	Load1      *float64
	Load5      *float64
	Load15     *float64
	Uptime     *int64
	NetSentBps *float64
	NetRecvBps *float64
	Disks      []proto.Disk
	Temps      []proto.Temp
}

// Point is one chart point: raw values for short periods, averages for aggregates.
type Point struct {
	At         time.Time
	CPU        *float64
	CPUMax     *float64
	MemUsed    *float64
	MemTotal   *float64
	SwapUsed   *float64
	Load1      *float64
	Load5      *float64
	Load15     *float64
	NetSentBps *float64
	NetRecvBps *float64
	Disks      []proto.Disk
	Temps      []proto.Temp
}

type ContainerRow struct {
	HostID     int64
	Name       string
	Image      string
	Status     string
	CPU        *float64
	MemUsed    *int64
	NetSentBps *float64
	NetRecvBps *float64
	UpdatedAt  time.Time
}

// jsonOrNil marshals v, or returns nil so the column stays NULL when the
// section was not collected at all.
func jsonOrNil(v any, notCollected bool) any {
	if notCollected {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

func (s *Store) LastSampleAt(hostID int64) (time.Time, bool, error) {
	var at sql.NullString
	if err := s.db.QueryRow(`SELECT max(at) FROM samples WHERE host_id = ?`, hostID).Scan(&at); err != nil {
		return time.Time{}, false, err
	}
	if !at.Valid {
		return time.Time{}, false, nil
	}
	t, err := parseTime(at.String)
	return t, err == nil, err
}

// InsertSample stores one sample and the container state it carries.
// A sample at or before the last stored one is refused with ErrStale.
func (s *Store) InsertSample(hostID int64, sm *proto.Sample) error {
	last, ok, err := s.LastSampleAt(hostID)
	if err != nil {
		return err
	}
	if ok && !sm.At.After(last) {
		return ErrStale
	}
	var netSent, netRecv *float64
	if sm.Net != nil {
		netSent, netRecv = &sm.Net.SentBps, &sm.Net.RecvBps
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO samples(host_id, at, cpu, mem_used, mem_total, swap_used, swap_total, load1, load5, load15, uptime, net_sent_bps, net_recv_bps, disks, temps)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		hostID, fmtTime(sm.At), sm.CPU, sm.MemUsed, sm.MemTotal, sm.SwapUsed, sm.SwapTotal, sm.Load1, sm.Load5, sm.Load15, sm.Uptime,
		netSent, netRecv, jsonOrNil(sm.Disks, sm.Disks == nil), jsonOrNil(sm.Temps, sm.Temps == nil))
	if err != nil {
		return err
	}
	if sm.DockerAvailable {
		at := fmtTime(sm.At)
		for _, c := range sm.Containers {
			if _, err := tx.Exec(`INSERT INTO containers(host_id, name, image, status, cpu, mem_used, net_sent_bps, net_recv_bps, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?)
				ON CONFLICT(host_id, name) DO UPDATE SET image=excluded.image, status=excluded.status, cpu=excluded.cpu,
				mem_used=excluded.mem_used, net_sent_bps=excluded.net_sent_bps, net_recv_bps=excluded.net_recv_bps, updated_at=excluded.updated_at`,
				hostID, c.Name, c.Image, c.Status, c.CPU, c.MemUsed, c.NetSentBps, c.NetRecvBps, at); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO container_samples(host_id, name, at, cpu, mem_used, net_sent_bps, net_recv_bps) VALUES (?,?,?,?,?,?,?)`,
				hostID, c.Name, at, c.CPU, c.MemUsed, c.NetSentBps, c.NetRecvBps); err != nil {
				return err
			}
		}
		// Containers that disappeared from a *collected* list are gone. A sample
		// without Docker leaves the table untouched: unavailable is not "none".
		args := make([]any, 0, len(sm.Containers)+1)
		args = append(args, hostID)
		q := `DELETE FROM containers WHERE host_id = ?`
		if len(sm.Containers) > 0 {
			q += ` AND name NOT IN (?` + strings.Repeat(",?", len(sm.Containers)-1) + `)`
			for _, c := range sm.Containers {
				args = append(args, c.Name)
			}
		}
		if _, err := tx.Exec(q, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LatestSample(hostID int64) (SampleRow, error) {
	row := s.db.QueryRow(`SELECT host_id, at, cpu, mem_used, mem_total, swap_used, swap_total, load1, load5, load15, uptime, net_sent_bps, net_recv_bps, disks, temps
		FROM samples WHERE host_id = ? ORDER BY at DESC LIMIT 1`, hostID)
	var r SampleRow
	var at string
	var disks, temps sql.NullString
	err := row.Scan(&r.HostID, &at, &r.CPU, &r.MemUsed, &r.MemTotal, &r.SwapUsed, &r.SwapTotal, &r.Load1, &r.Load5, &r.Load15, &r.Uptime, &r.NetSentBps, &r.NetRecvBps, &disks, &temps)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if r.At, err = parseTime(at); err != nil {
		return r, err
	}
	if disks.Valid {
		json.Unmarshal([]byte(disks.String), &r.Disks)
	}
	if temps.Valid {
		json.Unmarshal([]byte(temps.String), &r.Temps)
	}
	return r, nil
}

// TableForPeriod maps a chart period to the table holding the right resolution.
func TableForPeriod(period string) (string, time.Duration, error) {
	switch period {
	case "1h":
		return "samples", time.Hour, nil
	case "24h":
		return "samples", 24 * time.Hour, nil
	case "7d":
		return "samples_10m", 7 * 24 * time.Hour, nil
	case "30d":
		return "samples_1h", 30 * 24 * time.Hour, nil
	case "1y":
		return "samples_1d", 365 * 24 * time.Hour, nil
	}
	return "", 0, fmt.Errorf("store: unknown period %q", period)
}

// Series returns chart points for a host over the period ending at now.
func (s *Store) Series(hostID int64, period string, now time.Time) ([]Point, error) {
	table, span, err := TableForPeriod(period)
	if err != nil {
		return nil, err
	}
	from := fmtTime(now.Add(-span))
	q := `SELECT at, cpu, NULL, mem_used, mem_total, swap_used, load1, load5, load15, net_sent_bps, net_recv_bps, disks, temps FROM samples WHERE host_id = ? AND at >= ? ORDER BY at`
	if table != "samples" {
		q = `SELECT at, cpu_avg, cpu_max, mem_used_avg, mem_total, swap_used_avg, load1_avg, load5_avg, load15_avg, net_sent_bps_avg, net_recv_bps_avg, disks, temps FROM ` + table + ` WHERE host_id = ? AND at >= ? ORDER BY at`
	}
	rows, err := s.db.Query(q, hostID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var p Point
		var at string
		var disks, temps sql.NullString
		if err := rows.Scan(&at, &p.CPU, &p.CPUMax, &p.MemUsed, &p.MemTotal, &p.SwapUsed, &p.Load1, &p.Load5, &p.Load15, &p.NetSentBps, &p.NetRecvBps, &disks, &temps); err != nil {
			return nil, err
		}
		if p.At, err = parseTime(at); err != nil {
			return nil, err
		}
		if disks.Valid {
			json.Unmarshal([]byte(disks.String), &p.Disks)
		}
		if temps.Valid {
			json.Unmarshal([]byte(temps.String), &p.Temps)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Containers(hostID int64) ([]ContainerRow, error) {
	rows, err := s.db.Query(`SELECT host_id, name, image, status, cpu, mem_used, net_sent_bps, net_recv_bps, updated_at FROM containers WHERE host_id = ? ORDER BY name`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContainerRow
	for rows.Next() {
		var c ContainerRow
		var at string
		if err := rows.Scan(&c.HostID, &c.Name, &c.Image, &c.Status, &c.CPU, &c.MemUsed, &c.NetSentBps, &c.NetRecvBps, &at); err != nil {
			return nil, err
		}
		if c.UpdatedAt, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
