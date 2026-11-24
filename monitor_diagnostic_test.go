package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSub2APIBalanceAuthDiagnostics(t *testing.T) {
	const privateDetail = "private-upstream-detail"
	for _, tc := range []struct {
		name       string
		status     int
		mediaType  string
		mitigated  string
		body       string
		diagnostic string
	}{
		{"IP restriction code", 403, "application/json", "", `{"code":"ACCESS_DENIED","message":"private-upstream-detail"}`, "ACCESS_DENIED"},
		{"group denial code", 403, "application/json", "", `{"code":"GROUP_NOT_ALLOWED"}`, "GROUP_NOT_ALLOWED"},
		{"expired key code", 403, "application/json", "", `{"code":"API_KEY_EXPIRED"}`, "API_KEY_EXPIRED"},
		{"insufficient account balance code", 403, "application/json", "", `{"code":"INSUFFICIENT_BALANCE","isValid":true,"balance":0,"unit":"USD"}`, "INSUFFICIENT_BALANCE"},
		{"missing subscription code", 403, "application/json", "", `{"code":"SUBSCRIPTION_NOT_FOUND"}`, "SUBSCRIPTION_NOT_FOUND"},
		{"invalid key code", 401, "application/json", "", `{"code":"INVALID_API_KEY"}`, "INVALID_API_KEY"},
		{"unknown code", 403, "application/json", "", `{"code":"private-upstream-detail"}`, "UPSTREAM_JSON_AUTH_REJECTION"},
		{"known code in message", 403, "application/json", "", `{"message":"ACCESS_DENIED"}`, "UPSTREAM_JSON_AUTH_REJECTION"},
		{"Cloudflare challenge", 403, "text/html", "challenge", `<html>private-upstream-detail</html>`, "UPSTREAM_CLOUDFLARE_CHALLENGE"},
		{"generic HTML", 403, "text/html; charset=utf-8", "private-upstream-detail", `<html>private-upstream-detail</html>`, "UPSTREAM_HTML_AUTH_REJECTION"},
		{"unrecognized response", 403, "text/plain", "", `private-upstream-detail`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/usage" || r.Header.Get("Authorization") != "Bearer private-wallet-key" {
					t.Error("unexpected request to wallet endpoint")
				}
				w.Header().Set("Content-Type", tc.mediaType)
				w.Header().Set("Cf-Mitigated", tc.mitigated)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			p, _ := testPlugin(t)
			p.client = server.Client()
			site := addSite(t, p, server.URL)
			response := call(t, p, http.MethodPost, fmt.Sprintf("/sites/%d/query", site.ID), nil)
			got := readPricingResponse[Observation](t, response)
			require.Equal(t, tc.status, got.HTTPStatus)
			require.Equal(t, Authentication, got.Outcome)
			require.Equal(t, "UPSTREAM_AUTHENTICATION_FAILED", got.Code)
			require.Equal(t, tc.diagnostic, got.Diagnostic)
			require.Empty(t, got.Balances)
			if tc.diagnostic == "" {
				require.NotContains(t, response.Body.String(), `"diagnostic"`)
			}
			history := call(t, p, http.MethodGet, fmt.Sprintf("/sites/%d/history?source=balance", site.ID), nil)
			rows := readPricingResponse[struct{ Items []Observation }](t, history)
			require.Len(t, rows.Items, 1)
			require.Equal(t, tc.diagnostic, rows.Items[0].Diagnostic)
			for _, body := range []string{response.Body.String(), history.Body.String()} {
				require.NotContains(t, body, privateDetail)
				require.NotContains(t, body, "private-wallet-key")
			}
		})
	}
}

func TestWalletErrorDiagnosticAllowlist(t *testing.T) {
	for _, code := range []newAPIWalletDiagnostic{newAPIKeyQuotaOnly, newAPINoAccountBalanceWithKey} {
		known := fmt.Errorf("outer: %w", code)
		require.ErrorIs(t, known, errWallet)
		require.Equal(t, string(code), walletErrorDiagnostic(known))
	}
	require.Empty(t, walletErrorDiagnostic(newAPIWalletDiagnostic("private-upstream-detail")))
	require.Empty(t, walletErrorDiagnostic(errWallet))
}

func TestNewAPIWalletDiagnosticResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			_, _ = io.WriteString(w, `{}`)
		case "/api/usage/token/":
			_, _ = io.WriteString(w, `{"code":true,"data":{"object":"token_usage","total_available":17}}`)
		case "/user/balance":
			_, _ = io.WriteString(w, `<html/>`)
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	p, _ := testPlugin(t)
	p.client = server.Client()
	c := DefaultConfig()
	c.Name, c.Adapter, c.BaseURL = "fixture", "new_api", server.URL
	created := call(t, p, http.MethodPost, "/sites", SaveRequest{Config: c, Secrets: &Secrets{Key: "private-wallet-key"}})
	site := readPricingResponse[Site](t, created)
	response := call(t, p, http.MethodPost, fmt.Sprintf("/sites/%d/query", site.ID), nil)
	got := readPricingResponse[Observation](t, response)
	require.Equal(t, http.StatusOK, got.HTTPStatus)
	require.Equal(t, Unknown, got.Outcome)
	require.Equal(t, "SITE_BALANCE_UNAVAILABLE", got.Code)
	require.Equal(t, string(newAPIKeyQuotaOnly), got.Diagnostic)
	require.Empty(t, got.Balances)
}
