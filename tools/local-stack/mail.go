package main

// Real transactional email for NSM (verification codes, support replies).
//
// Configuration is read from environment variables on the server that runs
// NSM-Masterlist.exe — credentials are NEVER stored in files:
//
//	NSM_SMTP_HOST  SMTP server hostname (e.g. smtp.zohomail.com) — unset = no delivery
//	NSM_SMTP_PORT  SMTP port (587 = STARTTLS, 465 = implicit TLS; default 587)
//	NSM_SMTP_USER  SMTP login (usually support@nsmultiplayer.com)
//	NSM_SMTP_PASS  SMTP app password
//	NSM_MAIL_FROM  From address (default support@nsmultiplayer.com)
//
// When SMTP is not configured, verification codes are still shown in the
// in-app notification and written to the service log for local development —
// but callers are told the truth (mail was NOT sent).

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"os"
	"regexp"
	"strings"
	"time"
)

var emailFormatRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// validEmailAddress does a pragmatic RFC-lite check: one @, non-empty local,
// dotted domain. The code must never silently accept a blank or fake address.
func validEmailAddress(email string) bool {
	email = strings.TrimSpace(email)
	if email == "" || len(email) > 254 || strings.Contains(email, "@users.nsm.local") {
		return false
	}
	return emailFormatRe.MatchString(email)
}

func mailFrom() string {
	if v := strings.TrimSpace(os.Getenv("NSM_MAIL_FROM")); v != "" {
		return v
	}
	return "support@nsmultiplayer.com"
}

func smtpConfigured() bool {
	return strings.TrimSpace(os.Getenv("NSM_SMTP_HOST")) != ""
}

// sendMail delivers one plain-text message through the configured SMTP relay.
// It fails loudly and returns the real error — success is never faked.
func sendMail(to, subject, body string) error {
	host := strings.TrimSpace(os.Getenv("NSM_SMTP_HOST"))
	if host == "" {
		return fmt.Errorf("email delivery is not configured (NSM_SMTP_HOST is unset)")
	}
	port := strings.TrimSpace(os.Getenv("NSM_SMTP_PORT"))
	if port == "" {
		port = "587"
	}
	user := os.Getenv("NSM_SMTP_USER")
	pass := os.Getenv("NSM_SMTP_PASS")
	from := mailFrom()

	addr := net.JoinHostPort(host, port)
	var client *smtp.Client
	var err error
	if port == "465" {
		dialer := &net.Dialer{Timeout: 15 * time.Second}
		conn, derr := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
		if derr != nil {
			return derr
		}
		client, err = smtp.NewClient(conn, host)
	} else {
		client, err = smtp.Dial(addr)
	}
	if err != nil {
		return err
	}
	defer client.Close()

	if port != "465" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
				return err
			}
		}
	}
	if user != "" {
		auth := smtp.PlainAuth("", user, pass, host)
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	wc, err := client.Data()
	if err != nil {
		return err
	}
	msg := strings.Join([]string{
		"From: NSM Support <" + from + ">",
		"To: <" + to + ">",
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Date: " + time.Now().Format(time.RFC1123Z),
		"",
		body,
	}, "\r\n")
	if _, err := wc.Write([]byte(msg)); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// dispatchVerificationEmail emails the 6-digit verification code and reports
// whether the message was actually handed to the mail relay.
func dispatchVerificationEmail(email, username, code string) bool {
	if email == "" {
		return false
	}
	body := "Hi " + username + ",\n\n" +
		"Your 6-digit NSM email verification code is: " + code + "\n\n" +
		"Enter it at https://nsmultiplayer.com/verify-email to verify " + email + ".\n\n" +
		"If you did not create an NSM account, you can ignore this email.\n\n" +
		"— NSM Support (support@nsmultiplayer.com)"
	if err := sendMail(email, "Your NSM Email Verification Code: "+code, body); err != nil {
		log.Printf("[email-verify] mail to %s for @%s NOT sent (%v) — code for local use: %s", email, username, err, code)
		return false
	}
	log.Printf("[email-verify] verification email sent to %s for @%s", email, username)
	return true
}
