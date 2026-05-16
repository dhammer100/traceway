package services

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/tracewayapp/traceway/backend/app/config"
)

type emailService struct {
	enabled  bool
	host     string
	port     int
	username string
	password string
	from     string
	baseUrl  string
}

var EmailService *emailService

// errHeaderInjection is returned when an attempted email header value contains
// CR/LF and could be used to smuggle additional headers (e.g. Bcc:).
var errHeaderInjection = errors.New("email header value contains forbidden CR/LF")

// sanitizeHeaderValue strips CR/LF/NUL so a value can be safely interpolated
// into a header. Header values must be single-line per RFC 5322.
func sanitizeHeaderValue(v string) (string, error) {
	if strings.ContainsAny(v, "\r\n\x00") {
		return "", errHeaderInjection
	}
	return v, nil
}

// sanitizeHeaderValueLossy is the same as sanitizeHeaderValue but replaces
// offending characters with spaces instead of erroring. Used for trusted-ish
// inputs (e.g. operator-set From address) where a hard failure would block
// legitimate mail.
func sanitizeHeaderValueLossy(v string) string {
	return strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(v)
}

func InitEmail() {
	cfg := config.Config

	enabled := cfg.SMTPEnabled == "true"
	port, _ := strconv.Atoi(cfg.SMTPPort)
	if port == 0 {
		port = 587
	}

	baseUrl := cfg.AppBaseURL
	if baseUrl == "" {
		baseUrl = "http://localhost:5173"
	}

	EmailService = &emailService{
		enabled:  enabled,
		host:     cfg.SMTPHost,
		port:     port,
		username: cfg.SMTPUsername,
		password: cfg.SMTPPassword,
		from:     sanitizeHeaderValueLossy(cfg.SMTPFrom),
		baseUrl:  baseUrl,
	}

	if enabled {
		config.Logln("Email service initialized with SMTP")
	} else {
		config.Logln("Email service initialized in log-only mode (SMTP disabled)")
	}
}

func (e *emailService) SendInvitation(toEmail string, inviterName string, orgName string, token string) error {
	to, err := sanitizeHeaderValue(toEmail)
	if err != nil {
		return fmt.Errorf("invalid recipient: %w", err)
	}
	// inviter name and org name come from user/admin-controlled fields stored
	// in the DB — refuse to emit a message that injects headers via them.
	safeInviter, err := sanitizeHeaderValue(inviterName)
	if err != nil {
		return fmt.Errorf("invalid inviter name: %w", err)
	}
	safeOrg, err := sanitizeHeaderValue(orgName)
	if err != nil {
		return fmt.Errorf("invalid organization name: %w", err)
	}

	inviteUrl := fmt.Sprintf("%s/accept-invitation?token=%s", e.baseUrl, token)

	subject := fmt.Sprintf("You've been invited to join %s on Traceway", safeOrg)
	body := fmt.Sprintf(`Hello,

%s has invited you to join %s on Traceway.

Click the link below to accept the invitation:
%s

This invitation will expire in 7 days.

If you did not expect this invitation, you can safely ignore this email.

Best regards,
The Traceway Team
`, safeInviter, safeOrg, inviteUrl)

	if !e.enabled {
		config.Logf("[EMAIL LOG] To: %s\nSubject: %s\nBody:\n%s", to, subject, body)
		return nil
	}

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		e.from, to, subject, body)

	if err := e.sendMail([]string{to}, []byte(msg)); err != nil {
		config.Logf("Failed to send invitation email to %s: %v", to, err)
		return err
	}

	config.Logf("Invitation email sent to %s for organization %s", to, safeOrg)
	return nil
}

func (e *emailService) IsEnabled() bool {
	return e.enabled
}

func (e *emailService) SendPasswordReset(toEmail string, token string) error {
	to, err := sanitizeHeaderValue(toEmail)
	if err != nil {
		return fmt.Errorf("invalid recipient: %w", err)
	}

	resetUrl := fmt.Sprintf("%s/reset-password?token=%s", e.baseUrl, token)

	subject := "Reset your Traceway password"
	body := fmt.Sprintf(`Hello,

You requested to reset your password for your Traceway account.

Click the link below to reset your password:
%s

This link will expire in 1 hour.

If you did not request this password reset, you can safely ignore this email.

Best regards,
The Traceway Team
`, resetUrl)

	if !e.enabled {
		config.Logf("[EMAIL LOG] To: %s\nSubject: %s\nBody:\n%s", to, subject, body)
		return nil
	}

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		e.from, to, subject, body)

	if err := e.sendMail([]string{to}, []byte(msg)); err != nil {
		config.Logf("Failed to send password reset email to %s: %v", to, err)
		return err
	}

	config.Logf("Password reset email sent to %s", to)
	return nil
}

func (e *emailService) sendMail(to []string, msg []byte) error {
	addr := net.JoinHostPort(e.host, strconv.Itoa(e.port))

	var conn net.Conn
	var err error
	switch e.port {
	case 465:
		// Implicit TLS — wrap the TCP connection in TLS before SMTP starts.
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: e.host,
			MinVersion: tls.VersionTLS12,
		})
	default:
		conn, err = net.DialTimeout("tcp", addr, 10*time.Second)
	}
	if err != nil {
		return fmt.Errorf("SMTP dial failed: %w", err)
	}

	client, err := smtp.NewClient(conn, e.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SMTP client failed: %w", err)
	}
	defer client.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Submission ports (587, 25) expect STARTTLS — upgrade before auth so that
	// SMTP_USERNAME / SMTP_PASSWORD are not sent in cleartext.
	if e.port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{
				ServerName: e.host,
				MinVersion: tls.VersionTLS12,
			}); err != nil {
				return fmt.Errorf("SMTP STARTTLS failed: %w", err)
			}
		} else if e.username != "" {
			// If the server doesn't advertise STARTTLS but we'd otherwise send
			// credentials in cleartext, refuse — operators should switch ports
			// or hosts rather than leak credentials silently.
			return errors.New("SMTP server does not support STARTTLS; refusing to send credentials in cleartext")
		}
	}

	if e.username != "" {
		auth := smtp.PlainAuth("", e.username, e.password, e.host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth failed: %w", err)
		}
	}
	if err := client.Mail(e.from); err != nil {
		return fmt.Errorf("SMTP MAIL failed: %w", err)
	}
	for _, r := range to {
		if err := client.Rcpt(r); err != nil {
			return fmt.Errorf("SMTP RCPT failed: %w", err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA failed: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("SMTP write failed: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP close data failed: %w", err)
	}
	return client.Quit()
}
