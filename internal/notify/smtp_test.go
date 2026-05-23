package notify

import (
	"bufio"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTPServer is a tiny line-protocol SMTP server used to verify
// STARTTLS / AUTH downgrade behavior of sendOverConn. It only honors a
// scripted set of responses — it does NOT actually accept mail.
type fakeSMTPServer struct {
	addr           string
	advertiseTLS   bool
	advertiseAuth  bool
	wg             sync.WaitGroup
	close          func()
	mu             sync.Mutex
	authReceived   bool
	starttlsSeen   bool
	commandsSeen   []string
}

func startFakeSMTP(t *testing.T, advertiseTLS, advertiseAuth bool) *fakeSMTPServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTPServer{
		addr:          l.Addr().String(),
		advertiseTLS:  advertiseTLS,
		advertiseAuth: advertiseAuth,
		close:         func() { _ = l.Close() },
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go f.handleConn(conn)
		}
	}()
	return f
}

func (f *fakeSMTPServer) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	send := func(s string) { _, _ = w.WriteString(s); _ = w.Flush() }
	send("220 fake.smtp ready\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.mu.Lock()
		f.commandsSeen = append(f.commandsSeen, line)
		f.mu.Unlock()
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			send("250-fake.smtp\r\n")
			if f.advertiseAuth {
				send("250-AUTH PLAIN LOGIN\r\n")
			}
			if f.advertiseTLS {
				send("250-STARTTLS\r\n")
			}
			send("250 HELP\r\n")
		case strings.HasPrefix(up, "HELO"):
			send("250 fake.smtp\r\n")
		case strings.HasPrefix(up, "STARTTLS"):
			f.mu.Lock()
			f.starttlsSeen = true
			f.mu.Unlock()
			send("220 ready to start tls\r\n")
			// Don't actually negotiate TLS; the client will error and
			// the test only asserts STARTTLS handshake was attempted.
			return
		case strings.HasPrefix(up, "AUTH"):
			f.mu.Lock()
			f.authReceived = true
			f.mu.Unlock()
			send("235 ok\r\n")
		case strings.HasPrefix(up, "QUIT"):
			send("221 bye\r\n")
			return
		default:
			send("250 ok\r\n")
		}
	}
}

func (f *fakeSMTPServer) authWasReceived() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authReceived
}

func (f *fakeSMTPServer) starttlsAttempted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starttlsSeen
}

// P1-3 (review): start_tls=true MUST fail when the server doesn't
// advertise the STARTTLS extension. Silently downgrading would leak
// AUTH credentials.
func TestSMTPSenderStartTLSRequiredFailsWhenUnsupported(t *testing.T) {
	fs := startFakeSMTP(t, false, true)
	defer fs.close()
	conn, err := net.Dial("tcp", fs.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	host, _, _ := net.SplitHostPort(fs.addr)
	auth := smtp.PlainAuth("", "user", "pass", host)
	err = sendOverConn(conn, host, auth, "from@x", []string{"to@y"}, []byte("hi"), true, false)
	if err == nil {
		t.Fatal("expected start_tls failure when server doesn't advertise STARTTLS")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("expected STARTTLS error, got %v", err)
	}
	if fs.authWasReceived() {
		t.Errorf("AUTH must not happen when STARTTLS was refused")
	}
}

// P1-3 (review): when start_tls=false (or unset) and the connection is
// plaintext, sendOverConn must refuse to send AUTH credentials.
func TestSMTPSenderDoesNotAuthOverPlaintextByDefault(t *testing.T) {
	fs := startFakeSMTP(t, false, true)
	defer fs.close()
	conn, err := net.Dial("tcp", fs.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	host, _, _ := net.SplitHostPort(fs.addr)
	auth := smtp.PlainAuth("", "user", "pass", host)
	err = sendOverConn(conn, host, auth, "from@x", []string{"to@y"}, []byte("hi"), false, false)
	if err == nil {
		t.Fatal("expected refuse-plaintext-auth error")
	}
	if !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("expected plaintext-auth error, got %v", err)
	}
	if fs.authWasReceived() {
		t.Errorf("AUTH must not be sent without TLS")
	}
}

// Sanity: with start_tls=true and the server advertising STARTTLS, the
// client at least attempts the STARTTLS handshake (the fake server
// aborts after the 220 so this only asserts intent, not success).
func TestSMTPSenderAttemptsStartTLSWhenSupported(t *testing.T) {
	fs := startFakeSMTP(t, true, true)
	defer fs.close()
	conn, err := net.Dial("tcp", fs.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	host, _, _ := net.SplitHostPort(fs.addr)
	auth := smtp.PlainAuth("", "user", "pass", host)
	// Expected: client attempts STARTTLS, our fake closes the TLS dance
	// so we get an error there — but importantly NO auth before TLS.
	_ = sendOverConn(conn, host, auth, "from@x", []string{"to@y"}, []byte("hi"), true, false)
	if !fs.starttlsAttempted() {
		t.Errorf("client did not even attempt STARTTLS handshake")
	}
	if fs.authWasReceived() {
		t.Errorf("AUTH must not happen until after STARTTLS succeeds")
	}
}
