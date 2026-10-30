// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package notification

import (
	"bufio"
	"context"
	"mime"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/domain"
)

// fakeSMTP serves one session. mailReply is the answer to MAIL FROM; an empty
// value closes the connection instead, as a crashed or restarted relay does.
func fakeSMTP(t *testing.T, mailReply string) (int, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader, writer := bufio.NewReader(conn), bufio.NewWriter(conn)
		reply := func(line string) { _, _ = writer.WriteString(line + "\r\n"); _ = writer.Flush() }
		reply("220 localhost ESMTP")
		var message strings.Builder
		inData := false
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					received <- message.String()
					reply("250 queued")
					continue
				}
				message.WriteString(line)
				continue
			}
			command := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(command, "EHLO"):
				reply("250-localhost\r\n250 8BITMIME")
			case strings.HasPrefix(command, "MAIL FROM"):
				if mailReply == "" {
					return
				}
				reply(mailReply)
			case command == "DATA":
				inData = true
				reply("354 end with dot")
			case command == "QUIT":
				reply("221 bye")
				return
			default:
				reply("250 ok")
			}
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, received
}

func smtpSender(t *testing.T, port int) *Sender {
	t.Helper()
	sender, err := New(Config{Settings: domain.NotificationSettings{SMTPEnabled: true, SMTPHost: "127.0.0.1", SMTPPort: port, SMTPFrom: "witshield@example.test", SMTPTo: []string{"admin@example.test"}}})
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

func TestSMTPConnectionLostBeforeAcceptanceIsRetryable(t *testing.T) {
	port, _ := fakeSMTP(t, "")
	err := smtpSender(t, port).SendChannel(context.Background(), domain.NotificationSMTP, domain.NotificationEvent{ID: "ntf_drop", Title: "drop"})
	// Nothing was accepted, so another attempt cannot duplicate the message.
	// The durable outbox turns a non-retryable error into a terminal failure
	// after this single attempt (httpapi/notification_worker.go).
	if err == nil || !IsRetryable(err) {
		t.Fatalf("lost SMTP session err=%v retryable=%v; want a retryable failure", err, IsRetryable(err))
	}
}

func TestSMTPPermanentRejectionStaysTerminal(t *testing.T) {
	port, _ := fakeSMTP(t, "550 5.7.1 sender rejected")
	err := smtpSender(t, port).SendChannel(context.Background(), domain.NotificationSMTP, domain.NotificationEvent{ID: "ntf_rejected", Title: "rejected"})
	if err == nil || IsRetryable(err) {
		t.Fatalf("permanent SMTP rejection err=%v retryable=%v", err, IsRetryable(err))
	}
}

func TestSMTPNonASCIISubjectIsEncodedWord(t *testing.T) {
	port, received := fakeSMTP(t, "250 ok")
	title := "已触发进程隔离"
	if err := smtpSender(t, port).SendChannel(context.Background(), domain.NotificationSMTP, domain.NotificationEvent{ID: "ntf_cn", Title: title, Message: "正文"}); err != nil {
		t.Fatal(err)
	}
	var message string
	select {
	case message = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("SMTP sink did not receive a message")
	}
	var subject string
	for _, line := range strings.Split(message, "\r\n") {
		if strings.HasPrefix(line, "Subject: ") {
			subject = strings.TrimPrefix(line, "Subject: ")
			break
		}
	}
	for i := 0; i < len(subject); i++ {
		if subject[i] >= 0x80 {
			t.Fatalf("Subject header carries raw 8-bit bytes (RFC 5322 headers are US-ASCII): %q", subject)
		}
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(subject)
	if err != nil || decoded != "[WitShield] "+title {
		t.Fatalf("subject %q decodes to %q (err=%v)", subject, decoded, err)
	}
}
