// Package apps holds the HTTP clients for the apps s5n wires together. They are
// deliberately small and schema-driven: every request payload is built from the
// app's own schema endpoint where one exists, so a version bump changes field
// lists without changing this package.
package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Doer is the subset of *http.Client the clients need, so tests can inject a
// server.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is a base URL plus static headers. Every app client embeds one.
type Client struct {
	BaseURL string
	HTTP    Doer
	Header  http.Header
}

// NewClient builds a client with a sane timeout. Wiring is a local, short
// conversation; nothing here should hang for the Job's whole deadline.
func NewClient(baseURL string, h Doer) *Client {
	if h == nil {
		h = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: h, Header: http.Header{}}
}

// HTTPError is a non-2xx response, kept as an error value so callers can classify
// (a 409 on create is "already there", a 404 on lookup is "absent").
type HTTPError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	body := strings.Join(strings.Fields(e.Body), " ")
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	if body == "" {
		return fmt.Sprintf("%s %s: %s", e.Method, e.URL, http.StatusText(e.Status))
	}
	return fmt.Sprintf("%s %s: %s: %s", e.Method, e.URL, http.StatusText(e.Status), body)
}

// IsNotFound reports whether err is a 404 from this package.
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

// IsConflict reports whether err is a 409 from this package.
func IsConflict(err error) bool { return statusIs(err, http.StatusConflict) }

func statusIs(err error, status int) bool {
	he, ok := err.(*HTTPError)
	return ok && he.Status == status
}

// Get decodes a JSON response into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.send(ctx, http.MethodGet, path, "", nil, out, nil)
}

// Post sends a JSON body and decodes the response into out.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.sendJSON(ctx, http.MethodPost, path, body, out, nil)
}

// Put sends a JSON body and decodes the response into out.
func (c *Client) Put(ctx context.Context, path string, body, out any) error {
	return c.sendJSON(ctx, http.MethodPut, path, body, out, nil)
}

// PostForm sends an application/x-www-form-urlencoded body and returns the raw
// response text, which is what qBittorrent's auth endpoint answers with.
func (c *Client) PostForm(ctx context.Context, path string, form url.Values, out any) (string, error) {
	var text string
	err := c.send(ctx, http.MethodPost, path, "application/x-www-form-urlencoded",
		[]byte(form.Encode()), out, &text)
	return text, err
}

func (c *Client) sendJSON(ctx context.Context, method, path string, body, out any, text *string) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
	}
	return c.send(ctx, method, path, "application/json", payload, out, text)
}

func (c *Client) send(ctx context.Context, method, path, contentType string, payload []byte, out any, text *string) error {
	req, err := c.request(ctx, method, path, contentType, payload)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, req.URL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Method: method, URL: req.URL.String(), Status: resp.StatusCode, Body: string(data)}
	}
	if text != nil {
		*text = string(data)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, req.URL, err)
		}
	}
	return nil
}

// AnyStatus reports success for any HTTP response at all, including 401/403. It is
// the right readiness probe for apps that answer "forbidden" until a session
// exists: the socket, the listener and the routing are what is being waited for,
// not the payload.
func (c *Client) AnyStatus(ctx context.Context, path string) error {
	req, err := c.request(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (c *Client) request(ctx context.Context, method, path, contentType string, payload []byte) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	for k, values := range c.Header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// JSON is the default, but Plex content-negotiates /identity: asking for
	// application/json gets a JSON document, and the XML parser would then read
	// an empty body. A client that sets its own Accept keeps it.
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	return req, nil
}

// GetText fetches a plain-text endpoint (qBittorrent's version and auth
// endpoints answer with text, not JSON).
func (c *Client) GetText(ctx context.Context, path string) (string, error) {
	var s string
	err := c.send(ctx, http.MethodGet, path, "", nil, nil, &s)
	return s, err
}

// Retry calls fn until it succeeds or timeout passes. It is how the Job waits for
// an app to finish starting without holding the Helm hook open for every pod
// individually.
func Retry(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	delay := 500 * time.Millisecond
	var last error
	for {
		last = fn(ctx)
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s: %w", timeout, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), last)
		case <-time.After(delay):
		}
		if delay < 5*time.Second {
			delay *= 2
		}
	}
}
