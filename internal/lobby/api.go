// SPDX-FileCopyrightText: 2026 MKZ Systems LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lobby implements the fl-lobby v1 REST service: register/heartbeat, deregister and list
// for Fighters Legacy dedicated servers.
//
// The wire contract is docs/server-ops/lobby-api.md in the fighters-legacy repo, and that document
// is the source of truth -- the C++ halves (engine/net/LobbyRegistration, engine/net/LobbyListClient)
// already speak it. Where this service is deliberately more tolerant than the document requires,
// the reason is on the code.
package lobby

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Service is the HTTP surface. Build one with New and mount it with Handler.
type Service struct {
	cfg   Config
	store *Store
	log   *slog.Logger

	writes *limiter
	reads  *limiter

	now func() time.Time
}

// New builds a Service. now may be nil, in which case the wall clock is used; the tests inject one
// so TTL and rate-limit behaviour can be exercised without sleeping.
func New(cfg Config, log *slog.Logger, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	// A bucket is worth keeping only while it could still refuse something. Once enough time has
	// passed for a full refill it is indistinguishable from a fresh one, so that -- with a floor --
	// is the idle TTL.
	idle := func(rate float64, burst int) time.Duration {
		d := time.Duration(float64(burst)/rate) * time.Second
		if d < time.Minute {
			d = time.Minute
		}
		return d
	}
	return &Service{
		cfg:    cfg,
		store:  NewStore(cfg.MaxEntries, cfg.MaxPerHost, now),
		log:    log,
		writes: newLimiter(cfg.WriteRate, cfg.WriteBurst, idle(cfg.WriteRate, cfg.WriteBurst), now),
		reads:  newLimiter(cfg.ReadRate, cfg.ReadBurst, idle(cfg.ReadRate, cfg.ReadBurst), now),
		now:    now,
	}
}

// Handler returns the routed HTTP handler.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	// Method-qualified patterns, so an unroutable method answers 405 from the mux rather than
	// falling into a handler that has to check.
	mux.HandleFunc("POST /v1/servers", s.handleRegister)
	mux.HandleFunc("DELETE /v1/servers", s.handleDeregister)
	mux.HandleFunc("GET /v1/servers", s.handleList)
	// Not part of the versioned contract: a liveness probe for a container runtime or a reverse
	// proxy. Kept outside /v1 precisely so it is not something the frozen API has to carry.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return mux
}

// Store exposes the entry table (the smoke test and the prune loop use it).
func (s *Service) Store() *Store { return s.store }

// Sweep drops expired entries and idle rate-limit buckets. Run it on a ticker.
func (s *Service) Sweep() {
	s.store.Prune()
	s.writes.sweep()
	s.reads.sweep()
}

type registerRequest struct {
	Name        string `json:"name"`
	Port        *int   `json:"port"`
	Players     *int   `json:"players"`
	MaxPlayers  *int   `json:"max_players"`
	MaxPlayers2 *int   `json:"maxPlayers"`
	Mode        string `json:"mode"`
	Mission     string `json:"mission"`
	Visibility  string `json:"visibility"`

	// Neither of the next two is sent by the current C++ client, and both are optional. See
	// docs/contract-notes.md for why they exist and what happens while nothing sends them.
	Passworded *bool `json:"passworded"`
	HeartbeatS *int  `json:"heartbeat_s"`
}

type deregisterRequest struct {
	Port *int `json:"port"`
}

// serverJSON is the listed shape. The client accepts `address` for `host` and `maxPlayers` for
// `max_players`, but it is the lobby's job to emit the canonical spelling of each, not to pick.
type serverJSON struct {
	Name       string `json:"name"`
	Host       string `json:"host"`
	Port       uint16 `json:"port"`
	Mode       string `json:"mode"`
	Mission    string `json:"mission"`
	Players    int    `json:"players"`
	MaxPlayers int    `json:"max_players"`
	Passworded bool   `json:"passworded"`
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	addr, ok := clientIP(r, s.cfg.TrustedProxies)
	if !ok {
		writeError(w, http.StatusBadRequest, "could not determine source address")
		return
	}
	host := addr.String()
	if !s.writes.allow(host) {
		s.tooManyRequests(w)
		return
	}

	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Port == nil {
		writeError(w, http.StatusBadRequest, "port is required")
		return
	}
	if *req.Port < 1 || *req.Port > 65535 {
		writeError(w, http.StatusBadRequest, "port out of range")
		return
	}
	// The contract says a private server never POSTs. Rather than silently listing one that did,
	// say so -- a server whose operator believes it is private and finds it listed is the worse
	// outcome of the two.
	if req.Visibility != "" && req.Visibility != "public" {
		writeError(w, http.StatusBadRequest, "only public servers may register")
		return
	}

	maxPlayers := req.MaxPlayers
	if maxPlayers == nil {
		maxPlayers = req.MaxPlayers2
	}

	entry := Entry{
		Name:       req.Name,
		Host:       host,
		Port:       uint16(*req.Port),
		Mode:       req.Mode,
		Mission:    req.Mission,
		Players:    clampCount(req.Players),
		MaxPlayers: clampCount(maxPlayers),
		Passworded: req.Passworded != nil && *req.Passworded,
	}

	heartbeat := s.cfg.Heartbeat
	if req.HeartbeatS != nil {
		secs := min(max(*req.HeartbeatS, MinHeartbeatSeconds), MaxHeartbeatSeconds)
		heartbeat = time.Duration(secs) * time.Second
	}
	ttl := s.cfg.ttlFor(heartbeat)

	created, err := s.store.Upsert(entry, ttl)
	switch {
	case errors.Is(err, ErrLobbyFull):
		// 503, not 4xx: the registrant did nothing wrong and should retry. The C++ client backs
		// off on any non-2xx, so it will.
		writeError(w, http.StatusServiceUnavailable, "lobby is at capacity")
		return
	case errors.Is(err, ErrHostFull):
		writeError(w, http.StatusTooManyRequests, "too many servers registered from this address")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not register")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.log.Info("registered", "host", host, "port", entry.Port, "name", entry.Name, "ttl", ttl)
	}
	writeJSON(w, status, map[string]any{
		"status":       "ok",
		"expires_in_s": int(ttl.Seconds()),
	})
}

func (s *Service) handleDeregister(w http.ResponseWriter, r *http.Request) {
	addr, ok := clientIP(r, s.cfg.TrustedProxies)
	if !ok {
		writeError(w, http.StatusBadRequest, "could not determine source address")
		return
	}
	host := addr.String()
	if !s.writes.allow(host) {
		s.tooManyRequests(w)
		return
	}

	// The contract document puts the port in a JSON body; #999's checklist writes it as a query
	// parameter (`DELETE /v1/servers?port=N`). Both are accepted rather than picking a winner and
	// making the other spelling a silent no-op. The body wins when both are present.
	port := 0
	var req deregisterRequest
	if err := decodeJSON(w, r, &req); err == nil && req.Port != nil {
		port = *req.Port
	} else if q := r.URL.Query().Get("port"); q != "" {
		if n, perr := strconv.Atoi(q); perr == nil {
			port = n
		}
	}
	if port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, "port is required")
		return
	}

	if s.store.Delete(host, uint16(port)) {
		s.log.Info("deregistered", "host", host, "port", port)
	}
	// A missing entry is explicitly not an error: a DELETE that races the TTL is normal.
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	addr, ok := clientIP(r, s.cfg.TrustedProxies)
	if !ok {
		writeError(w, http.StatusBadRequest, "could not determine source address")
		return
	}
	if !s.reads.allow(addr.String()) {
		s.tooManyRequests(w)
		return
	}

	entries := s.store.List(MaxListEntries)

	// Build the array row by row so the 1 MiB cap is enforced on the bytes actually sent, not
	// estimated from a row count. The client stops reading at 1 MiB and would be left parsing a
	// severed array, so a row that would cross the line is dropped instead.
	body := make([]byte, 0, 1024)
	body = append(body, '[')
	for _, e := range entries {
		row, err := json.Marshal(serverJSON{
			Name:       e.Name,
			Host:       e.Host,
			Port:       e.Port,
			Mode:       e.Mode,
			Mission:    e.Mission,
			Players:    e.Players,
			MaxPlayers: e.MaxPlayers,
			Passworded: e.Passworded,
		})
		if err != nil {
			continue
		}
		sep := 0
		if len(body) > 1 {
			sep = 1
		}
		if len(body)+sep+len(row)+1 > MaxListBytes {
			s.log.Warn("list truncated at response cap", "cap_bytes", MaxListBytes)
			break
		}
		if sep == 1 {
			body = append(body, ',')
		}
		body = append(body, row...)
	}
	body = append(body, ']')

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Service) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"servers": s.store.Len(),
	})
}

func (s *Service) tooManyRequests(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(int(s.cfg.Heartbeat.Seconds())))
	writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	// Unknown fields are IGNORED, not rejected: the contract's own client is described as
	// deliberately tolerant, and a lobby that refused a field a newer server added would break
	// every listing on that server rather than ignoring one value.
	if err := dec.Decode(dst); err != nil {
		return errors.New("malformed JSON body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// clampCount keeps a reported player count non-negative. A negative is nonsense rather than an
// attack, so it is corrected rather than refused.
func clampCount(v *int) int {
	if v == nil || *v < 0 {
		return 0
	}
	return *v
}
