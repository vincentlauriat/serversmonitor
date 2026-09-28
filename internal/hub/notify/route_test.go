package notify

import (
	"context"
	"reflect"
	"testing"
)

type named string

func (n named) Name() string                        { return string(n) }
func (n named) Send(context.Context, Message) error { return nil }

func routed(chans []Channel) []string {
	out := []string{}
	for _, c := range chans {
		out = append(out, c.Name())
	}
	return out
}

func TestRoute(t *testing.T) {
	enabled := []Channel{named("smtp"), named("webhook")} // teams is off
	for _, tc := range []struct {
		name        string
		route, also []string
		want        []string
	}{
		{"nil is every enabled channel", nil, nil, []string{"smtp", "webhook"}},
		{"empty is none", []string{}, nil, []string{}},
		{"only what the route names", []string{"webhook"}, nil, []string{"webhook"}},
		{"a disabled channel is skipped", []string{"teams"}, nil, []string{}},
		{"also adds where the fired went", []string{"webhook"}, []string{"smtp"}, []string{"smtp", "webhook"}},
		{"also on an empty route", []string{}, []string{"smtp", "teams"}, []string{"smtp"}},
	} {
		if got := routed(Route(enabled, tc.route, tc.also)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeRoute(t *testing.T) {
	if r, err := NormalizeRoute(nil); r != nil || err != nil {
		t.Fatalf("nil must stay nil: %#v %v", r, err)
	}
	if r, err := NormalizeRoute([]string{}); r == nil || len(r) != 0 || err != nil {
		t.Fatalf("empty must stay empty, not nil: %#v %v", r, err)
	}
	r, err := NormalizeRoute([]string{"teams", "smtp", "teams"})
	if err != nil || !reflect.DeepEqual(r, []string{"smtp", "teams"}) {
		t.Fatalf("got %v %v, want [smtp teams]", r, err)
	}
	if _, err := NormalizeRoute([]string{"pager"}); err == nil {
		t.Fatal("an unknown channel must be refused")
	}
}
