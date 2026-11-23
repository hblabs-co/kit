// Package scaleway is a small client for Scaleway's Transactional
// Email API (TEM, v1alpha1). It wraps a single endpoint — POST /emails
// — because that is all the apps need: send one mail, know whether a
// failure is worth retrying.
//
// Why stdlib only: the official scaleway-sdk-go pulls in a big tree
// of regional / IAM helpers we don't use. A small wrapper keeps the
// dependency surface obvious (this module has no dependencies at all)
// and a fake for testing easy.
//
// Errors come in two shapes:
//
//   - *APIError when Scaleway returned 4xx/5xx. The caller can
//     distinguish retryable (5xx, 429) from terminal (4xx) with
//     Retryable — a queue consumer uses that split to decide whether
//     to redeliver the message or drop it.
//   - Plain error (wrapped) for transport-level failures (DNS,
//     timeout, malformed response body). Treat these as retryable.
//
// Client reads no environment variables: the app passes the secret
// key, project ID and region in Options. NotifierFromEnv is the one
// helper that reads the standard HB_SCALEWAY_* / HB_MAIL_FROM* keys.
package scaleway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Configuration errors returned by Send before any request is made.
// They are terminal: retrying cannot fix a missing credential.
var (
	ErrNoSecretKey = errors.New("scaleway: secret key not set")
	ErrNoProjectID = errors.New("scaleway: project ID not set")
)

// Configured reports whether the client has the credentials Send
// needs. Apps use it to run without mail in local dev ("log the mail
// instead") without attempting a request that can only fail.
func (c *Client) Configured() bool {
	return c != nil && c.SecretKey != "" && c.ProjectID != ""
}

// APIBase is the global root for Scaleway's TEM API. Exported for
// callers that probe the API themselves (e.g. a key-validity check). Region is part
// of the path because TEM is a regional service; the client appends
// `/<region>/emails` when assembling the URL.
const APIBase = "https://api.scaleway.com/transactional-email/v1alpha1/regions"

// defaultRegion is what New uses when the caller passes an empty
// region. fr-par is the most popular Scaleway region today; callers
// override it through Options.Region (nl-ams, pl-waw).
const defaultRegion = "fr-par"

// defaultHTTPTimeout caps any single Send. Scaleway's TEM responds
// in <300ms steady state; 10 seconds is a generous ceiling that
// keeps a wedged TCP connection from holding a consumer
// goroutine forever.
const defaultHTTPTimeout = 10 * time.Second

// Client carries the secret key + project ID + region once so the
// caller doesn't pass them on every Send. Safe for concurrent use.
type Client struct {
	SecretKey string
	ProjectID string
	Region    string
	BaseURL   string // overridden in tests; production uses APIBase
	HTTP      *http.Client
}

// Options configures New. Empty values pick defaults: leave Region
// empty to fall back to fr-par.
type Options struct {
	SecretKey string
	ProjectID string
	Region    string

	// BaseURL is the scheme+host the client hits instead of
	// `APIBase`. Tests pass an httptest.Server.URL here; production
	// callers leave it empty.
	BaseURL string

	// HTTPClient overrides the default 10-second-timeout client.
	// Production callers leave this nil; tests pass a fake client
	// when they need to stub the transport instead of the server.
	HTTPClient *http.Client
}

// New constructs a client. Region defaults to fr-par; BaseURL
// defaults to Scaleway's production root; HTTPClient defaults to a
// 10-second-timeout client. An empty region read from the
// environment therefore still picks fr-par.
func New(opts Options) *Client {
	if opts.Region == "" {
		opts.Region = defaultRegion
	}
	if opts.BaseURL == "" {
		opts.BaseURL = APIBase
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		SecretKey: opts.SecretKey,
		ProjectID: opts.ProjectID,
		Region:    opts.Region,
		BaseURL:   opts.BaseURL,
		HTTP:      opts.HTTPClient,
	}
}

// Address is the {email, name} pair Scaleway expects for from / to.
// Name is optional — leave empty if the recipient has no display
// name on their side.
type Address struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// Header is one extra RFC 5322 header on the outgoing mail
// (Scaleway's `additional_headers`), e.g. Reply-To.
type Header struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ReplyTo returns the Reply-To header for addr. Typical use on a
// contact form: the mail is sent from a no-reply sender and answering
// it reaches the visitor.
func ReplyTo(addr string) Header { return Header{Key: "Reply-To", Value: addr} }

// Email is the request payload. Subject is required; supply at
// least one of Text or HTML. Scaleway rejects mails with both
// fields empty (400 invalid_request). ProjectID is filled in by
// Send — callers leave it zero.
type Email struct {
	From    Address   `json:"from"`
	To      []Address `json:"to"`
	Subject string    `json:"subject"`
	Text    string    `json:"text,omitempty"`
	HTML    string    `json:"html,omitempty"`

	// Headers are optional additional headers (see ReplyTo).
	Headers []Header `json:"additional_headers,omitempty"`

	ProjectID string `json:"project_id"`
}

// SendResponse is a slim subset of TEM's response — enough to
// confirm the message was queued and surface its ID for tracing.
// Scaleway returns more fields (status, recipients[].status, …)
// that we ignore here; expand if the operator surface grows.
type SendResponse struct {
	Emails []struct {
		ID        string `json:"id"`
		MessageID string `json:"message_id"`
		Status    string `json:"status"`
	} `json:"emails"`
}

// APIError is the shape Scaleway returns on 4xx/5xx. Distinguish
// from transport errors so callers can retry on network blips but
// not on "invalid_argument".
//
// Retryable reports whether the status indicates a transient
// failure (5xx + 429). A queue consumer uses that to decide
// between redelivery (retryable) and ack-with-log (terminal — bad
// payload, missing permission).
type APIError struct {
	Status  int    `json:"-"`
	Type    string `json:"type"`
	Message string `json:"message"`
	RawBody string `json:"-"`
}

// Error implements the error interface. Includes the status code
// so it shows up in handler logs without an extra Sprintf at
// the call site.
func (e *APIError) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("scaleway %d: %s — %s", e.Status, e.Type, e.Message)
	}
	return fmt.Sprintf("scaleway %d: %s", e.Status, strings.TrimSpace(e.RawBody))
}

// Retryable returns true for status codes that the handler should
// nak. 5xx is the obvious case; 429 means Scaleway is throttling
// us and a retry after the configured backoff is correct.
func (e *APIError) Retryable() bool {
	if e == nil {
		return false
	}
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

// Send submits one Email to TEM. Returns the parsed response or an
// *APIError on 4xx/5xx; transport-level failures come back as
// wrapped plain errors. Honours the caller's context for
// cancellation (so a SIGTERM aborts an in-flight Send).
func (c *Client) Send(ctx context.Context, e Email) (*SendResponse, error) {
	if c.SecretKey == "" {
		return nil, ErrNoSecretKey
	}
	if c.ProjectID == "" {
		return nil, ErrNoProjectID
	}
	e.ProjectID = c.ProjectID

	body, err := marshalFn(e)
	if err != nil {
		return nil, fmt.Errorf("scaleway marshal: %w", err)
	}

	url := fmt.Sprintf("%s/%s/emails", strings.TrimRight(c.BaseURL, "/"), c.Region)
	req, err := newRequestFn(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("scaleway request: %w", err)
	}
	req.Header.Set("X-Auth-Token", c.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scaleway transport: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		apiErr := &APIError{Status: resp.StatusCode, RawBody: string(raw)}
		// Best-effort: pull the typed message from the body when
		// Scaleway returned a JSON error envelope.
		_ = json.Unmarshal(raw, apiErr)
		return nil, apiErr
	}

	var out SendResponse
	if len(bytes.TrimSpace(raw)) == 0 {
		// Accepted with an empty body: the mail is queued, there is
		// just no ID to report. Not an error — returning one here
		// would make a caller retry a mail that was already sent.
		return &out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("scaleway decode: %w (body=%s)", err, string(raw))
	}
	return &out, nil
}

// AsAPIError unwraps err to its underlying *APIError, returning nil
// when the chain doesn't contain one. Helper for callers that want
// `if apiErr := scaleway.AsAPIError(err); apiErr != nil { ... }`
// without writing the errors.As boilerplate every time.
func AsAPIError(err error) *APIError {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return nil
}

// marshalFn and newRequestFn are variable seams the tests swap to
// exercise the stdlib error branches Send checks for. Production
// paths are the obvious identity wrappers.
var (
	marshalFn    = json.Marshal
	newRequestFn = http.NewRequestWithContext
)
