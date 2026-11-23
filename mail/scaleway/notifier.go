package scaleway

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotifierDisabled is returned by Notifier.Send when the client
// has no credentials or the sender / recipient address is missing.
var ErrNotifierDisabled = errors.New("scaleway: notifier not configured")

// Notifier sends plain-text mails from one fixed sender to one fixed
// recipient: a website form forwarding submissions to an inbox, an
// alert to an operator. Its method set (Enabled, Send) is what form
// handlers expect from a mail backend, so the handler does not need
// to import this package.
type Notifier struct {
	client *Client
	from   Address
	to     Address
}

// NewNotifier sends through client from from to to. A nil or
// unconfigured client yields a disabled notifier rather than an
// error, so an app can boot without mail in local dev.
func NewNotifier(client *Client, from, to Address) *Notifier {
	return &Notifier{client: client, from: from, to: to}
}

// Enabled reports whether Send can succeed as far as configuration
// goes: credentials set and both addresses known.
func (n *Notifier) Enabled() bool {
	return n != nil && n.client.Configured() && n.from.Email != "" && n.to.Email != ""
}

// Send delivers one plain-text mail. A non-empty replyTo becomes the
// Reply-To header, so answering the mail reaches that address.
func (n *Notifier) Send(ctx context.Context, subject, text, replyTo string) error {
	if !n.Enabled() {
		return ErrNotifierDisabled
	}
	e := Email{
		From:    n.from,
		To:      []Address{n.to},
		Subject: subject,
		Text:    text,
	}
	if replyTo != "" {
		e.Headers = []Header{ReplyTo(replyTo)}
	}
	if _, err := n.client.Send(ctx, e); err != nil {
		return fmt.Errorf("scaleway: notify: %w", err)
	}
	return nil
}
