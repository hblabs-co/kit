package scaleway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNotifier_Enabled(t *testing.T) {
	ok := New(Options{SecretKey: "k", ProjectID: "p"})
	from, to := Address{Email: "from@example.com"}, Address{Email: "to@example.com"}
	cases := map[string]struct {
		n    *Notifier
		want bool
	}{
		"nil notifier":  {nil, false},
		"nil client":    {NewNotifier(nil, from, to), false},
		"no creds":      {NewNotifier(New(Options{}), from, to), false},
		"no from":       {NewNotifier(ok, Address{}, to), false},
		"no to":         {NewNotifier(ok, from, Address{}), false},
		"configured ok": {NewNotifier(ok, from, to), true},
	}
	for name, c := range cases {
		if got := c.n.Enabled(); got != c.want {
			t.Errorf("%s: Enabled = %v, want %v", name, got, c.want)
		}
	}
	if err := NewNotifier(nil, from, to).Send(context.Background(), "s", "b", ""); !errors.Is(err, ErrNotifierDisabled) {
		t.Fatalf("disabled Send = %v", err)
	}
}

func TestNotifier_Send(t *testing.T) {
	var got Email
	var path, token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, token = r.URL.Path, r.Header.Get("X-Auth-Token")
		got = Email{}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(Options{SecretKey: "sk", ProjectID: "pid", BaseURL: srv.URL + "/"})
	n := NewNotifier(c, Address{Email: "no-reply@example.com", Name: "Site"}, Address{Email: "inbox@example.com"})
	if err := n.Send(context.Background(), "subj", "body", "user@example.com"); err != nil {
		t.Fatal(err)
	}
	if path != "/fr-par/emails" || token != "sk" || got.ProjectID != "pid" || got.From.Name != "Site" ||
		len(got.To) != 1 || got.To[0].Email != "inbox@example.com" || got.Subject != "subj" || got.Text != "body" ||
		len(got.Headers) != 1 || got.Headers[0].Key != "Reply-To" || got.Headers[0].Value != "user@example.com" {
		t.Fatalf("path=%s token=%s email=%+v", path, token, got)
	}
	if err := n.Send(context.Background(), "subj", "body", ""); err != nil || got.Headers != nil {
		t.Fatalf("no reply-to: err=%v headers=%v", err, got.Headers)
	}
}

func TestNotifier_SendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "nope")
	}))
	defer srv.Close()
	c := New(Options{SecretKey: "sk", ProjectID: "pid", BaseURL: srv.URL})
	err := NewNotifier(c, Address{Email: "f@example.com"}, Address{Email: "t@example.com"}).Send(context.Background(), "s", "b", "")
	if err == nil || !strings.Contains(err.Error(), "notify") || AsAPIError(err) == nil {
		t.Fatalf("err = %v", err)
	}
}
