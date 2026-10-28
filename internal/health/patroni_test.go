package health

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPatroniEligibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		role      Role
		status    int
		body      string
		want      State
		wantError bool
	}{
		{"primary", Primary, 200, `{"state":"running","role":"primary","server_version":180006}`, Healthy, false},
		{"replica", Replica, 200, `{"state":"running","role":"replica","tags":{"noloadbalance":false}}`, Healthy, false},
		{"replica_without_tags", Replica, 200, `{"state":"running","role":"replica"}`, Healthy, false},
		{"http_no_content", Primary, 204, ``, Unhealthy, false},
		{"null_state", Primary, 200, `{"state":null,"role":"primary"}`, Unknown, true},
		{"exact_body_limit", Primary, 200, `{"state":"running","role":"primary"}` + strings.Repeat(" ", 65536-len(`{"state":"running","role":"primary"}`)), Healthy, false},
		{"http_unhealthy", Primary, 503, `{"state":"running","role":"primary"}`, Unhealthy, false},
		{"wrong_role", Primary, 200, `{"state":"running","role":"replica"}`, Unhealthy, false},
		{"unsupported_legacy_role", Primary, 200, `{"state":"running","role":"master"}`, Unhealthy, false},
		{"stopped", Primary, 200, `{"state":"stopped","role":"primary"}`, Unhealthy, false},
		{"noloadbalance", Replica, 200, `{"state":"running","role":"replica","tags":{"noloadbalance":true}}`, Unhealthy, false},
		{"malformed", Primary, 200, `{`, Unknown, true},
		{"missing_fields", Primary, 200, `{}`, Unknown, true},
		{"trailing_document", Primary, 200, `{"state":"running","role":"primary"}{}`, Unknown, true},
		{"duplicate_state", Primary, 200, `{"state":"stopped","state":"running","role":"primary"}`, Unknown, true},
		{"escaped_duplicate", Primary, 200, `{"state":"stopped","\u0073tate":"running","role":"primary"}`, Unknown, true},
		{"duplicate_tag", Replica, 200, `{"state":"running","role":"replica","tags":{"noloadbalance":true,"noloadbalance":false}}`, Unknown, true},
		{"wrong_tag_type", Replica, 200, `{"state":"running","role":"replica","tags":{"noloadbalance":"false"}}`, Unknown, true},
		{"null_tags", Replica, 200, `{"state":"running","role":"replica","tags":null}`, Unknown, true},
		{"invalid_utf8", Primary, 200, "{\"state\":\"running\",\"role\":\"primary\",\"ignored\":\"" + string([]byte{255}) + "\"}", Unknown, true},
		{"body_too_large", Primary, 200, `{"state":"running","role":"primary"}` + strings.Repeat(" ", 65536), Unknown, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				expected := "/primary"
				if c.role == Replica {
					expected = "/replica?lag=1048576"
				}
				if r.URL.RequestURI() != expected {
					http.Error(w, "unexpected endpoint", 500)
					return
				}
				w.WriteHeader(c.status)
				_, _ = fmt.Fprint(w, c.body)
			}))
			defer server.Close()
			client, err := NewPatroni(server.URL, time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			got, err := client.Probe(context.Background(), c.role, 1048576)
			end := time.Now()
			if (err != nil) != c.wantError {
				t.Fatalf("error=%v, wantError=%v", err, c.wantError)
			}
			if got.State != c.want {
				t.Errorf("state=%v, want=%v", got.State, c.want)
			}
			if got.StartedAt.Before(start) || got.StartedAt.After(end) {
				t.Error("observation must retain this request's start time")
			}
		})
	}
}

func TestPatroniRejectsUnsafeURL(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"http://example.com", "http://192.0.2.1", "https://user:pass@example.com", "https://example.com/path", "https://example.com?token=value", "https://example.com#fragment", "file:///tmp/socket", "https:opaque", "https://example.com:0", "https://example.com:65536", "https://example.com:"} {
		t.Run(base, func(t *testing.T) {
			if _, err := NewPatroni(base, time.Second, nil); err == nil {
				t.Error("unsafe or ambiguous URL accepted")
			}
		})
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		if _, err := NewPatroni("http://127.0.0.1", timeout, nil); err == nil {
			t.Error("nonpositive timeout accepted")
		}
	}
}

func TestPatroniDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect target requested")
		_, _ = fmt.Fprint(w, `{"state":"running","role":"primary"}`)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	p, err := NewPatroni(source.URL, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Probe(context.Background(), Primary, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Unhealthy {
		t.Fatalf("redirect authorized: %+v", got)
	}
}

func TestPatroniRequiresTrustedTLSAndHostname(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state":"running","role":"primary"}`)
	}))
	defer server.Close()
	untrusted, err := NewPatroni(server.URL, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := untrusted.Probe(context.Background(), Primary, 0); err == nil || got.State != Unknown {
		t.Fatalf("untrusted certificate authorized: %+v %v", got, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	trusted, err := NewPatroni(server.URL, time.Second, roots)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := trusted.Probe(context.Background(), Primary, 0); err != nil || got.State != Healthy {
		t.Fatalf("trusted endpoint denied: %+v %v", got, err)
	}
	if server.Certificate().VerifyHostname("localhost") == nil {
		t.Fatal("fixture must exclude localhost")
	}
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "localhost:" + u.Port()
	wrongHost, err := NewPatroni(u.String(), time.Second, roots)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := wrongHost.Probe(context.Background(), Primary, 0); err == nil || got.State != Unknown {
		t.Fatalf("wrong certificate hostname authorized: %+v %v", got, err)
	}
}

func TestPatroniRejectsTLS12(t *testing.T) {
	t.Parallel()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state":"running","role":"primary"}`)
	}))
	// #nosec G402 -- TLS 1.2 is an intentionally rejected test fixture.
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	p, err := NewPatroni(server.URL, time.Second, roots)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := p.Probe(context.Background(), Primary, 0); err == nil || got.State != Unknown {
		t.Fatalf("TLS1.2 authorized: %+v %v", got, err)
	}
}

func TestPatroniBoundsBodyReadAndHonorsCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRequest), func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			p, err := NewPatroni(server.URL, 100*time.Millisecond, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				got, err := p.Probe(ctx, Primary, 0)
				if err == nil || got.State != Unknown {
					result <- fmt.Errorf("blocked response authorized: %+v %v", got, err)
					return
				}
				result <- nil
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not start")
			}
			if cancelRequest {
				cancel()
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("probe exceeded bounded deadline")
			}
		})
	}
}

func TestPatroniConcurrentProbes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state":"running","role":"primary"}`)
	}))
	defer server.Close()
	p, err := NewPatroni(server.URL, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if got, err := p.Probe(context.Background(), Primary, 0); err != nil || got.State != Healthy {
				t.Errorf("concurrent probe failed: %+v %v", got, err)
			}
		})
	}
	wg.Wait()
}

func TestPatroniBoundsHeadersAndRejectsUnsupportedRole(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Oversized", strings.Repeat("x", 70000))
		_, _ = fmt.Fprint(w, `{"state":"running","role":"primary"}`)
	}))
	defer server.Close()
	p, err := NewPatroni(server.URL, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := p.Probe(context.Background(), Primary, 0); err == nil || got.State != Unknown {
		t.Fatalf("oversized header authorized: %+v %v", got, err)
	}
	if got, err := p.Probe(context.Background(), Role("leader"), 0); err == nil || got.State != Unknown {
		t.Fatalf("unsupported role authorized: %+v %v", got, err)
	}
}

func TestPatroniSnapshotsTrustRoots(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state":"running","role":"primary"}`)
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	p, err := NewPatroni(server.URL, time.Second, roots)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(server.Certificate())
	if got, err := p.Probe(context.Background(), Primary, 0); err == nil || got.State != Unknown {
		t.Fatalf("caller changed trust after construction: %+v %v", got, err)
	}
}

type closeFailureBody struct{ io.Reader }

func (closeFailureBody) Close() error { return errBodyClose }

var errBodyClose = errors.New("synthetic body close failure")

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPatroniRejectsBodyCloseFailure(t *testing.T) {
	t.Parallel()
	p, err := NewPatroni("http://127.0.0.1", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: closeFailureBody{Reader: strings.NewReader(`{"state":"running","role":"primary"}`)}}, nil
	})
	got, err := p.Probe(context.Background(), Primary, 0)
	if got.State != Unknown || !errors.Is(err, errBodyClose) {
		t.Fatalf("close failure authorized: %+v %v", got, err)
	}
}
