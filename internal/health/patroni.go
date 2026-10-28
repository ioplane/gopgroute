package health

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const responseLimit = 64 << 10

// Patroni owns a bounded HTTP client; concurrent probes do not share observations.
type Patroni struct {
	base   *url.URL
	client *http.Client
}

// NewPatroni requires loopback HTTP or verified TLS 1.3 and rejects URL credentials.
func NewPatroni(base string, timeout time.Duration, roots *x509.CertPool) (*Patroni, error) {
	if timeout <= 0 {
		return nil, errors.New("health timeout must be positive")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, errors.New("invalid Patroni base URL")
	}
	if u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("Patroni base URL must contain only scheme and authority")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("invalid Patroni port")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid Patroni port")
		}
	}
	switch u.Scheme {
	case "http":
		ip, err := netip.ParseAddr(u.Hostname())
		if err != nil || !ip.IsLoopback() {
			return nil, errors.New("plain HTTP requires a literal loopback address")
		}
	case "https":
	default:
		return nil, errors.New("Patroni URL must use HTTP or HTTPS")
	}
	if roots != nil {
		roots = roots.Clone()
	}
	transport := &http.Transport{
		DialContext:            (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		MaxResponseHeaderBytes: responseLimit,
		DisableKeepAlives:      true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots},
	}
	return &Patroni{base: u, client: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Probe checks a role-specific endpoint. Replica lag is expressed in bytes.
func (p *Patroni) Probe(ctx context.Context, role Role, maxLagBytes uint64) (result Check, err error) {
	result.StartedAt = time.Now()
	endpoint := *p.base
	switch role {
	case Primary:
		endpoint.Path = "/primary"
	case Replica:
		endpoint.Path = "/replica"
		endpoint.RawQuery = "lag=" + strconv.FormatUint(maxLagBytes, 10)
	default:
		return result, errors.New("unsupported Patroni role")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return result, fmt.Errorf("create Patroni health request: %w", err)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return result, fmt.Errorf("request Patroni health: %w", err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			result.State = Unknown
			err = errors.Join(err, fmt.Errorf("close Patroni health body: %w", closeErr))
		}
	}()
	if response.StatusCode != http.StatusOK {
		result.State = Unhealthy
		return result, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		return result, fmt.Errorf("read Patroni health: %w", err)
	}
	if len(body) > responseLimit || !utf8.Valid(body) {
		return result, errors.New("Patroni health body exceeds limit or is not UTF-8")
	}
	values, err := object(body)
	if err != nil {
		return result, fmt.Errorf("decode Patroni health: %w", err)
	}
	state, err := requiredString(values, "state")
	if err != nil {
		return result, err
	}
	actualRole, err := requiredString(values, "role")
	if err != nil {
		return result, err
	}
	noLoadBalance := false
	if raw, ok := values["tags"]; ok {
		tags, err := object(raw)
		if err != nil {
			return result, fmt.Errorf("decode Patroni tags: %w", err)
		}
		if raw, ok := tags["noloadbalance"]; ok {
			var value *bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return result, fmt.Errorf("decode noloadbalance: %w", err)
			}
			if value == nil {
				return result, errors.New("null noloadbalance tag")
			}
			noLoadBalance = *value
		}
	}
	result.State = Unhealthy
	if state == "running" && actualRole == string(role) && (role != Replica || !noLoadBalance) {
		result.State = Healthy
	}
	return result, nil
}

// object rejects ambiguous names while preserving unknown fields for compatibility.
func object(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("expected an object name")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate JSON object name")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON data")
	}
	return fields, nil
}

func requiredString(fields map[string]json.RawMessage, key string) (string, error) {
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("missing Patroni field %s", key)
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("decode Patroni field %s: %w", key, err)
	}
	if value == nil {
		return "", fmt.Errorf("null Patroni field %s", key)
	}
	return *value, nil
}
