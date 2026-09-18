package proto

import (
	"testing"
	"time"
)

func TestDecodeSampleIgnoresUnknownFields(t *testing.T) {
	raw := []byte(`{"v":1,"type":"sample","at":"2026-09-17T10:00:00Z","cpu":12.5,"future_metric":42}`)
	msg, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	s, ok := msg.(*Sample)
	if !ok {
		t.Fatalf("want *Sample, got %T", msg)
	}
	if s.CPU == nil || *s.CPU != 12.5 {
		t.Fatalf("cpu = %v", s.CPU)
	}
	if !s.At.Equal(time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("at = %v", s.At)
	}
}

func TestDecodeSampleMissingFieldIsNotCollected(t *testing.T) {
	raw := []byte(`{"v":1,"type":"sample","at":"2026-09-17T10:00:00Z"}`)
	msg, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := msg.(*Sample)
	if s.CPU != nil || s.MemUsed != nil || s.Net != nil || s.Disks != nil {
		t.Fatalf("missing fields must stay nil: %+v", s)
	}
}

func TestDecodeUnknownType(t *testing.T) {
	if _, err := Decode([]byte(`{"v":1,"type":"teleport"}`)); err != ErrUnknownType {
		t.Fatalf("want ErrUnknownType, got %v", err)
	}
}

func TestDecodeHello(t *testing.T) {
	raw := []byte(`{"v":1,"type":"hello","agent_version":"0.1.0","os":"linux","arch":"arm64","hostname":"pi","cores":4,"mem_total":8000000000,"capabilities":["docker"]}`)
	msg, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	h := msg.(*Hello)
	if h.Hostname != "pi" || h.Cores != 4 || len(h.Capabilities) != 1 {
		t.Fatalf("hello = %+v", h)
	}
}

func TestEncodeSetsVersion(t *testing.T) {
	data, err := Encode(&Welcome{Type: TypeWelcome, IntervalSec: 10})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if w := msg.(*Welcome); w.V != Version || w.IntervalSec != 10 {
		t.Fatalf("welcome = %+v", w)
	}
}

func TestEncodeRequiresType(t *testing.T) {
	if _, err := Encode(&Welcome{}); err == nil {
		t.Fatal("want error for empty type")
	}
}
