package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// monitor.go queries explicit account endpoints with bounded, redirect-free requests.
// Public target resolution is pinned at dialing time. Transport, authentication,
// quota and malformed-response failures remain distinct and never become zero balances.

var (
	errWallet = errors.New("SITE_BALANCE_UNAVAILABLE")       // Missing authoritative account amount.
	errAuth   = errors.New("UPSTREAM_AUTHENTICATION_FAILED") // Rejected monitoring account credential.
)

// validHostURL restricts the management credential to the local host API.
func validHostURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}

// publicDial resolves and connects only globally routable destination addresses.
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, a := range ips {
		if !a.IP.IsGlobalUnicast() || a.IP.IsPrivate() || a.IP.IsLoopback() || a.IP.IsLinkLocalUnicast() {
			return nil, errors.New("private target rejected")
		}
	}
	var last error
	for _, a := range ips {
		c, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
		if e == nil {
			return c, nil
		}
		last = e
	}
	if last == nil {
		last = errors.New("no target addresses")
	}
	return nil, last
}

// monitorClient disables redirects and ambient proxies to contain credential delivery.
func monitorClient(proxy string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = publicDial
	if proxy != "" {
		if u, e := url.Parse(proxy); e == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: 25 * time.Second, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// endpoint preserves deployment prefixes while removing one conventional v1 suffix.
func endpoint(base, path string) string {
	return strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1") + path
}

func request(ctx context.Context, c *http.Client, target, key, user string) ([]byte, int, error) {
	body, status, _, err := requestWithHeaders(ctx, c, target, key, user)
	return body, status, err
}

// requestWithHeaders reads at most two MiB. Response data stays in the monitor.
func requestWithHeaders(ctx context.Context, c *http.Client, target, key, user string) ([]byte, int, http.Header, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	r.Header.Set("Accept", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if user != "" {
		r.Header.Set("New-Api-User", user)
	}
	response, err := c.Do(r)
	if err != nil {
		return nil, 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if len(body) > 2*1024*1024 {
		return nil, response.StatusCode, response.Header, errWallet
	}
	return body, response.StatusCode, response.Header, err
}

func sub2APIAuthDiagnostic(status int, headers http.Header, body []byte) string {
	if strings.EqualFold(headers.Get("Cf-Mitigated"), "challenge") {
		return "UPSTREAM_CLOUDFLARE_CHALLENGE"
	}
	if json.Valid(body) {
		code := gjson.GetBytes(body, "code").String()
		switch status {
		case http.StatusUnauthorized:
			switch code {
			case "API_KEY_REQUIRED", "INVALID_API_KEY", "API_KEY_DISABLED", "USER_NOT_FOUND", "USER_INACTIVE":
				return code
			}
		case http.StatusForbidden:
			switch code {
			case "ACCESS_DENIED", "GROUP_DELETED", "GROUP_DISABLED", "GROUP_NOT_ALLOWED",
				"API_KEY_EXPIRED", "INSUFFICIENT_BALANCE", "SUBSCRIPTION_NOT_FOUND":
				return code
			}
		}
		return "UPSTREAM_JSON_AUTH_REJECTION"
	}
	mediaType, _, _ := mime.ParseMediaType(headers.Get("Content-Type"))
	if mediaType == "text/html" || strings.HasPrefix(http.DetectContentType(body), "text/html;") {
		return "UPSTREAM_HTML_AUTH_REJECTION"
	}
	return ""
}

func walletErrorDiagnostic(err error) string {
	var detail newAPIWalletDiagnostic
	if errors.As(err, &detail) {
		switch detail {
		case newAPIKeyQuotaOnly, newAPINoAccountBalanceWithKey:
			return detail.WalletDiagnostic()
		}
	}
	return ""
}

// observe classifies a wallet or explicitly configured probe without provider writes.
func (p *Plugin) observe(parent context.Context, c Config, s Secrets, source string) Observation {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	o := Observation{RunID: hex.EncodeToString(nonce), Source: source, At: time.Now().UTC(), Outcome: Unknown, Code: "SITE_BALANCE_UNAVAILABLE"}
	client := p.client
	if s.ProxyURL != "" {
		client = monitorClient(s.ProxyURL)
		defer client.CloseIdleConnections()
	}
	path := map[string]string{"sub2api": "/v1/usage", "zivv": "/v1/user/balance", "derouter": "/balance", "kimi_balance": "/v1/users/me/balance", "deepseek_balance": "/user/balance", "new_api": "/user/balance"}[c.Adapter]
	target, key, user := endpoint(c.BaseURL, path), s.Key, ""
	var probe *Probe
	if source != "balance" {
		for i := range c.Probes {
			if c.Probes[i].ID == source {
				probe = &c.Probes[i]
				break
			}
		}
		if probe == nil {
			o.Outcome = Configuration
			o.Code = "PROBE_NOT_CONFIGURED"
			return o
		}
		target = probe.URL
		key = s.ProbeKeys[source]
		if probe.UseWalletKey {
			key = s.Key
		}
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, time.Duration(probe.TimeoutSeconds)*time.Second)
		defer stop()
	}
	var body []byte
	var status int
	var headers http.Header
	var err error
	newAPI := source == "balance" && c.Adapter == "new_api"
	if newAPI {
		o.Balances, status, err = queryNewAPI(ctx, client, c.BaseURL, s)
	} else {
		body, status, headers, err = requestWithHeaders(ctx, client, target, key, user)
	}
	o.At = time.Now().UTC()
	o.HTTPStatus = status
	if errors.Is(err, errWallet) {
		o.Diagnostic = walletErrorDiagnostic(err)
		return o
	}
	if errors.Is(err, errAuth) {
		o.Outcome = Authentication
		o.Code = errAuth.Error()
		return o
	}
	if errors.Is(err, ErrInvalid) {
		o.Outcome = Configuration
		o.Code = "NEW_API_USER_ID_INVALID"
		return o
	}
	if err != nil {
		o.Code = "MONITOR_REQUEST_FAILED"
		o.Outcome = MonitorFailure
		if s.ProxyURL == "" && parent.Err() == nil {
			if isNetworkOutage(err) {
				o.Outcome = Unreachable
				o.Code = "UPSTREAM_UNREACHABLE"
			}
		}
		return o
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		o.Outcome = Authentication
		o.Code = "UPSTREAM_AUTHENTICATION_FAILED"
		if source == "balance" && c.Adapter == "sub2api" {
			o.Diagnostic = sub2APIAuthDiagnostic(status, headers, body)
		}
		return o
	}
	if status == 429 {
		o.Outcome = RateLimited
		o.Code = "UPSTREAM_RATE_LIMITED"
		return o
	}
	if status >= 500 && s.ProxyURL == "" {
		o.Outcome = Unreachable
		o.Code = "UPSTREAM_UNAVAILABLE"
		return o
	}
	if status < 200 || status >= 300 {
		return o
	}
	if probe != nil {
		ok := len(probe.SuccessCodes) == 0
		for _, allowed := range probe.SuccessCodes {
			ok = ok || status == allowed
		}
		if ok && json.Valid(body) && gjson.GetBytes(body, probe.JSONPath).Exists() && gjson.GetBytes(body, probe.JSONPath).String() == probe.Expected {
			o.Outcome = Healthy
			o.Code = "PROBE_OK"
		} else {
			o.Code = "PROBE_RESPONSE_MISMATCH"
			if json.Valid(body) && gjson.GetBytes(body, probe.JSONPath).Exists() {
				o.Outcome = ProbeUnhealthy
			}
		}
		return o
	}
	if !newAPI {
		o.Balances, err = parseWallet(c.Adapter, body)
	}
	if errors.Is(err, errAuth) {
		o.Outcome = Authentication
		o.Code = errAuth.Error()
		return o
	}
	if err != nil {
		return o
	}
	if len(o.Balances) == 0 {
		return o
	}
	o.Outcome = Healthy
	o.Code = "WALLET_OK"
	return o
}

// isNetworkOutage accepts connection and DNS failures while excluding TLS/configuration errors.
// An outer url.Error implements net.Error even for invalid header values, so its
// mere presence cannot establish an upstream outage or justify suspending providers.
func isNetworkOutage(err error) bool {
	var cert *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &cert) || errors.As(err, &authority) || errors.As(err, &invalid) {
		return false
	}
	var op *net.OpError
	var dns *net.DNSError
	if errors.As(err, &op) || errors.As(err, &dns) {
		return true
	}
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	return errors.Is(err, context.DeadlineExceeded)
}
