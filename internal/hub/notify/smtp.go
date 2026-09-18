package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// SMTP sends one mail per alert transition. net/smtp is frozen but complete,
// and one dependency avoided is one dependency to keep patched.
type SMTP struct{ cfg SMTPConfig }

func NewSMTP(cfg SMTPConfig) *SMTP { return &SMTP{cfg: cfg} }

func (s *SMTP) Name() string { return "smtp" }

// sanitizeHeader keeps a value on one line. Host names come from the browser,
// and a CRLF in one would otherwise append headers of the sender's choosing.
func sanitizeHeader(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.TrimSpace(v)
}

func newMessageID(host string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b) + "@" + host
}

// BuildMail renders one RFC 5322 message. Kept separate from Send so the bytes
// can be asserted without a server.
func BuildMail(cfg SMTPConfig, m Message, now time.Time, msgID string) []byte {
	var b strings.Builder
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, sanitizeHeader(v)) }
	h("From", cfg.From)
	h("To", strings.Join(cfg.To, ", "))
	h("Subject", m.Subject())
	h("Message-ID", "<"+msgID+">")
	h("Date", now.UTC().Format(time.RFC1123Z))
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Auto-Submitted", "auto-generated") // stops well-behaved autoresponders
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(m.Body(), "\n", "\r\n"))
	return []byte(b.String())
}

func (s *SMTP) Send(ctx context.Context, m Message) error {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	d := net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if s.cfg.TLSMode == "tls" {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: s.cfg.Host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		// Nothing answered. That is the network, not the message.
		return MarkRetryable(fmt.Errorf("smtp dial %s: %w", addr, err))
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return classifySMTP(err)
	}
	defer c.Close()
	if s.cfg.TLSMode == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
			return classifySMTP(err)
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return classifySMTP(err)
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return classifySMTP(err)
	}
	for _, to := range s.cfg.To {
		if err := c.Rcpt(to); err != nil {
			return classifySMTP(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return classifySMTP(err)
	}
	if _, err := w.Write(BuildMail(s.cfg, m, time.Now(), newMessageID(s.cfg.Host))); err != nil {
		return classifySMTP(err)
	}
	if err := w.Close(); err != nil { // the server's verdict on the payload arrives here
		return classifySMTP(err)
	}
	return c.Quit()
}

// classifySMTP maps a reply code onto the retry policy: 4xx is "come back
// later", 5xx is "this will never work". An error with no code at all is a
// transport problem, which is always worth another try.
func classifySMTP(err error) error {
	if code, ok := smtpCode(err); ok {
		if code >= 400 && code < 500 {
			return MarkRetryable(err)
		}
		return err
	}
	return MarkRetryable(err)
}

// smtpCode extracts the reply code from a net/textproto error, which is what
// net/smtp returns for every server refusal.
func smtpCode(err error) (int, bool) {
	var e *textproto.Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return 0, false
}
