package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"agento/internal/pi"
)

var (
	errUnauthorized = errors.New("gateway: missing or invalid bearer token")
)

// Prompt rejection, split into two so the client is told which case occurred —
// "you sent no user turn at all" and "your most recent user turn was empty"
// look identical to the handler but need different fixes.
//
// The empty case is an error rather than a reason to send an earlier turn:
// answering a question the user did not just ask, with nothing indicating
// anything was wrong, is worse than a clear failure.
var (
	errNoUserTurn = errors.New("messages must contain a user message")
	errBlankTurn  = errors.New("the most recent user message is empty; resend the text " +
		"rather than an empty message")
)

// maxConversationIDLen bounds X-Hermes-Session-Id. The store's entry COUNT is
// LRU-capped, but an unbounded key would let a client drive memory growth per
// request regardless of the cap.
const maxConversationIDLen = 256

// Config configures a Server.
type Config struct {
	// Password is the bearer token the app authenticates with. An empty value
	// disables authentication entirely, which is refused unless AllowNoAuth is
	// set: an agent with a shell must never be reachable unauthenticated.
	Password string

	// AllowNoAuth permits starting with an empty Password. Intended for local
	// debugging only.
	AllowNoAuth bool

	// DefaultProfile handles requests with no profile path segment.
	DefaultProfile string

	// TurnTimeout bounds one assistant turn. A turn that never settles is a bug
	// in Pi or a wedged provider, and must not hold an HTTP connection open
	// indefinitely.
	TurnTimeout time.Duration

	// AgentDir is Pi's agent directory (PI_CODING_AGENT_DIR), used to read mcp.json
	// for the toolsets inventory. Optional: empty means "no agent directory
	// configured", and the toolsets endpoint then reports an empty list rather than
	// failing, since a gateway with no MCP configuration is a legitimate state.
	AgentDir string

	// InventoryTimeout bounds one read-only metadata call to a Pi agent
	// (get_available_models, get_commands).
	//
	// Deliberately separate from TurnTimeout, and much shorter. These are local IPC
	// reads of a child's own state and should answer in milliseconds; a turn
	// legitimately runs for minutes because a model is involved. Using TurnTimeout
	// here meant a wedged Pi held a handler goroutine — and the app's picker or
	// Skills page — for up to ten minutes, for a request that carries no work worth
	// waiting that long for.
	//
	// Generous rather than tight on purpose: at startup Pi is still connecting MCP
	// servers, and a budget that flaked under that load would trade a rare wedge for
	// a common spurious failure. Expiry surfaces as the same 502 as any other
	// upstream error, so the client sees one failure mode, not two.
	InventoryTimeout time.Duration

	// KeepaliveInterval writes an SSE comment while waiting, so proxies and the
	// app do not treat an idle stream as dead. Zero means "no choice made" and is
	// replaced with the default; set it to KeepaliveDisabled to turn the comments
	// off deliberately.
	KeepaliveInterval time.Duration

	// Logf receives operational messages. Nil discards them.
	Logf func(format string, args ...any)
}

func (c *Config) withDefaults() {
	if c.TurnTimeout <= 0 {
		c.TurnTimeout = 10 * time.Minute
	}
	if c.InventoryTimeout <= 0 {
		c.InventoryTimeout = 30 * time.Second
	}
	// Exactly zero means "the caller did not choose", which withDefaults fills in.
	// A negative value is a deliberate KeepaliveDisabled and passes through — this
	// used to test <= 0, which quietly overrode a caller who had explicitly asked
	// for no keepalive comments and made PI_KEEPALIVE=0 impossible.
	if c.KeepaliveInterval == 0 {
		c.KeepaliveInterval = 20 * time.Second
	}
	if c.DefaultProfile == "" {
		c.DefaultProfile = "default"
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	// The presented token is trimmed before comparison, so the configured value has
	// to be trimmed too or a PASSWORD carrying a trailing newline — easy to
	// introduce by editing .env, and invisible in most viewers — silently fails
	// every request with an opaque 401. Trimmed once here rather than per request.
	c.Password = strings.TrimSpace(c.Password)
}

// KeepaliveDisabled turns the SSE keepalive comments off.
//
// A negative duration, because the zero value of a Config field already means
// "the caller did not choose" and withDefaults fills it in with 20s. A sentinel
// rather than "0 disables" so that an unset field and an explicit off stay
// distinguishable.
//
// Worth having: behind a proxy with a long idle timeout the comments carry no
// information, and when reading raw SSE output while debugging they are noise.
const KeepaliveDisabled time.Duration = -1

// Server is the HTTP surface in front of a Pi agent.
//
// One Agent per profile: Pi loads AGENTS.md and skills from its working
// directory, so a profile is a process, not a runtime option.
type Server struct {
	cfg      Config
	handlers map[string]*agentHandler
	mux      *http.ServeMux

	// mcpServers is the configured MCP server set, read once at startup. Empty when
	// there is no agent directory or no readable mcp.json, which is a valid state.
	mcpServers []string
}

// agentHandler is one profile's agent plus its session store.
type agentHandler struct {
	profile  string
	runner   agentRunner
	sessions *SessionStore

	// turnMu serialises turns on this profile. It is not optional bookkeeping:
	// Pi holds ONE session per process and Subscribe() broadcasts to every
	// subscriber, so two concurrent turns would race on get_state /
	// switch_session / prompt and each would see the other's deltas — two
	// conversations interleaved into one streamed reply.
	//
	// Held for the whole turn, including the stream, since the deltas are what
	// interleave.
	turnMu sync.Mutex
}

// New builds a Server. The agents map is copied, so the caller may reuse it.
func New(cfg Config, agents map[string]Agent) (*Server, error) {
	cfg.withDefaults()

	if cfg.Password == "" && !cfg.AllowNoAuth {
		return nil, errors.New("gateway: Password is empty; set AllowNoAuth to accept that")
	}

	s := &Server{
		cfg:      cfg,
		handlers: make(map[string]*agentHandler, len(agents)),
		mux:      http.NewServeMux(),
	}

	// Read once, here, rather than per request. mcp.json is fixed for the life of
	// the process: the entrypoint runs `pi mcp list` at boot and refuses to start if
	// any server fails, and Pi connects its MCP servers at startup. Re-reading it on
	// every picker poll was pure disk I/O for a value that cannot change, and it read
	// the file with no size limit, so a corrupt or oversized file was a cheap DoS on a
	// route the app polls.
	//
	// A failure here is logged and treated as "no servers", not fatal: a gateway with
	// no MCP configuration is a legitimate state, and the log is what distinguishes it
	// from a deliberate empty list. Failing New would take chat down over a skills
	// page.
	s.mcpServers = readMCPServers(cfg.AgentDir, cfg.Logf)

	// A profile is a distinct Pi process with its own personality and skills, so
	// each needs its own session bookkeeping; sharing a store would let two
	// profiles claim the same session file.
	for profile, agent := range agents {
		if agent == nil {
			return nil, errors.New("gateway: nil agent for profile " + profile)
		}
		// One store per profile: sharing one would let two profiles claim the
		// same Pi session file.
		store := NewSessionStore()
		// Stored by pointer: the handler carries a mutex, and copying it would copy
		// the lock — two requests then hold two different locks over one turn.
		s.handlers[profile] = &agentHandler{
			profile:  profile,
			runner:   agentRunner{agent: agent, sessions: store},
			sessions: store,
		}
	}

	if _, ok := s.handlers[cfg.DefaultProfile]; !ok && len(s.handlers) > 0 {
		return nil, errors.New("gateway: default profile " + cfg.DefaultProfile +
			" has no agent configured")
	}

	s.mux.HandleFunc("/healthz", s.handleHealth)
	// Probe paths are written by humans and orchestrators, and "/healthz/" is a
	// common variant. Answering it rather than 404 keeps a liveness check from
	// failing on punctuation.
	s.mux.HandleFunc("/healthz/", s.handleHealth)
	s.mux.HandleFunc("/", s.route)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Profiles lists the configured profile names.
func (s *Server) Profiles() []string {
	out := make([]string, 0, len(s.handlers))
	for name := range s.handlers {
		out = append(out, name)
	}
	return out
}

// handleHealth is deliberately unauthenticated so orchestrators can probe it
// without a credential. It reveals only liveness.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// The "/healthz/" pattern matches the whole subtree, so "/healthz/anything"
	// reached this handler too and answered 200. Only the two exact forms are the
	// probe; anything else is a 404, so an orchestrator cannot mistake a typo'd
	// path for a healthy agent.
	if p := r.URL.Path; p != "/healthz" && p != "/healthz/" {
		// bounded() like every other reflected path: this one is reachable WITHOUT
		// authentication, so an unbounded echo hands an anonymous caller a way to
		// make the server emit an arbitrarily large response.
		apiError(w, http.StatusNotFound, "invalid_request_error", "", "unknown path "+bounded(p))
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "", "GET or HEAD only")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// A HEAD response carries headers only; writing a body violates the method's
	// contract and can desync a keep-alive client.
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// authenticate enforces the bearer token.
//
// Compared in constant time so a timing difference cannot be used to recover the
// token byte by byte.
func (s *Server) authenticate(r *http.Request) error {
	if s.cfg.Password == "" {
		return nil
	}
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return errUnauthorized
	}
	token := strings.TrimSpace(header[len(prefix):])
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Password)) != 1 {
		return errUnauthorized
	}
	return nil
}

// ReservedProfileNames are directory names under the profile directory that cannot
// be used as a profile, because routing reads a leading segment matching one of them
// as "no profile segment here".
//
// Exported, and used by profile discovery, so that the skip and the reservation come
// from one place.
var ReservedProfileNames = []string{reservedPathSegment, "api"}

// reservedPathSegment is the API version root — the segment that reads as "no profile
// here" rather than as a profile name. Derived into ReservedProfileNames above rather
// than written out again there, so the name has exactly one spelling.
//
// This exists as one list precisely because two lists is how this went wrong before:
// routing tested `head == "v1" || head == "api"` inline while profile discovery
// skipped only "v1". They drifted, and a profiles/api directory started a working
// agent that no request could ever reach — every path segment "api" was claimed by
// the provider-picker route instead. Silent, and visible only as a profile that
// appears in `docker compose ps` and 404s.
const reservedPathSegment = "v1"

// IsReservedProfileName reports whether name may not be used as a profile directory.
func IsReservedProfileName(name string) bool {
	for _, r := range ReservedProfileNames {
		if name == r {
			return true
		}
	}
	return false
}

// requireMethod enforces that a route is reached with the right verb, answering 405
// with an Allow header rather than letting the handler run and misreport.
//
// Split out because five routes need it and the 405 body should read the same each
// time. A client that sends POST to a GET inventory route is a bug worth naming
// precisely: 404 would send it looking for a different path.
func requireMethod(w http.ResponseWriter, r *http.Request, want, route string) bool {
	if r.Method == want {
		return true
	}
	w.Header().Set("Allow", want)
	apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "",
		route+" accepts "+want)
	return false
}

// splitProfile separates an optional leading profile segment from the endpoint.
//
// The proxy strips /p/pi before forwarding, so the app's per-tab profile path
// arrives first: /story/v1/chat/completions. A path that starts with a reserved
// first segment has no profile and falls back to the default.
//
// Decided on the FIRST segment alone. An earlier version inferred it from the
// second segment, which mistook "v1" in /v1/chat/completions for a profile name
// and answered 404.
func (s *Server) splitProfile(urlPath string) (profile, rest string) {
	// Clean collapses the duplicate slashes a client can produce
	// (/story//v1/chat/completions), which would otherwise leave an empty segment
	// and route nowhere.
	trimmed := strings.Trim(path.Clean("/"+strings.TrimPrefix(urlPath, "/")), "/")
	if trimmed == "" {
		return s.cfg.DefaultProfile, ""
	}
	head, tail, hasTail := strings.Cut(trimmed, "/")
	// Reserved first segments: the API version root and the provider-picker
	// namespace. Without reserving "api", /api/model/options — which nginx routes
	// here unprefixed — reads "api" as a profile name and 404s as "unknown profile
	// api", a needlessly confusing way to say "wrong path". Checked against the same
	// list profile discovery skips, so the two cannot disagree.
	// No `head == ""` case: trimmed returned early if empty, and Cut on a non-empty
	// string never yields an empty head. The condition was unreachable.
	if IsReservedProfileName(head) {
		return s.cfg.DefaultProfile, trimmed
	}
	if !hasTail {
		// A single segment names a profile that was addressed without an endpoint
		// ("/story"), or a profile that does not exist at all. Keep the name: as a
		// path it reached the default profile's handler and was reported as
		// "unknown path story", which sends someone debugging a typo'd profile to
		// look at their URL rather than at their profile list.
		return head, ""
	}
	return head, tail
}

// route dispatches on the profile path segment.
//
// The proxy strips the /p/pi prefix before forwarding, so the app's per-tab
// profile path arrives as the first segment: /story/v1/chat/completions. A
// request with no segment falls back to the default profile, which keeps a client
// that has not configured a per-tab path working.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	profile, rest := s.splitProfile(r.URL.Path)

	// Every route except healthz sits behind the bearer token.
	if err := s.authenticate(r); err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agento"`)
		apiError(w, http.StatusUnauthorized, "authentication_error", "",
			"missing or invalid Authorization bearer token")
		return
	}

	handler, ok := s.handlers[profile]
	if !ok {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown profile "+bounded(profile))
		return
	}

	// A profile was named with no endpoint under it. The profile resolved, so
	// saying which one and what was expected beats a bare path echo.
	if rest == "" {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"profile "+bounded(profile)+" has no endpoint; expected /"+
				bounded(profile)+"/v1/chat/completions")
		return
	}

	// The two session routes, matched by prefix rather than as switch cases because the
	// conversation id is a path segment. Handled before the switch so the two routes share
	// one place, and so a malformed id is reported by the same code that parses it.
	//
	// Reachable two ways: /{profile}/api/sessions/{id} from a tab configured with a
	// profile path, and /api/sessions/{id} unprefixed — splitProfile reserves "api", so
	// the second resolves to the default profile with the whole path as `rest`. One case
	// serves both, which is the same reason /api/model/options needs no profile.
	if strings.HasPrefix(rest, "api/sessions/") {
		if !requireMethod(w, r, http.MethodGet, "/api/sessions/{id}") {
			return
		}
		s.handleSession(w, handler, rest)
		return
	}

	switch rest {
	case "v1/chat/completions":
		// A known endpoint reached with the wrong verb is 405, not 404: the
		// distinction tells a client whether to fix its method or its path.
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "",
				"/v1/chat/completions accepts POST")
			return
		}
		s.handleChat(w, r, handler)

	// Read-only inventories behind the same version prefix. The app reaches
	// /{profile}/v1/skills and /{profile}/v1/toolsets through ServerApi, and nginx
	// forwards /p/* here, so the profile prefix has already been consumed.
	case "v1/skills":
		if !requireMethod(w, r, http.MethodGet, "/v1/skills") {
			return
		}
		s.handleSkills(w, r, handler)
	case "v1/toolsets":
		if !requireMethod(w, r, http.MethodGet, "/v1/toolsets") {
			return
		}
		s.handleToolsets(w, r, handler)

	// The provider picker. Only the "api" spelling is routed: splitProfile returns the
	// whole trimmed path when the first segment is reserved, and everything after the
	// profile segment otherwise, so both paths a client actually sends land on
	// rest == "api/model/options":
	//
	//	/api/model/options             nginx routes this exact path here
	//	/{profile}/api/model/options   the app builds it from the per-tab path
	//
	// A bare "/god/model/options" reaches rest == "model/options", but nothing sends
	// it: the app builds "{path}/api/model/options" and nginx routes the /api/ one.
	// An earlier version accepted it as a tolerant alias, which was untested surface
	// invented on the off-chance that some client strips the segment.
	case "api/model/options":
		if !requireMethod(w, r, http.MethodGet, "/api/model/options") {
			return
		}
		s.handleModelOptions(w, r, handler)
	default:
		if rest == "v1" || strings.HasPrefix(rest, "v1/") {
			apiError(w, http.StatusNotFound, "invalid_request_error", "",
				"unknown endpoint /"+bounded(rest))
			return
		}
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown path "+bounded(r.URL.Path))
	}
}

// handleChat runs one assistant turn and streams it as SSE.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, h *agentHandler) {
	// Everything cheap happens before the profile lock is taken: reading the body,
	// and validating that a prompt and a reasoning effort are present at all.
	//
	// Taking the lock first meant a client that opened a request and dribbled the
	// body held the whole profile for up to TurnTimeout, forcing every other tab on
	// that profile to 429 — one slow client denying service to everyone. None of
	// this touches the agent, so none of it needs the lock.
	var req chatRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}

	// reasoning_effort is required, not defaulted. Pi accepts an unknown thinking
	// level silently, so a missing or misspelled value would otherwise produce a
	// plausible answer at the wrong reasoning depth with nothing indicating the
	// setting was ignored.
	if req.ModelOptions == nil || strings.TrimSpace(req.ModelOptions.ReasoningEffort) == "" {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "model_options.reasoning_effort",
			"model_options.reasoning_effort is required. Send it explicitly — for "+
				"example {\"model_options\":{\"reasoning_effort\":\"off\"}}. It is not "+
				"optional: Pi accepts an unrecognised level silently, so a default would "+
				"be indistinguishable from the requested setting being applied.")
		return
	}

	userTurn, err := lastUserTurn(req.Messages)
	if err != nil {
		// The message distinguishes "no user turn" from "your latest turn was empty",
		// because the client fixes them differently.
		apiError(w, http.StatusBadRequest, "invalid_request_error", "messages", err.Error())
		return
	}
	if userTurn.SystemMessages > 0 {
		// Logged, not applied. Personality comes from the profile's AGENTS.md, so a
		// per-request system message is dropped like every other non-user turn; a
		// silent drop would leave the caller believing it took effect.
		s.cfg.Logf("gateway: ignoring %d system message(s): personality is per-profile, not per-request",
			userTurn.SystemMessages)
	}

	// Validated before the profile lock, with the rest of the cheap checks. This id
	// becomes a map key, so its length is bounded — and checking it while holding
	// the lock meant a malformed header occupied the profile, and answered 429
	// instead of 400 whenever the profile was busy. None of this touches the
	// agent, so none of it needs the lock.
	conversationID := strings.TrimSpace(r.Header.Get("X-Hermes-Session-Id"))
	if len(conversationID) > maxConversationIDLen {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "X-Hermes-Session-Id",
			"X-Hermes-Session-Id must be at most 256 characters")
		return
	}

	// The profile lock is held only for the agent interaction: session resolution,
	// the reasoning level, and the turn itself.
	if !h.turnMu.TryLock() {
		w.Header().Set("Retry-After", "5")
		apiError(w, http.StatusTooManyRequests, "rate_limit_error", "",
			"this profile is already answering a turn. One turn runs at a time per "+
				"profile, because the agent holds a single session and streaming turns "+
				"concurrently would interleave them.")
		return
	}
	defer h.turnMu.Unlock()

	// The turn budget starts only once the profile is held, so time spent
	// contending for the lock is not charged to the turn — and the 429 path does
	// not leave a timer running.
	turnCtx, cancel := context.WithTimeout(r.Context(), s.cfg.TurnTimeout)
	defer cancel()

	// Apply the client's provider/model selection before anything that reads
	// model-dependent state (#251). selectModel validates against the live
	// inventory first, so an unknown provider or model 400s here without
	// touching the process — a bad selection costs nothing, same guarantee
	// the effort check below used to carry alone.
	wantProvider := strings.TrimSpace(req.Provider)
	wantModel := strings.TrimSpace(req.Model)
	prevModel, switched, err := h.runner.selectModel(turnCtx, wantProvider, wantModel)
	if err != nil {
		var um *unknownModelError
		var up *unknownProviderError
		switch {
		case errors.As(err, &um):
			// Echo um.model, not wantModel: the multi-provider case
			// appends a "name one" hint to the value, and rebuilding
			// from the request would drop it.
			apiError(w, http.StatusBadRequest, "invalid_request_error", "model",
				"unknown model "+strconv.Quote(bounded(um.model)))
			return
		case errors.As(err, &up):
			apiError(w, http.StatusBadRequest, "invalid_request_error", "provider",
				"unknown provider "+strconv.Quote(bounded(up.provider)))
			return
		default:
			s.cfg.Logf("gateway: selecting model: %v", err)
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"could not switch the agent's model")
			return
		}
	}

	// A failed pre-prompt turn must not silently repoint the profile: the
	// switch above already applied, so every failure below that returns
	// before prompting reverts to the pre-switch model first. Best-effort —
	// if the revert itself fails, log and still report the turn's error,
	// not the cleanup's. No-op when nothing switched (or the previous
	// model could not be named).
	//
	// Detached context, not turnCtx: when the failure IS the deadline (or a
	// client disconnect), turnCtx is already done and a revert fired on it
	// would silently no-op — leaving exactly the sticky switch this exists
	// to prevent. Five seconds is generous for a local IPC round trip.
	revertModel := func() {
		if switched && prevModel != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, rerr := h.runner.agent.Call(ctx, "set_model",
				map[string]any{"model": prevModel}); rerr != nil {
				s.cfg.Logf("gateway: reverting model after failed turn: %v", rerr)
			}
		}
	}

	agentState, err := h.runner.resolveSession(turnCtx, conversationID)
	if err != nil {
		revertModel()
		s.cfg.Logf("gateway: resolving session: %v", err)
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not resolve the agent session")
		return
	}

	// The thinking levels are read AFTER the model switch, because they are
	// per-model: validating effort against the previous model's ladder is the
	// wrong-model refusal #251 was written about. This used to run before the
	// session resolve so a bad request cost nothing; the model gate above keeps
	// that property for selections. Effort can only be judged against the
	// applied model's levels, so a bad-effort request is refused after the
	// switch — and the refusal path reverts to the pre-switch model, or one
	// bad request would silently repoint every following turn.
	levels, err := h.runner.thinkingLevels(turnCtx)
	if err != nil {
		// Tail, not verbatim: a wedged agent can emit megabytes of stderr and this
		// runs on every failed turn, so one bad agent would grow the log without
		// bound. The last lines are the ones that say what went wrong.
		s.cfg.Logf("gateway: reading thinking levels: %v (stderr: %s)", err,
			Tail(h.runner.agent.Stderr(), StderrTailLimit))
		revertModel()
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not read the agent's supported reasoning levels")
		return
	}
	level, effortErr := resolveEffort(req.ModelOptions.ReasoningEffort, levels)
	if effortErr != nil {
		revertModel()
		detail := "unrecognised value"
		if errors.Is(effortErr, errEffortUnsupported) {
			detail = "not supported by this model; supported levels: " + supportedList(levels)
		}
		apiError(w, http.StatusBadRequest, "invalid_request_error", "model_options.reasoning_effort",
			"model_options.reasoning_effort "+detail+
				". The agent accepted this silently and ignored it, so it is refused here.")
		return
	}

	// The model reported in frames is the one Pi is actually running — which,
	// since the selection above, is the applied model, not the claim. Before
	// #251 the gateway never selected on those fields, so this echo was the
	// only honest half of a substitution the app could not see; now the claim
	// and the echo agree whenever the switch succeeded. It is still bounded:
	// the value is echoed on every streamed delta, and real ids are about 15
	// characters, so this guards the shape rather than the value and changes
	// nothing in practice.
	//
	// If Pi reports no model, fall back to the client's claim but BOUND it: the
	// raw claim arrives in a 4 MiB-capped body and would otherwise be repeated on
	// every delta. Falling back unbounded would reintroduce the amplification this
	// line was written to remove; blank would be worse still, since an empty model
	// on every frame is a wire shape no client can do anything with.
	model := bounded(agentState.Model.ID)
	if model == "" {
		model = bounded(req.Model)
	}
	req.Model = model

	// Always sent, even when agentState already reports this level, and that is
	// deliberate rather than an oversight. Skipping it when
	// agentState.ThinkingLevel == level would save one local IPC round trip per
	// turn — measured against live Pi, get_state does report the applied level
	// exactly for all five of off/minimal/low/medium/high — so the skip looks free.
	//
	// It is not free, for two reasons.
	//
	// First, this call is the only thing standing between a silent wrong-depth turn
	// and a correct one: Pi answers set_thinking_level with success and leaves an
	// unrecognised level UNCHANGED. Every guard elsewhere in this handler exists
	// because a plausible answer at the wrong reasoning depth, with nothing saying
	// the setting was ignored, is the failure nobody can notice. A conditional skip
	// widens exactly that surface, and it widens it by trusting a value reported
	// by another process about its own internal state.
	//
	// Second — and this is the one that actually decides it — I verified that
	// get_state reports the level faithfully, but NOT that set_thinking_level is
	// free of other effects. If a future Pi does anything else on that call, a
	// conditional skip silently changes behaviour, and the change would be
	// invisible because the level would still read back correct.
	//
	// The round trip being saved is a local IPC call: no model call, no tokens,
	// microseconds of wall clock. The migration's goal is token cost, which this
	// does not touch. Paying microseconds to keep one setting unconditional is the
	// trade, and the round trip added after a session switch was made for the same
	// reason: prefer a provably-correct state over a saved local call.
	if _, err := h.runner.agent.Call(turnCtx, "set_thinking_level",
		map[string]any{"level": level}); err != nil {
		revertModel()
		s.cfg.Logf("gateway: set_thinking_level(%s): %v", level, err)
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not apply the requested reasoning level")
		return
	}

	// Subscribe before prompting: Pi can begin streaming before the prompt
	// response arrives, and subscribing afterwards would lose the first deltas.
	events, unsubscribe := h.runner.agent.Subscribe()
	defer unsubscribe()

	// The request may ask for a single JSON response; the app always streams, but
	// a non-streaming client must not hang waiting for SSE.
	if !req.Stream {
		s.handleChatBuffered(w, turnCtx, h, events, userTurn, conversationID, req)
		return
	}

	sse := newSSEWriter(w)
	sse.headers()
	w.WriteHeader(http.StatusOK)
	if err := sse.comment("stream open"); err != nil {
		return
	}

	completionID := completionIDFor(conversationID)

	// Usage is cumulative on Pi's side; the app takes the last frame that carries
	// it, so it is attached once at the end of the turn.
	var lastUsage piUsage

	keepalive := startKeepalive(turnCtx, sse, s.cfg.KeepaliveInterval)
	defer keepalive()

	if err := h.runner.agent.Send(turnCtx, "prompt", promptPayload(userTurn)); err != nil {
		s.writeStreamError(sse, "could not deliver the prompt to the agent")
		return
	}

	for {
		select {
		case <-turnCtx.Done():
			s.writeStreamError(sse, "the agent did not finish this turn in time")
			return

		case rec, open := <-events:
			if !open {
				// The agent exited; subscribers are closed on process death.
				s.writeStreamError(sse, "the agent stopped before finishing this turn")
				return
			}

			// Usage is cumulative and reported on message_update today, but it is
			// decoded for EVERY record rather than inside one case: Pi also nests it
			// under other events (compaction_end carries result.usage), and a record
			// whose final counts were never read would silently report zero tokens
			// for a completed turn.
			if u, ok := decodeUsage(rec); ok {
				lastUsage = u
			}
			// A turn Pi could not complete is reported as an error, not as an empty
			// success. Checked before the delta switch because an errored turn emits no
			// content at all, so without this the stream would end cleanly and the client
			// would see a 200 with `finish_reason: stop` and no text.
			if msg, failed := decodeTurnError(rec); failed {
				s.writeStreamError(sse, msg)
				return
			}

			switch rec.Type {
			case "message_update":
				text, reasoning, isDelta, ok := decodeMessageUpdate(rec)
				if !ok || !isDelta {
					continue
				}
				if text == "" && reasoning == "" {
					continue
				}
				frame := newChunk(completionID, req.Model, text, reasoning)
				if err := sse.write(frame); err != nil {
					return
				}

			default:
				// Tool progress. A known lifecycle event always renders; an
				// unfamiliar one renders only if it names a tool, so a future Pi
				// event about a tool does not make it vanish mid-execution while an
				// unrelated event is not mislabelled as one. Without a tool name
				// there is nothing to attribute, and the app skips unattributable
				// frames anyway.
				name, named := decodeToolEvent(rec)
				if !named {
					break
				}
				status := toolStatus(rec.Type)
				if !isToolEvent(rec.Type) {
					// Unfamiliar type that still names a tool: say it is running
					// rather than claiming a start or end we did not observe.
					status = "running"
				}
				if err := sse.write(toolFrame{Tool: name, Status: status}); err != nil {
					return
				}

			case pi.TypeAgentSettled:
				// The end signal for the turn. Usage goes on its own frame so the
				// app's last-seen-wins parsing picks it up before [DONE].
				if u := lastUsage.toWire(); u.nonZero() {
					if err := sse.write(chunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: nowUnix(),
						Model:   req.Model,
						Usage:   u,
					}); err != nil {
						return
					}
				}
				_ = sse.done()
				return
			}
		}
	}
}

// handleChatBuffered answers a non-streaming request with one JSON body, for
// clients other than the app. The app always sets stream=true.
//
// Tool progress is deliberately NOT reported here. These frames exist to drive the
// app's live progress UI, and a buffered response has no timeline to attach them
// to; emitting them into a single JSON body would mean inventing an ordering the
// caller never asked for. The asymmetry with the streaming path is intentional.
// handleChatBuffered serves a non-streamed completion.
//
// Unlike the streaming path this puts NOTHING on the wire until the turn is done,
// so it holds the profile lock in silence for as long as the agent takes. Two
// consequences worth knowing before pointing a proxy at it:
//
//   - A proxy read timeout shorter than TurnTimeout cuts the connection with no
//     partial output to show for it, and the client cannot resume from a session
//     the way it can from a dropped stream. The deployed chain is now ordered
//     outermost-first — nginx 990s idle, this TurnTimeout 960s (PI_TURN_TIMEOUT),
//     and under that the 900s + 30s spawn_subagent delegation ceiling — so the
//     layer that fires is the innermost one that has an answer to give. A proxy
//     shorter than this one would turn every long turn into an unexplained 504;
//     the app never hits the buffered path anyway because it always streams.
//   - The profile is blocked for the whole turn either way. Streaming occupies it
//     too, so switching a client to non-streamed buys nothing but removes the
//     progress signal.
func (s *Server) handleChatBuffered(
	w http.ResponseWriter, ctx context.Context, h *agentHandler,
	events <-chan pi.Record, userTurn *turn, conversationID string, req chatRequest,
) {
	// Same derived id as the streaming path, so a completion can be correlated
	// from logs regardless of which mode served it.
	completionID := completionIDFor(conversationID)
	var text, reasoning strings.Builder
	var lastUsage piUsage

	if err := h.runner.agent.Send(ctx, "prompt", promptPayload(userTurn)); err != nil {
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not deliver the prompt to the agent")
		return
	}

	for {
		select {
		case <-ctx.Done():
			apiError(w, http.StatusGatewayTimeout, "timeout_error", "",
				"the agent did not finish this turn in time")
			return
		case rec, open := <-events:
			if !open {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the agent stopped before finishing this turn")
				return
			}
			// Decoded for every record, not just message_update — see the streaming
			// loop for why.
			if u, ok := decodeUsage(rec); ok {
				lastUsage = u
			}
			if msg, failed := decodeTurnError(rec); failed {
				// 502, not 200-with-blanks: the gateway is healthy and the agent is not,
				// and the caller has to be able to tell that apart from a short answer.
				apiError(w, http.StatusBadGateway, "upstream_error", "", msg)
				return
			}

			switch rec.Type {
			case "message_update":
				if t, r, isDelta, ok := decodeMessageUpdate(rec); ok && isDelta {
					text.WriteString(t)
					reasoning.WriteString(r)
				}
			case pi.TypeAgentSettled:
				// choices[0].message, not choices[0].delta: this is the non-streaming
				// response and a standard OpenAI client reads exactly that. Emitting
				// the delta shape here handed such a client a 200 it could not parse.
				body := completion{
					ID:      completionID,
					Object:  "chat.completion",
					Created: nowUnix(),
					Model:   req.Model,
					Choices: []completionChoice{{
						Message: delta{
							Content:          text.String(),
							ReasoningContent: reasoning.String(),
						},
						Finish: "stop",
					}},
				}
				if u := lastUsage.toWire(); u.nonZero() {
					body.Usage = u
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = writeJSON(w, body)
				return
			}
		}
	}
}

// promptPayload builds Pi's prompt command, carrying images when present.
//
// Pi takes text and images as separate fields, which is exactly what the two
// OpenAI content shapes reduce to.
func promptPayload(userTurn *turn) map[string]any {
	payload := map[string]any{"message": userTurn.Text}
	if len(userTurn.Images) > 0 {
		images := make([]map[string]any, 0, len(userTurn.Images))
		for _, img := range userTurn.Images {
			images = append(images, map[string]any{
				"type":     "image",
				"data":     img.Data,
				"mimeType": img.MimeType,
			})
		}
		payload["images"] = images
	}
	return payload
}

// writeStreamError emits the terminal error frame. No [DONE] follows, matching the
// app's contract for a failed turn.
func (s *Server) writeStreamError(sse *sseWriter, message string) {
	_ = sse.write(errorFrame{Error: message})
}

// startKeepalive emits SSE comments so an idle stream is not reaped by a proxy.
// Comments are used because the app skips lines starting with ':'.
//
// The returned function stops the ticker and waits for the goroutine to exit, so
// no comment can be written after the caller has moved on.
func startKeepalive(ctx context.Context, sse *sseWriter, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = sse.comment("keepalive")
			}
		}
	}()

	// Stop signals AND waits. Closing the channel alone only guarantees the
	// goroutine returns at its next select; if it is already inside sse.comment()
	// it keeps writing after stop() has returned — and the caller runs this from a
	// defer on the way out of the handler, so the write can land after the handler
	// returned. http.ResponseWriter may not be used after the handler has
	// returned, and net/http can reuse the connection underneath it.
	//
	// The sseWriter mutex stops two writes interleaving, but it cannot stop a write
	// arriving too late; only joining here can.
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
}
