package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNewAPIAccountParsers(t *testing.T) {
	usd := defaultNewAPISettings()
	cny := newAPISettings{"CNY", decimal.NewFromInt(500000), decimal.RequireFromString("7.3")}
	tokens := newAPISettings{"TOKENS", usd.factor, usd.rate}
	for _, tt := range []struct {
		name, route, body, id, unit, amount string
		settings                            newAPISettings
		fail                                bool
	}{
		{"embedded display ignores corrupt key counters", "token", `{"code":true,"data":{"user_balance_display":"$1,264.28","total_available":"broken","total_granted":-1}}`, "", "USD", "1264.28", usd, false},
		{"embedded raw CNY", "token", `{"data":{"user_balance":500000,"currency":"USD"}}`, "", "CNY", "7.3", cny, false},
		{"embedded null display falls through", "token", `{"data":{"user_balance_display":null,"user_balance":500000}}`, "", "USD", "1", usd, false},
		{"embedded malformed preferred cannot fall through", "token", `{"data":{"user_balance_display":"bad","user_balance":500000}}`, "", "", "", usd, true},
		{"key quota is not wallet", "token", `{"data":{"quota":500000,"total_available":123,"total_used":4,"total_granted":127,"unlimited_quota":true}}`, "", "", "", usd, true},
		{"self raw without lifetime total", "self", `{"success":true,"data":{"id":42,"quota":632140000,"used_quota":"ignored"}}`, "42", "USD", "1264.28", usd, false},
		{"self explicit display currency", "self", `{"data":{"id":"42","quota":"12.30","currency":"RMB"}}`, "42", "CNY", "12.3", usd, false},
		{"self tokens", "self", `{"data":{"quota":500000}}`, "", "TOKENS", "500000", tokens, false},
		{"self debt", "self", `{"data":{"quota":-500000}}`, "", "USD", "-1", usd, false},
		{"self mismatch", "self", `{"data":{"id":43,"quota":500000}}`, "42", "", "", usd, true},
		{"self missing id", "self", `{"data":{"quota":500000}}`, "42", "", "", usd, true},
		{"self precise large id", "self", `{"data":{"id":9007199254740993,"quota":500000}}`, "9007199254740993", "USD", "1", usd, false},
		{"self float rounding must not match", "self", `{"data":{"id":9007199254740992,"quota":500000}}`, "9007199254740993", "", "", usd, true},
		{"self fractional id", "self", `{"data":{"id":42.0000000001,"quota":500000}}`, "42", "", "", usd, true},
		{"self overflow id", "self", `{"data":{"id":9223372036854775808,"quota":500000}}`, "42", "", "", usd, true},
		{"root display", "compat", `{"total_balance":"1264.28"}`, "", "USD", "1264.28", usd, false},
		{"nested remaining", "compat", `{"data":{"remaining":"12.1234567890"}}`, "", "USD", "12.123456789", usd, false},
		{"nested quota", "compat", `{"data":{"quota":500000,"used_quota":500000}}`, "", "USD", "1", usd, false},
		{"nested quota determines raw conversion", "compat", `{"data":{"balance":500000,"quota":1}}`, "", "USD", "1", usd, false},
		{"list no currency display", "compat", `{"balance_infos":[{"total_balance":1264.28}]}`, "", "USD", "1264.28", usd, false},
		{"list preferred currency", "compat", `{"balance_infos":[{"currency":"USD","total_balance":2},{"currency":"CNY","balance":3}]}`, "", "CNY", "3", cny, false},
		{"list fallback first", "compat", `{"balance_infos":[{"currency":"CNY","remaining":3}]}`, "", "CNY", "3", usd, false},
		{"nested list wins", "compat", `{"balance_infos":[{"balance":2}],"data":{"balance_infos":[{"balance":3}]}}`, "", "USD", "3", usd, false},
		{"empty nested list retains root", "compat", `{"balance_infos":[{"balance":2}],"data":{"balance_infos":[]}}`, "", "USD", "2", usd, false},
		{"root quota forbidden", "compat", `{"quota":500000}`, "", "", "", usd, true},
		{"key allowances forbidden", "compat", `{"data":{"total_available":123,"total_used":4,"total_granted":127}}`, "", "", "", usd, true},
		{"preferred invalid", "compat", `{"total_balance":"bad","balance":42}`, "", "", "", usd, true},
		{"negative used", "compat", `{"balance":1,"used":-1}`, "", "", "", usd, true},
		{"invalid total", "compat", `{"balance":1,"total":"bad"}`, "", "", "", usd, true},
		{"auth code", "compat", `{"code":false,"balance":42}`, "", "", "", usd, true},
		{"auth success", "self", `{"success":false,"data":{"quota":500000}}`, "", "", "", usd, true},
		{"HTML", "compat", `<html>login</html>`, "", "", "", usd, true},
		{"nonfinite", "compat", `{"balance":"NaN"}`, "", "", "", usd, true},
		{"overflow", "compat", `{"balance":"1e999999"}`, "", "", "", usd, true},
		{"precision bound", "compat", `{"balance":"1.12345678901"}`, "", "", "", usd, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var wallets []Balance
			var err error
			switch tt.route {
			case "token":
				var present bool
				wallets, present, err = newAPIEmbedded([]byte(tt.body), tt.settings)
				if !present {
					err = errWallet
				}
			case "self":
				wallets, err = newAPISelf([]byte(tt.body), tt.settings, tt.id)
			default:
				wallets, err = newAPICompatible([]byte(tt.body), tt.settings)
			}
			if tt.fail {
				require.Error(t, err)
				require.Empty(t, wallets)
				return
			}
			require.NoError(t, err)
			require.Len(t, wallets, 1)
			require.Equal(t, tt.unit, wallets[0].Unit)
			require.Equal(t, tt.amount, wallets[0].Available)
			if tt.route == "self" || tt.route == "token" {
				require.Nil(t, wallets[0].Used)
				require.Nil(t, wallets[0].Total)
			}
		})
	}
}

func TestNewAPIUserQuotaUsesSiteDisplaySettings(t *testing.T) {
	for _, tt := range []struct {
		name, settings, quota string
		want                  Balance
	}{
		{"USD large integer", `"quota_display_type":"USD","quota_per_unit":1000000,"usd_exchange_rate":7.3`, "9007199254740993",
			Balance{Unit: "USD", Available: "9007199254.740993", Basis: "token.data.user_quota.total_available:raw:quota_per_unit=1000000"}},
		{"CNY", `"quota_display_type":"CNY","quota_per_unit":1000000,"usd_exchange_rate":7.3`, "1234567",
			Balance{Unit: "CNY", Available: "9.0123391", Basis: "token.data.user_quota.total_available:raw:quota_per_unit=1000000:usd_exchange_rate=7.3"}},
		{"TOKENS large integer", `"quota_display_type":"TOKENS","quota_per_unit":1000000,"usd_exchange_rate":7.3`, "9223372036854775807",
			Balance{Unit: "TOKENS", Available: "9223372036854775807", Basis: "token.data.user_quota.total_available:raw"}},
		{"zero", `"quota_display_type":"USD"`, "0",
			Balance{Unit: "USD", Available: "0", Basis: "token.data.user_quota.total_available:raw:quota_per_unit=500000"}},
		{"debt", `"quota_display_type":"USD"`, "-500000",
			Balance{Unit: "USD", Available: "-1", Basis: "token.data.user_quota.total_available:raw:quota_per_unit=500000"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/status":
					fmt.Fprintf(w, `{"success":true,"data":{%s}}`, tt.settings)
				case "/api/usage/token/":
					fmt.Fprintf(w, `{"code":true,"data":{"object":"token_usage","currency":"USD","total_available":17,"total_used":41,"user_quota":{"total_available":%s,"total_used":500000}}}`, tt.quota)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			wallets, status, err := queryNewAPI(context.Background(), server.Client(), server.URL, Secrets{Key: "fixture"})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, status)
			require.Equal(t, []Balance{tt.want}, wallets)
		})
	}
}

func TestNewAPIUserQuotaRejectsInvalidRawAmounts(t *testing.T) {
	for _, raw := range []string{`null`, `false`, `[]`, `"123"`, `"bad"`, `1.25`, `9223372036854775808`} {
		t.Run(raw, func(t *testing.T) {
			body := fmt.Sprintf(`{"code":true,"data":{"total_available":17,"user_quota":{"total_available":%s}}}`, raw)
			wallets, present, err := newAPIEmbedded([]byte(body), defaultNewAPISettings())
			require.True(t, present)
			require.Empty(t, wallets)
			require.ErrorIs(t, err, errWallet)
		})
	}
}

func TestNewAPIKeyWalletDiagnostics(t *testing.T) {
	for _, tt := range []struct {
		name, tokenBody, compatBody, walletToken string
		compatStatus                             int
		wantErr                                  error
		wantDiagnostic                           string
	}{
		{"key quota only", `{"code":true,"data":{"total_available":17}}`, `<html/>`, "", 200, errWallet, "NEW_API_KEY_QUOTA_ONLY"},
		{"non JSON compatibility response", `{}`, `<html/>`, "", 200, errWallet, "NEW_API_NO_ACCOUNT_BALANCE_WITH_KEY"},
		{"missing account fields", `{}`, `{"data":{}}`, "", 200, errWallet, "NEW_API_NO_ACCOUNT_BALANCE_WITH_KEY"},
		{"partial account data", `{"data":{"total_available":17,"user_quota":{"total_used":42}}}`, `{}`, "", 200, errWallet, "NEW_API_NO_ACCOUNT_BALANCE_WITH_KEY"},
		{"account token configured", `{}`, `<html/>`, "fixture-wallet", 200, errWallet, ""},
		{"compatibility route missing", `{"data":{"total_available":17}}`, `<html/>`, "", 404, nil, ""},
		{"explicit rejection", `{"data":{"total_available":17}}`, `{"success":false}`, "", 200, errAuth, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/status":
					fmt.Fprint(w, `{}`)
				case "/api/usage/token/":
					fmt.Fprint(w, tt.tokenBody)
				case "/api/user/self":
					fmt.Fprint(w, `{}`)
				case "/user/balance":
					w.WriteHeader(tt.compatStatus)
					fmt.Fprint(w, tt.compatBody)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			wallets, status, err := queryNewAPI(context.Background(), server.Client(), server.URL, Secrets{Key: "fixture", WalletToken: tt.walletToken})
			require.Empty(t, wallets)
			require.Equal(t, tt.compatStatus, status)
			require.ErrorIs(t, err, tt.wantErr)
			if tt.wantDiagnostic == "" {
				require.Equal(t, tt.wantErr, err)
				return
			}
			var diagnostic interface{ WalletDiagnostic() string }
			require.ErrorAs(t, err, &diagnostic)
			require.Equal(t, tt.wantDiagnostic, diagnostic.WalletDiagnostic())
			require.NotErrorIs(t, err, errAuth)
			require.EqualError(t, err, "SITE_BALANCE_UNAVAILABLE")
		})
	}
}

func TestNewAPIDiscoveryAndCredentials(t *testing.T) {
	for _, tt := range []struct {
		name, tokenBody, selfBody, compatBody string
		tokenStatus, selfStatus               int
		key, wallet, id                       string
		paths                                 []string
		outcome, amount                       string
	}{
		{"embedded display takes precedence", `{"data":{"user_balance_display":3,"user_balance":500000,"user_quota":{"total_available":1000000}}}`, "", "", 200, 200, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/"}, Healthy, "3"},
		{"embedded raw takes precedence", `{"data":{"user_balance":500000,"user_quota":{"total_available":1000000}}}`, "", "", 200, 200, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/"}, Healthy, "1"},
		{"official account quota stops discovery", `{"code":true,"data":{"total_available":17,"user_quota":{"total_available":13579246}}}`, "", "", 200, 200, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/"}, Healthy, "27.158492"},
		{"self", `{"data":{"total_available":999}}`, `{"data":{"id":42,"quota":1000000}}`, "", 200, 200, "api-key", "user-token", "0042", []string{"/api/status", "/api/usage/token/", "/api/user/self"}, Healthy, "2"},
		{"compat fallback", `{}`, `<html/>`, `{"balance":3}`, 200, 200, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/", "/api/user/self", "/user/balance"}, Healthy, "3"},
		{"key quota allows compat fallback", `{"code":true,"data":{"total_available":17}}`, "", `{"balance":3}`, 200, 200, "api-key", "", "", []string{"/api/status", "/api/usage/token/", "/user/balance"}, Healthy, "3"},
		{"missing token route", "", "", `{"remaining":4}`, 404, 200, "api-key", "", "", []string{"/api/status", "/api/usage/token/", "/user/balance"}, Healthy, "4"},
		{"no API key self works", "", `{"data":{"id":42,"quota":1000000}}`, "", 200, 200, "", "user-token", "42", []string{"/api/status", "/api/user/self"}, Healthy, "2"},
		{"mismatch blocks fallback", `{}`, `{"data":{"id":43,"quota":1000000}}`, `{"balance":9}`, 200, 200, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/", "/api/user/self"}, Authentication, ""},
		{"self 401 blocks fallback", `{}`, `{}`, `{"balance":9}`, 200, 401, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/", "/api/user/self"}, Authentication, ""},
		{"self 429 blocks fallback", `{}`, `{}`, `{"balance":9}`, 200, 429, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/", "/api/user/self"}, RateLimited, ""},
		{"self 503 may use compatible wallet", `{}`, `{}`, `{"balance":9}`, 200, 503, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/", "/api/user/self", "/user/balance"}, Healthy, "9"},
		{"token 403", `{"data":{"user_quota":{"total_available":1000000}}}`, `{}`, `{}`, 403, 200, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/"}, Authentication, ""},
		{"token outage", `{}`, `{}`, `{}`, 503, 200, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/"}, Unreachable, ""},
		{"false token code", `{"code":false,"data":{"user_quota":{"total_available":1000000}}}`, `{}`, `{}`, 200, 200, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/"}, Authentication, ""},
		{"invalid preferred cannot fallback", `{"data":{"user_balance_display":"bad","user_quota":{"total_available":1000000}}}`, `{}`, `{"balance":9}`, 200, 200, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/"}, Unknown, ""},
		{"invalid official account quota cannot fallback", `{"code":true,"data":{"total_available":17,"user_quota":{"total_available":false}}}`, `{}`, `{"balance":9}`, 200, 200, "api-key", "user-token", "42", []string{"/api/status", "/api/usage/token/"}, Unknown, ""},
		{"only key fields", `{"data":{"total_available":999,"quota":500000}}`, "", `<html/>`, 200, 200, "api-key", "", "", []string{"/api/status", "/api/usage/token/", "/user/balance"}, Unknown, ""},
		{"invalid configured ID no requests", `{}`, `{}`, `{}`, 200, 200, "api-key", "user-token", "42.5", nil, Configuration, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := strings.TrimPrefix(r.URL.Path, "/prefix")
				paths = append(paths, path)
				key, user := "Bearer api-key", ""
				if path == "/api/status" {
					key = ""
				}
				if path == "/api/user/self" {
					key, user = "Bearer user-token", "42"
				}
				require.Equal(t, key, r.Header.Get("Authorization"))
				require.Equal(t, user, r.Header.Get("New-Api-User"))
				switch path {
				case "/api/status":
					fmt.Fprint(w, `{"success":true,"data":{"quota_display_type":"USD"}}`)
				case "/api/usage/token/":
					w.WriteHeader(tt.tokenStatus)
					fmt.Fprint(w, tt.tokenBody)
				case "/api/user/self":
					w.WriteHeader(tt.selfStatus)
					fmt.Fprint(w, tt.selfBody)
				case "/user/balance":
					fmt.Fprint(w, tt.compatBody)
				default:
					t.Errorf("unexpected path %s", path)
				}
			}))
			defer server.Close()
			p := &Plugin{client: server.Client()}
			cfg := DefaultConfig()
			cfg.Adapter = "new_api"
			cfg.BaseURL = server.URL + "/prefix/v1/"
			o := p.observe(context.Background(), cfg, Secrets{Key: tt.key, WalletToken: tt.wallet, WalletUserID: tt.id}, "balance")
			require.Equal(t, tt.paths, paths)
			require.Equal(t, tt.outcome, o.Outcome)
			if tt.amount != "" {
				require.Len(t, o.Balances, 1)
				require.Equal(t, tt.amount, o.Balances[0].Available)
			} else {
				require.Empty(t, o.Balances)
			}
		})
	}
}

func TestNewAPISettingsAndTimeout(t *testing.T) {
	for _, tt := range []struct{ body, unit, amount string }{
		{`{"success":true,"data":{"quota_display_type":"CNY","quota_per_unit":100,"usd_exchange_rate":7.3}}`, "CNY", "36.5"},
		{`{"success":true,"data":{"quota_display_type":"TOKENS"}}`, "TOKENS", "500"},
		{`{"success":true,"data":{"quota_display_type":"USD","quota_per_unit":0}}`, "USD", "0.001"},
		{`{"success":false,"data":{"quota_display_type":"CNY","quota_per_unit":1}}`, "USD", "0.001"},
		{`{"success":true,"data":{"quota_per_unit":1}}`, "USD", "0.001"},
		{`<html/>`, "USD", "0.001"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tt.body) }))
		s := newAPIDisplaySettings(context.Background(), server.Client(), server.URL)
		server.Close()
		n, err := s.amount(gjson.Parse(`500`), false)
		require.NoError(t, err)
		require.Equal(t, tt.unit, s.unit)
		require.Equal(t, tt.amount, n)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			fmt.Fprint(w, `{}`)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := queryNewAPI(ctx, server.Client(), server.URL, Secrets{WalletToken: "token"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestNewAPIInvalidIdentityRejectedOnSave(t *testing.T) {
	p, _ := testPlugin(t)
	s := addSite(t, p, "https://example.com")
	cfg := s.Config
	cfg.Adapter = "new_api"
	for _, id := range []string{"-1", "0", "1.1", "9223372036854775808", "42\r\nInjected: yes"} {
		require.Equal(t, 400, call(t, p, "PUT", "/sites/1", SaveRequest{Config: cfg, Version: s.ConfigVersion, Secrets: &Secrets{WalletUserID: id}}).Code)
	}
}
