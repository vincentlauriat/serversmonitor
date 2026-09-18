// Package proto defines the JSON messages exchanged between smagent and smhub.
package proto

import "time"

// Version is the protocol version stamped on every message.
const Version = 1

const (
	TypeHello       = "hello"
	TypeWelcome     = "welcome"
	TypeSample      = "sample"
	TypeReconfigure = "reconfigure"
)

// Hello is the agent's first message; the hub answers with Welcome.
type Hello struct {
	V            int      `json:"v"`
	Type         string   `json:"type"`
	AgentVersion string   `json:"agent_version"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Hostname     string   `json:"hostname"`
	Cores        int      `json:"cores"`
	MemTotal     int64    `json:"mem_total"`
	Capabilities []string `json:"capabilities"`
}

type Welcome struct {
	V            int      `json:"v"`
	Type         string   `json:"type"`
	IntervalSec  int      `json:"interval_sec"`
	IgnoreMounts []string `json:"ignore_mounts"`
}

type Reconfigure struct {
	V           int    `json:"v"`
	Type        string `json:"type"`
	IntervalSec int    `json:"interval_sec"`
}

type Disk struct {
	Mount    string  `json:"mount"`
	Used     int64   `json:"used"`
	Total    int64   `json:"total"`
	ReadBps  float64 `json:"read_bps"`
	WriteBps float64 `json:"write_bps"`
}

type Net struct {
	SentBps   float64 `json:"sent_bps"`
	RecvBps   float64 `json:"recv_bps"`
	SentTotal int64   `json:"sent_total"`
	RecvTotal int64   `json:"recv_total"`
}

type Temp struct {
	Sensor  string  `json:"sensor"`
	Celsius float64 `json:"celsius"`
}

type Container struct {
	Name       string  `json:"name"`
	Image      string  `json:"image"`
	Status     string  `json:"status"`
	CPU        float64 `json:"cpu"`
	MemUsed    int64   `json:"mem_used"`
	NetSentBps float64 `json:"net_sent_bps"`
	NetRecvBps float64 `json:"net_recv_bps"`
}

// Sample is one snapshot of a host. Every optional field is a pointer: a nil
// pointer means the agent did not collect it, which is never the same as zero.
type Sample struct {
	V               int         `json:"v"`
	Type            string      `json:"type"`
	At              time.Time   `json:"at"`
	CPU             *float64    `json:"cpu,omitempty"`
	MemUsed         *int64      `json:"mem_used,omitempty"`
	MemTotal        *int64      `json:"mem_total,omitempty"`
	SwapUsed        *int64      `json:"swap_used,omitempty"`
	SwapTotal       *int64      `json:"swap_total,omitempty"`
	Load1           *float64    `json:"load1,omitempty"`
	Load5           *float64    `json:"load5,omitempty"`
	Load15          *float64    `json:"load15,omitempty"`
	Uptime          *int64      `json:"uptime,omitempty"`
	Disks           []Disk      `json:"disks,omitempty"`
	Net             *Net        `json:"net,omitempty"`
	Temps           []Temp      `json:"temps,omitempty"`
	DockerAvailable bool        `json:"docker_available"`
	Containers      []Container `json:"containers,omitempty"`
}
