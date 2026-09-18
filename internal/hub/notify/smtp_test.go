package notify

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func testSMTPConfig() SMTPConfig {
	return SMTPConfig{Enabled: true, Host: "localhost", Port: 1025, From: "hub@example.test",
		To: []string{"vincent@example.test", "ops@example.test"}, TLSMode: "none"}
}

func TestBuildMailHeaders(t *testing.T) {
	raw := string(BuildMail(testSMTPConfig(), fired(), at, "abc@hub"))
	head, body, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		t.Fatalf("mail has no header/body separator:\n%q", raw)
	}
	for _, want := range []string{
		"From: hub@example.test",
		"To: vincent@example.test, ops@example.test",
		"Subject: [ServersMonitor] mac-vincent memory 92.4% (fired)",
		"Message-ID: <abc@hub>",
		"Date: Fri, 18 Sep 2026 03:14:00 +0000",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("header missing %q:\n%s", want, head)
		}
	}
	if !strings.Contains(body, "92.4%") {
		t.Errorf("body lost the value:\n%s", body)
	}
	if strings.Contains(strings.ReplaceAll(head, "\r\n", ""), "\n") {
		t.Error("headers must use CRLF line endings")
	}
}

func TestBuildMailRefusesHeaderInjection(t *testing.T) {
	// A host name is user input. If it reached the Subject line unescaped, a
	// name containing CRLF would let anyone add headers to the hub's own mail.
	//
	// The property is that no new header *line* appears. "Bcc:" surviving as
	// text inside the Subject value is harmless, and asserting its absence
	// anywhere in the block would test the wrong thing.
	m := fired()
	m.HostName = "pi\r\nBcc: attacker@example.test"
	raw := string(BuildMail(testSMTPConfig(), m, at, "x@hub"))
	head, _, _ := strings.Cut(raw, "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	if len(lines) != 8 {
		t.Fatalf("expected the 8 headers BuildMail writes, got %d:\n%s", len(lines), head)
	}
	for _, l := range lines {
		if strings.HasPrefix(strings.ToLower(l), "bcc:") {
			t.Fatalf("header injection got through:\n%s", head)
		}
	}
	if !strings.HasPrefix(lines[2], "Subject: ") || strings.Contains(lines[2], "\n") {
		t.Fatalf("the payload must stay inside the subject line: %q", lines[2])
	}
}

// fakeSMTP speaks just enough ESMTP to accept one message and record it.
type fakeSMTP struct {
	ln       net.Listener
	mu       sync.Mutex
	received []string
	reject   string // when set, the reply to the DATA payload
}

func startFakeSMTP(t *testing.T, reject string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, reject: reject}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) addr() (string, int) {
	a := f.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (f *fakeSMTP) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeSMTP) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	w := func(s string) { c.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	var body strings.Builder
	inData := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				if f.reject != "" {
					w(f.reject)
					continue
				}
				f.mu.Lock()
				f.received = append(f.received, body.String())
				f.mu.Unlock()
				body.Reset()
				w("250 ok")
				continue
			}
			body.WriteString(line + "\n")
			continue
		}
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			w("250-fake")
			w("250 AUTH PLAIN LOGIN")
		case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
			w("250 ok")
		case strings.HasPrefix(line, "AUTH"):
			w("235 authenticated")
		case line == "DATA":
			inData = true
			w("354 go ahead")
		case line == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func (f *fakeSMTP) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func TestSMTPSendReachesTheServer(t *testing.T) {
	f := startFakeSMTP(t, "")
	host, port := f.addr()
	cfg := testSMTPConfig()
	cfg.Host, cfg.Port = host, port
	ch := NewSMTP(cfg)
	if ch.Name() != "smtp" {
		t.Fatalf("name = %q", ch.Name())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ch.Send(ctx, fired()); err != nil {
		t.Fatalf("send: %v", err)
	}
	msgs := f.messages()
	if len(msgs) != 1 {
		t.Fatalf("server received %d messages", len(msgs))
	}
	if !strings.Contains(msgs[0], "Subject: [ServersMonitor] mac-vincent memory 92.4% (fired)") {
		t.Fatalf("wrong message:\n%s", msgs[0])
	}
}

func TestSMTPTemporaryFailureIsRetryable(t *testing.T) {
	f := startFakeSMTP(t, "451 mailbox busy")
	host, port := f.addr()
	cfg := testSMTPConfig()
	cfg.Host, cfg.Port = host, port
	err := NewSMTP(cfg).Send(context.Background(), fired())
	if err == nil {
		t.Fatal("a 451 must be an error")
	}
	if !Retryable(err) {
		t.Fatalf("a 4xx from SMTP is temporary and must be retryable: %v", err)
	}
}

func TestSMTPPermanentFailureIsNotRetryable(t *testing.T) {
	f := startFakeSMTP(t, "550 no such user")
	host, port := f.addr()
	cfg := testSMTPConfig()
	cfg.Host, cfg.Port = host, port
	err := NewSMTP(cfg).Send(context.Background(), fired())
	if err == nil {
		t.Fatal("a 550 must be an error")
	}
	if Retryable(err) {
		t.Fatalf("a 550 will fail identically forever: %v", err)
	}
}

func TestSMTPUnreachableHostIsRetryable(t *testing.T) {
	cfg := testSMTPConfig()
	cfg.Host, cfg.Port = "127.0.0.1", 1 // nothing listens on port 1
	err := NewSMTP(cfg).Send(context.Background(), fired())
	if !Retryable(err) {
		t.Fatalf("a refused connection is worth retrying: %v", err)
	}
}
