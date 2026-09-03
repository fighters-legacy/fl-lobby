// SPDX-FileCopyrightText: 2026 MKZ Systems LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package lobby

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestService(t *testing.T, tweak func(*Config)) (*Service, *fakeClock) {
	t.Helper()
	cfg := DefaultConfig()
	// The rate limiter is not what most of these tests are about, so give them room; the
	// rate-limit tests tighten it deliberately.
	cfg.WriteRate = 1000
	cfg.WriteBurst = 1000
	cfg.ReadRate = 1000
	cfg.ReadBurst = 1000
	if tweak != nil {
		tweak(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config is invalid: %v", err)
	}
	clk := newFakeClock()
	return New(cfg, slog.New(slog.DiscardHandler), clk.Now), clk
}

func do(t *testing.T, svc *Service, method, target, remote, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	req.RemoteAddr = remote
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)
	return w
}

// The exact body the C++ registration client builds (LobbyRegistration::buildBody). If this stops
// being accepted, every dedicated server silently vanishes from every browser.
const cppClientBody = `{"name":"My Server","port":4778,"players":3,"max_players":16,` +
	`"mode":"builtin:tdm","mission":"fjord","visibility":"public"}`

func TestRegisterAcceptsTheCppClientBody(t *testing.T) {
	svc, _ := newTestService(t, nil)

	w := do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", cppClientBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("first POST: status=%d body=%s, want 201", w.Code, w.Body)
	}
	w = do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51001", cppClientBody)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat POST: status=%d, want 200", w.Code)
	}

	list := getList(t, svc)
	if len(list) != 1 {
		t.Fatalf("listed %d servers, want 1", len(list))
	}
	got := list[0]
	// The source port changed between the two POSTs; the entry must key on the ADVERTISED game
	// port, not the ephemeral one the request came from.
	if got.Host != "203.0.113.7" || got.Port != 4778 {
		t.Fatalf("listed %s:%d, want 203.0.113.7:4778", got.Host, got.Port)
	}
	if got.Name != "My Server" || got.Mode != "builtin:tdm" || got.Mission != "fjord" {
		t.Fatalf("fields did not round-trip: %+v", got)
	}
	if got.Players != 3 || got.MaxPlayers != 16 {
		t.Fatalf("counts did not round-trip: players=%d max=%d", got.Players, got.MaxPlayers)
	}
}

func TestHostIsTheObservedAddressNotAClaimedOne(t *testing.T) {
	svc, _ := newTestService(t, nil)

	// A registrant naming someone else's address must not be able to list a server there: with no
	// auth in v1, the observed source address is the ONLY thing making a listing trustworthy.
	body := `{"name":"evil","port":4778,"host":"192.0.2.66","address":"192.0.2.66","visibility":"public"}`
	if w := do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", body); w.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201", w.Code)
	}
	if got := getList(t, svc)[0].Host; got != "203.0.113.7" {
		t.Fatalf("listed host=%s, want the observed 203.0.113.7 -- a claimed host was believed", got)
	}
}

func TestXForwardedForIsIgnoredWithoutATrustedProxy(t *testing.T) {
	svc, _ := newTestService(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/servers", strings.NewReader(cppClientBody))
	req.RemoteAddr = "203.0.113.7:51000"
	req.Header.Set("X-Forwarded-For", "192.0.2.66")
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)

	if got := getList(t, svc)[0].Host; got != "203.0.113.7" {
		t.Fatalf("listed host=%s, want 203.0.113.7: an untrusted XFF header was believed", got)
	}
}

func TestXForwardedForIsUsedBehindATrustedProxy(t *testing.T) {
	svc, _ := newTestService(t, func(c *Config) {
		c.TrustedProxies = mustPrefixes(t, "10.0.0.0/8")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/servers", strings.NewReader(cppClientBody))
	req.RemoteAddr = "10.1.2.3:51000"
	// A spoofed hop on the left, the proxy's own appended value on the right. Walking from the
	// right is what discards the attacker-controlled entry.
	req.Header.Set("X-Forwarded-For", "192.0.2.66, 203.0.113.7")
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)

	if got := getList(t, svc)[0].Host; got != "203.0.113.7" {
		t.Fatalf("listed host=%s, want 203.0.113.7", got)
	}
}

func TestRegisterRejections(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"no port", `{"name":"x","visibility":"public"}`, http.StatusBadRequest},
		{"zero port", `{"name":"x","port":0}`, http.StatusBadRequest},
		{"port out of range", `{"name":"x","port":70000}`, http.StatusBadRequest},
		{"private server", `{"name":"x","port":4778,"visibility":"private"}`, http.StatusBadRequest},
		{"malformed json", `{"name":`, http.StatusBadRequest},
		{"unknown fields are tolerated", `{"port":4778,"future_field":"x","visibility":"public"}`, http.StatusCreated},
		{"absent visibility is public", `{"port":4778}`, http.StatusCreated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(t, nil)
			if w := do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", tc.body); w.Code != tc.want {
				t.Fatalf("status=%d body=%s, want %d", w.Code, w.Body, tc.want)
			}
		})
	}
}

func TestEntryExpiresOnTheContractTTL(t *testing.T) {
	svc, clk := newTestService(t, nil)
	do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", cppClientBody)

	// Default heartbeat 30s x 2.5 = 75s.
	clk.Advance(74 * time.Second)
	if got := len(getList(t, svc)); got != 1 {
		t.Fatalf("at 74s: listed %d, want 1", got)
	}
	clk.Advance(2 * time.Second)
	if got := len(getList(t, svc)); got != 0 {
		t.Fatalf("at 76s: listed %d, want 0", got)
	}
}

func TestSelfReportedHeartbeatSetsTheTTL(t *testing.T) {
	svc, clk := newTestService(t, nil)
	// A server heartbeating on the maximum interval would be expired by a lobby that assumed 30s.
	body := `{"name":"slow","port":4778,"visibility":"public","heartbeat_s":300}`
	do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", body)

	clk.Advance(100 * time.Second)
	if got := len(getList(t, svc)); got != 1 {
		t.Fatalf("listed %d at 100s, want 1: a self-reported 300s heartbeat means a 750s TTL", got)
	}
}

func TestSelfReportedHeartbeatIsClamped(t *testing.T) {
	svc, clk := newTestService(t, nil)
	// A registrant asking for a year of TTL does not get one: the clamp mirrors the range the C++
	// client can actually be configured with.
	body := `{"name":"greedy","port":4778,"visibility":"public","heartbeat_s":999999}`
	do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", body)

	clk.Advance(MaxHeartbeatSeconds*time.Second*3 + time.Second) // past 2.5 x the clamped max
	if got := len(getList(t, svc)); got != 0 {
		t.Fatalf("listed %d, want 0: the heartbeat was not clamped", got)
	}
}

func TestPassworded(t *testing.T) {
	svc, _ := newTestService(t, nil)

	// Absent (what the current C++ client sends) means false, not missing.
	do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", cppClientBody)
	if getList(t, svc)[0].Passworded {
		t.Fatal("passworded=true with no field sent, want false")
	}

	svc2, _ := newTestService(t, nil)
	body := `{"name":"locked","port":4778,"visibility":"public","passworded":true}`
	do(t, svc2, http.MethodPost, "/v1/servers", "203.0.113.7:51000", body)
	if !getList(t, svc2)[0].Passworded {
		t.Fatal("passworded=false after it was sent as true")
	}
}

func TestDeregisterAcceptsBodyAndQuery(t *testing.T) {
	for _, tc := range []struct {
		name, target, body string
	}{
		// The contract document specifies the body; #999's checklist writes it as a query
		// parameter. Both work, so neither spelling is a silent no-op.
		{"body", "/v1/servers", `{"port":4778}`},
		{"query", "/v1/servers?port=4778", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(t, nil)
			do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", cppClientBody)

			w := do(t, svc, http.MethodDelete, tc.target, "203.0.113.7:51002", tc.body)
			if w.Code != http.StatusNoContent {
				t.Fatalf("status=%d, want 204", w.Code)
			}
			if got := len(getList(t, svc)); got != 0 {
				t.Fatalf("listed %d after DELETE, want 0", got)
			}
		})
	}
}

func TestDeregisterOfAMissingEntryIsNotAnError(t *testing.T) {
	svc, _ := newTestService(t, nil)
	// A DELETE that races the TTL is the normal shutdown case, not a fault.
	if w := do(t, svc, http.MethodDelete, "/v1/servers", "203.0.113.7:51000", `{"port":4778}`); w.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", w.Code)
	}
}

func TestDeregisterCannotTouchAnotherHost(t *testing.T) {
	svc, _ := newTestService(t, nil)
	do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", cppClientBody)

	// The entry is keyed on the source address, so a DELETE from elsewhere matches nothing. Without
	// this, anyone could unlist any server by guessing its port.
	do(t, svc, http.MethodDelete, "/v1/servers", "192.0.2.66:40000", `{"port":4778}`)
	if got := len(getList(t, svc)); got != 1 {
		t.Fatalf("listed %d, want 1: another host deregistered this server", got)
	}
}

func TestListIsAnEmptyArrayNotNull(t *testing.T) {
	svc, _ := newTestService(t, nil)
	w := do(t, svc, http.MethodGet, "/v1/servers", "198.51.100.9:40000", "")
	// `null` would be valid JSON and would not parse as an array. The client's reader wants an
	// array; give it one even when empty.
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Fatalf("body=%q, want []", body)
	}
}

func TestListRespectsTheEntryCap(t *testing.T) {
	svc, _ := newTestService(t, func(c *Config) {
		c.MaxEntries = MaxListEntries
		c.MaxPerHost = MaxListEntries
	})
	for i := 0; i < MaxListEntries+50; i++ {
		body := `{"name":"s","port":` + itoa(1024+i%60000) + `,"visibility":"public"}`
		host := "198.51." + itoa(i/250) + "." + itoa(i%250+1) + ":40000"
		do(t, svc, http.MethodPost, "/v1/servers", host, body)
	}
	if got := len(getList(t, svc)); got > MaxListEntries {
		t.Fatalf("listed %d, want <= %d", got, MaxListEntries)
	}
}

func TestListStaysUnderTheResponseCap(t *testing.T) {
	svc, _ := newTestService(t, func(c *Config) {
		c.MaxEntries = MaxListEntries
		c.MaxPerHost = MaxListEntries
	})
	// Maximum-length strings in every field, so the rows are as fat as the contract allows. The
	// client stops reading at 1 MiB and would be left parsing a severed array.
	big := strings.Repeat("w", MaxStringBytes)
	for i := 0; i < MaxListEntries; i++ {
		body := `{"name":"` + big + `","mode":"` + big + `","mission":"` + big +
			`","port":` + itoa(1024+i%60000) + `,"visibility":"public"}`
		host := "198.51." + itoa(i/250) + "." + itoa(i%250+1) + ":40000"
		do(t, svc, http.MethodPost, "/v1/servers", host, body)
	}
	w := do(t, svc, http.MethodGet, "/v1/servers", "203.0.113.99:40000", "")
	if n := w.Body.Len(); n > MaxListBytes {
		t.Fatalf("response is %d bytes, want <= %d", n, MaxListBytes)
	}
	// Whatever survived the cap must still be a parseable array.
	var out []serverJSON
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("truncated response is not valid JSON: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the cap dropped every row")
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	svc, _ := newTestService(t, nil)
	body := `{"name":"` + strings.Repeat("x", MaxRequestBytes*2) + `","port":4778}`
	if w := do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", body); w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
}

func TestWriteRateLimit(t *testing.T) {
	svc, clk := newTestService(t, func(c *Config) {
		c.WriteRate = 1
		c.WriteBurst = 3
	})
	for i := 0; i < 3; i++ {
		if w := do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", cppClientBody); w.Code/100 != 2 {
			t.Fatalf("request %d: status=%d, want 2xx within the burst", i, w.Code)
		}
	}
	w := do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", cppClientBody)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429 once the burst is spent", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("a 429 with no Retry-After leaves a client guessing")
	}

	// Another address is unaffected -- the limit is per source, not global.
	if w := do(t, svc, http.MethodPost, "/v1/servers", "198.51.100.5:51000", cppClientBody); w.Code/100 != 2 {
		t.Fatalf("other host: status=%d, want 2xx", w.Code)
	}

	clk.Advance(2 * time.Second)
	if w := do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", cppClientBody); w.Code/100 != 2 {
		t.Fatalf("after refill: status=%d, want 2xx", w.Code)
	}
}

func TestPerHostEntryCap(t *testing.T) {
	svc, _ := newTestService(t, func(c *Config) { c.MaxPerHost = 2 })
	for i, want := range []int{http.StatusCreated, http.StatusCreated, http.StatusTooManyRequests} {
		body := `{"name":"s","port":` + itoa(4778+i) + `,"visibility":"public"}`
		if w := do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", body); w.Code != want {
			t.Fatalf("registration %d: status=%d, want %d", i, w.Code, want)
		}
	}
}

func TestLobbyFullIsA503(t *testing.T) {
	svc, _ := newTestService(t, func(c *Config) { c.MaxEntries = 1 })
	do(t, svc, http.MethodPost, "/v1/servers", "203.0.113.7:51000", cppClientBody)
	// The registrant did nothing wrong, so this is a retry-later, not a client error.
	if w := do(t, svc, http.MethodPost, "/v1/servers", "198.51.100.5:51000", cppClientBody); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", w.Code)
	}
}

func TestMethodAndPathRouting(t *testing.T) {
	svc, _ := newTestService(t, nil)
	for _, tc := range []struct {
		method, target string
		want           int
	}{
		{http.MethodPut, "/v1/servers", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v2/servers", http.StatusNotFound},
		{http.MethodGet, "/servers", http.StatusNotFound},
		{http.MethodGet, "/healthz", http.StatusOK},
	} {
		if w := do(t, svc, tc.method, tc.target, "203.0.113.7:51000", ""); w.Code != tc.want {
			t.Errorf("%s %s: status=%d, want %d", tc.method, tc.target, w.Code, tc.want)
		}
	}
}

func getList(t *testing.T, svc *Service) []serverJSON {
	t.Helper()
	w := do(t, svc, http.MethodGet, "/v1/servers", "198.51.100.9:40000", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/servers: status=%d", w.Code)
	}
	var out []serverJSON
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /v1/servers returned unparseable JSON: %v", err)
	}
	return out
}
