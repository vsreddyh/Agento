// Command pi-gateway serves the OpenAI-compatible HTTP surface the Agento Android
// app already speaks, backed by Pi agent processes.
//
// One Pi process per profile: Pi loads AGENTS.md and skills from its working
// directory, so a profile is a process rather than a runtime option. Each runs
// under /opt/pi/profiles/<name>/ with PI_CODING_AGENT_DIR pointing at the shared
// agent dir, which is where settings.json, mcp.json and the session store live.
//
// Environment:
//
//	PASSWORD          bearer token the app authenticates with (required); withheld
//	                  from each Pi child's environment
//	PI_SERVER_PORT    listen port (default 8643)
//	PI_GATEWAY_ADDR   listen address (default :8643)
//	PI_CODING_AGENT_DIR  Pi agent dir (default /opt/pi)
//	PI_PROFILE_DIR    where profile dirs live (default $PI_CODING_AGENT_DIR/profiles)
//	PI_TURN_TIMEOUT   seconds for one assistant turn (default 600)
//	PI_KEEPALIVE      seconds between SSE keepalive comments, 0 disables them
//	                    (default 20)
//	PI_INVENTORY_TIMEOUT  seconds for one model-list or skills read (default 30)
//	PI_BIN            pi executable (default "pi" from PATH)
//	PI_DEFAULT_MODEL  model per new session (default opencode-go/mimo-v2.6-flash)
//	OPENCODE_API_KEY  provider key, required by Pi itself
//	PI_READY_TIMEOUT  seconds to wait for each agent to answer a probe (default 60)
//	PI_DEFAULT_PROFILE  profile used when a request has no profile path segment
//	                    (default "default")
package main

import (
	"agento/internal/approvals"
	"agento/internal/connectors"
	"agento/internal/crashlog"
	"agento/internal/cronjobs"
	"agento/internal/memory"
	"agento/internal/runs"
	"agento/internal/teach"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agento/internal/gateway"
	"agento/internal/pi"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("pi-gateway: %v", err)
	}
}

func run() error {
	// Unconditional, and deliberately so: gateway.Config has an AllowNoAuth escape
	// hatch for local debugging, but this binary will not use it. The one place
	// that flag is reachable is a test calling gateway.New directly — shipping a
	// server that starts on an empty credential is not a debugging convenience, it
	// is an unauthenticated agent with a shell.
	password := os.Getenv("PASSWORD")
	if password == "" {
		return errors.New("PASSWORD is unset; refusing to serve an unauthenticated agent")
	}

	agentDir := envOr("PI_CODING_AGENT_DIR", "/opt/pi")
	profileDir := envOr("PI_PROFILE_DIR", filepath.Join(agentDir, "profiles"))
	piBin := envOr("PI_BIN", "pi")

	profiles, err := discoverProfiles(profileDir)
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		return fmt.Errorf("no profiles found under %s", profileDir)
	}
	sort.Strings(profiles)

	logger := log.New(os.Stderr, "[gateway] ", log.LstdFlags|log.Lmsgprefix)
	logger.Printf("profiles: %s", strings.Join(profiles, ", "))

	// Taught skills (#314), opened before Pi starts: the runtime dir must
	// hold this boot's files before the first --skill argument points at
	// it, or a fresh deploy boots skill-less. Same database, sibling
	// collection. Two degraded modes, both logged: an immediate open failure
	// serves an in-process store (activations die with the process), while a
	// lazy open failure — unreachable Mongo surfacing at first use — 502s
	// the routes with the cause in the log instead.
	var teachStore teach.Store
	if store, err := teach.FromEnv(); err != nil {
		logger.Printf("teach: falling back to in-process (activations do not survive restart): %v", err)
		teachStore = teach.NewMemoryStore()
	} else {
		teachStore = store
		// Background, not the signal ctx: it does not exist yet this early
		// in startup, and these are bounded one-shot calls anyway.
		indexCtx, cancelIndex := context.WithTimeout(context.Background(), 10*time.Second)
		if err := store.EnsureIndexes(indexCtx); err != nil {
			logger.Printf("teach: index setup failed: %v", err)
		}
		cancelIndex()
	}

	// Startup reconcile runs here, not later, for the reason above: Mongo
	// is the source of truth and the runtime dir is its cache. Profiles
	// reconcile concurrently under one shared budget: serial 10s Lists would
	// add 10s of boot per slow profile.
	{
		matCtx, cancelMat := context.WithTimeout(context.Background(), 10*time.Second)
		var matWG sync.WaitGroup
		for _, profile := range profiles {
			matWG.Add(1)
			go func(profile string) {
				defer matWG.Done()
				skills, err := teachStore.List(matCtx, profile)
				if err != nil {
					logger.Printf("teach: startup list failed for %s: %v", profile, err)
					return
				}
				n, skipped, err := teach.Materialize(teach.RuntimeDir(agentDir, profile), skills)
				if err != nil {
					logger.Printf("teach: startup materialize failed for %s: %v", profile, err)
				}
				if n > 0 {
					logger.Printf("teach: materialized %d skill(s) for %s", n, profile)
				}
				if len(skipped) > 0 {
					logger.Printf("teach: skipped unrenderable skill(s) for %s: %v", profile, skipped)
				}
			}(profile)
		}
		matWG.Wait()
		cancelMat()
	}

	// Crash files live under the bind-mounted agent dir, so they survive
	// container restarts (#290). Self-cleaning: files older than the TTL
	// are deleted here, at startup, and after each write.
	crashDir := filepath.Join(agentDir, "logs")
	if err := crashlog.Prune(crashDir); err != nil {
		logger.Printf("crash log prune failed: %v", err)
	}

	readyTimeout := secondsEnv("PI_READY_TIMEOUT", 60)
	defaultModel := envOr("PI_DEFAULT_MODEL", "opencode-go/mimo-v2.6-flash")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agents := make(map[string]gateway.Agent, len(profiles))
	closers := make([]func(), 0, len(profiles))

	for _, profile := range profiles {
		dir := filepath.Join(profileDir, profile)

		// Pi args build in two steps because Go allows only one spread per
		// append call: the committed skills first, then the taught-skills
		// runtime dir (nil when it holds no valid skill, so a profile with
		// nothing taught starts unchanged).
		piArgs := append(
			[]string{"--model", defaultModel, "--session-dir", filepath.Join(agentDir, "sessions")},
			// Per-profile skills, named explicitly.
			//
			// Pi does NOT discover a bare skills/ directory in the working
			// directory. Measured with one distinctly-named skill per candidate
			// location and PI_CODING_AGENT_DIR set: <agent-dir>/skills/ is found,
			// <cwd>/skills/ is not, and <cwd>/.agents/skills/ is not either
			// (project resources need trust, which a headless RPC session never
			// grants). So --skill <path> is the only route to skills that differ
			// per profile, and it is repeatable.
			//
			// Only added when the profile has at least one valid skill (a subdirectory
			// containing a SKILL.md): passing --skill at a path with no skill in it
			// is at best inert and at worst an error, and a skill-less profile must
			// still start. No count of which profiles have skills, because that
			// goes stale the moment someone adds one.
			skillArgs(profileSkillDir(dir))...,
		)
		piArgs = append(piArgs,
			skillArgs(teach.RuntimeDir(agentDir, profile))...,
		)

		client, err := pi.Start(ctx, pi.Options{
			Bin:  piBin,
			Dir:  dir,
			Args: piArgs,
			// Env replaces rather than merges, so the parent environment is copied
			// explicitly — setting it to just the provider key would leave the child
			// without PATH and the exec failure would look unrelated.
			//
			// PASSWORD is then stripped back out: the gateway authenticates to the
			// app, but the agent has no use for the credential and it has no business
			// holding one. A leaked key in an agent's environment is readable by
			// anything the agent runs, including tool output.
			Env: pi.EnvWith(
				pi.EnvironOrOs(withoutEnv(os.Environ(), "PASSWORD")),
				map[string]string{
					"PI_CODING_AGENT_DIR": agentDir,
					// The memory MCP server is one binary shared by every
					// profile, so the profile has to travel with the process:
					// Pi spawns its MCP servers as children, which inherit
					// this environment. Without it the memory server would
					// guess a profile and write one profile's facts into
					// another's store (#315).
					"AGENTO_PROFILE": profile,
				},
			),
			ReadyTimeout: readyTimeout,
		})
		if err != nil {
			// One unusable profile must not take down the others; a gateway that
			// serves two working profiles is more useful than none.
			//
			// The returned client is nil on failure, so the diagnostics Pi
			// collected have to come from the error itself — reaching for
			// client.Stderr() here would be a nil dereference, and it is exactly
			// the start-up failure whose stderr matters most.
			// Earlier profiles stay up. Closing them here would poison working
			// profiles with a later failure, and the deferred close would then hit
			// them a second time.
			logger.Printf("profile %s: agent did not start: %v", profile, err)
			continue
		}
		agents[profile] = client
		// Bound explicitly: the closure must close THIS profile's client, and
		// relying on the loop variable for that is a trap the next edit falls into.
		c := client
		closers = append(closers, func() { _ = c.Close() })

		// Surface an agent that dies later, which otherwise looks like a gateway
		// that has silently gone quiet. The stderr tail already captured
		// for the log line is also written to a crash file (#290), so a
		// mid-turn death leaves a trace with the same TTL as the rest.
		go func(profile string, c *pi.Client) {
			<-c.Done()
			if exitErr := c.ExitError(); exitErr != nil {
				tail := gateway.Tail(c.Stderr(), gateway.StderrTailLimit)
				logger.Printf("profile %s: agent exited: %v (stderr tail: %s)",
					profile, exitErr, tail)
				detail := fmt.Sprintf("component: agent %s\nexit: %v\nstderr tail:\n%s",
					profile, exitErr, tail)
				secrets := []string{password}
				for _, t := range strings.Split(os.Getenv("PASSWORD"), ",") {
					if t = strings.TrimSpace(t); t != "" {
						secrets = append(secrets, t)
					}
				}
				secrets = append(secrets, os.Getenv("MONGODB_URI"))
				path, err := crashlog.WriteCrash(crashDir, detail, secrets...)
				if err != nil {
					logger.Printf("profile %s: crash log write failed: %v", profile, err)
				} else {
					logger.Printf("profile %s: crash log: %s", profile, path)
					_ = crashlog.Prune(crashDir)
				}
			}
		}(profile, client)
	}

	defer func() {
		for _, c := range closers {
			c()
		}
	}()

	if len(agents) == 0 {
		return errors.New("no profile produced a working agent")
	}

	// Cron jobs live in Mongo next to the other domain collections. A
	// store failure must not take chat down: without it the jobs routes
	// answer 404 and the Scheduler stays idle, loudly logged.
	//
	// Only the store opens here. Fires run as chat-agent turns (#331), so
	// the worker's Fire function is wired after the gateway server exists
	// below; the tick loop starts there too.
	var cronStore gateway.JobStore
	var cronWorker *cronjobs.Worker
	if store, err := cronjobs.FromEnv(); err != nil {
		logger.Printf("cron jobs disabled: %v", err)
	} else {
		cronStore = store
		cronWorker = &cronjobs.Worker{Store: store, Logf: logger.Printf}
	}
	var cronFire gateway.CronFire
	if cronWorker != nil {
		// Bound to the worker, not to its Fire field: Fire is assigned
		// after the server exists (see below), and the method value reads
		// it at call time. Nothing serves requests before ListenAndServe,
		// which happens after that assignment — do not move serving
		// earlier without moving the assignment too. Manual /run shares
		// the tick path, so a hand tap and a scheduled fire can never
		// overlap a job.
		cronFire = cronWorker.FireNow
	}

	// Conversation-attached runs (#311). Same Mongo database as the rest of
	// the domain data, a sibling collection to cron jobs. Without it, runs
	// still outlive their request in this process, but a restart loses the
	// history — so a store failure is logged and the gateway keeps serving.
	var runStore runs.Store
	if store, err := runs.FromEnv(); err != nil {
		logger.Printf("run history: falling back to in-memory: %v", err)
		runStore = runs.NewMemoryStore()
	} else {
		runStore = store
		store.Logf = logger.Printf
		indexCtx, cancelIndex := context.WithTimeout(ctx, 10*time.Second)
		if err := store.EnsureIndexes(indexCtx); err != nil {
			logger.Printf("run history: index setup failed (records will not auto-expire): %v", err)
		}
		cancelIndex()
		// Execution cannot survive a process restart. Mark what was running
		// before serving, so the list never claims a dead run is in flight;
		// the output produced before the restart stays readable. Bounded like
		// the index setup above: a wedged database must not hang startup
		// forever.
		markCtx, cancelMark := context.WithTimeout(ctx, 10*time.Second)
		if n, err := store.MarkInterrupted(markCtx, time.Now()); err != nil {
			logger.Printf("run history: could not mark interrupted runs: %v", err)
		} else if n > 0 {
			logger.Printf("run history: marked %d in-flight run(s) interrupted after restart", n)
		}
		cancelMark()
	}

	// Approval gates + audit trail (#313). Same database, sibling
	// collections to runs. Without it the gateway answers approvals from
	// memory, and the relay falls back independently too — but a decision
	// then needs a SHARED store: with no Mongo the relay denies gated calls
	// immediately (relay_unavailable — a hold could never be decided), so the
	// failure is fast and audited rather than a 14-minute hang
	// (Store.Shared documents the same contract).
	var approvalStore approvals.Store
	if store, err := approvals.FromEnv(); err != nil {
		logger.Printf("approvals: falling back to in-memory: %v", err)
		mem := approvals.NewMemoryStore()
		mem.Logf = logger.Printf
		approvalStore = mem
	} else {
		approvalStore = store
		store.Logf = logger.Printf
		indexCtx, cancelIndex := context.WithTimeout(ctx, 10*time.Second)
		if err := store.EnsureIndexes(indexCtx); err != nil {
			logger.Printf("approvals: index setup failed (records will not auto-expire): %v", err)
		}
		cancelIndex()
		// A restart orphans the relay that owned every pending wait.
		// Expire them before serving so the list never shows a pending row
		// nothing will ever decide.
		markCtx, cancelMark := context.WithTimeout(ctx, 10*time.Second)
		if n, err := store.MarkInterrupted(markCtx, time.Now()); err != nil {
			logger.Printf("approvals: could not expire pending rows: %v", err)
		} else if n > 0 {
			logger.Printf("approvals: marked %d pending row(s) expired after restart", n)
		}
		cancelMark()
	}

	// Memory (#315). Same database, sibling collections. Without Mongo the
	// routes still work from an in-process store, but nothing survives a
	// restart and the memory MCP server (a separate process) cannot see it —
	// so an agent-proposed item would be invisible to the screen that has to
	// confirm it. Log that plainly rather than silently degrade.
	var memoryStore memory.Store
	if store, err := memory.FromEnv(); err != nil {
		logger.Printf("memory: falling back to in-process (not durable, agent proposals not shared): %v", err)
		memoryStore = memory.NewMemoryStore()
	} else {
		memoryStore = store
		store.Logf = logger.Printf
		indexCtx, cancelIndex := context.WithTimeout(ctx, 10*time.Second)
		if err := store.EnsureIndexes(indexCtx); err != nil {
			logger.Printf("memory: index setup failed: %v", err)
		}
		cancelIndex()
	}

	// The test-fire runner is a bare model client: it needs the same key and
	// model but no job store, so a Mongo-less stack still answers "no runner"
	// honestly instead of failing open. Scheduler fires do NOT use this path:
	// they run as chat-agent turns (wired above), so they can read notes and
	// fetch live pages at fire time.
	var teachRunner gateway.TeachRunner
	if key := strings.TrimSpace(os.Getenv("OPENCODE_API_KEY")); key != "" {
		runner := &cronjobs.BareClient{
			APIKey: key,
			// Renamed with fallback: PI_CRON_MODEL predates the split
			// (scheduler fires are chat turns now); existing deploys
			// keep working, new ones use the honest name.
			Model: envOr("PI_TEACH_MODEL", envOr("PI_CRON_MODEL", "mimo-v2.6-flash")),
		}
		teachRunner = runner.Run
	} else {
		logger.Printf("teach: no OPENCODE_API_KEY, skill tests unavailable")
	}

	// First connectors (#316). Same database, sibling collection. Without
	// Mongo the routes still answer from memory, but statuses die with the
	// process and no OAuth token can be stored anyway.
	var connectorStore connectors.Store
	if store, err := connectors.FromEnv(); err != nil {
		logger.Printf("connectors: falling back to in-process (statuses do not survive restart): %v", err)
		connectorStore = connectors.NewMemoryStore()
	} else {
		connectorStore = store
		store.Logf = logger.Printf
		// The states TTL: pending OAuth attempts reap themselves. Kept
		// explicit (not a no-op): without this index consumed-but-unread
		// rows would linger past their window. A failure here is loud on
		// purpose — without the TTL, abandoned states accumulate with no
		// sweeper (memory relies on the CreateState opportunistic sweep,
		// Mongo relies on THIS index), so triage must hear about it at
		// every startup, not once in a scrolled-past line.
		indexCtx, cancelIndex := context.WithTimeout(ctx, 10*time.Second)
		defer cancelIndex()
		if err := store.EnsureIndexes(indexCtx); err != nil {
			logger.Printf("connectors: WARNING index setup failed, oauth states will not reap: %v", err)
		}
	}

	// Google OAuth client (#316, second layer). ANY piece missing (client
	// id, secret, redirect URL, envelope key) leaves this nil and connect
	// answers 501 exactly like the v1 foundation — a half-open flow would
	// strand the user mid-consent. The store gate uses the Store.Durable()
	// contract, not a concrete type-assert: OAuth without durability would
	// store grants in memory that dies with the process, so a non-durable
	// store forces nil here too.
	var connectorOAuth *connectors.OAuthConfig
	if connectorStore.Durable() {
		if cfg, err := connectors.OAuthConfigFromEnv(); err != nil {
			// Echo the attempted redirect: non-secret, and the triage
			// pointer when the failure is a registration mismatch.
			// Bounded like the gateway's bounded(): the cap fires inside
			// CanonicalRedirectURL, so an uncapped env value would emit
			// megabytes to the log before any bound applies.
			redirect := os.Getenv("GOOGLE_REDIRECT_URL")
			if r := []rune(redirect); len(r) > 128 {
				redirect = string(r[:128]) + "\u2026 (truncated)"
			}
			logger.Printf("connectors: OAuth unavailable (%v; redirect %q); connect answers 501", err, redirect)
		} else {
			connectorOAuth = cfg
			// The redirect URL is not a secret — log it so the operator can
			// compare byte-for-byte against the Cloud console registration.
			// A mismatch dies in the browser (redirect_uri_mismatch), not
			// here, so this line is the triage pointer.
			// The connector list derives from the package source of truth,
			// not a literal, so this triage line cannot drift the next time
			// an id lands or drops.
			logger.Printf("connectors: OAuth configured for %s (callback %q)", strings.Join(connectors.ListIDs(), ", "), cfg.RedirectURL)
		}
	} else {
		logger.Printf("connectors: OAuth unavailable (no durable store); connect answers 501")
	}

	defaultProfile := envOr("PI_DEFAULT_PROFILE", "default")
	if _, ok := agents[defaultProfile]; !ok {
		// Fall back to the first profile that actually STARTED, not the first one
		// discovered. Using the discovery order meant a failure on the
		// alphabetically-first profile made the gateway refuse to serve even though
		// every other profile was healthy — the exact case this fallback exists for.
		names := make([]string, 0, len(agents))
		for name := range agents {
			names = append(names, name)
		}
		sort.Strings(names)
		logger.Printf("default profile %q not available; using %q (of %v)",
			defaultProfile, names[0], names)
		defaultProfile = names[0]
	}

	srv, err := gateway.New(gatewayConfig(password, agentDir, defaultProfile, crashDir, cronStore, cronFire, runStore, approvalStore, memoryStore, teachStore, teachRunner, connectorStore, connectorOAuth, logger.Printf), agents)
	if err != nil {
		return err
	}

	// Scheduler fires run as chat-agent turns on this server (#331), so the
	// worker is wired — and its tick loop started — only now that the server
	// exists. One tick a minute plus one immediate pass: the immediate pass
	// accounts fires missed while down (skip-and-note) instead of leaving
	// LastFire stale until the next minute.
	if cronWorker != nil {
		cronWorker.Fire = func(ctx context.Context, job cronjobs.Job) (cronjobs.FireResult, error) {
			text, runID, err := srv.FireSchedulerJob(ctx, job)
			// The run id travels even on failure: a failed chat turn
			// still exists in the Runs list, and the error row links it.
			if err != nil {
				return cronjobs.FireResult{RunID: runID}, err
			}
			return cronjobs.FireResult{Text: text, RunID: runID}, nil
		}
		worker := cronWorker
		go func() {
			worker.Tick(ctx, time.Now())
			tick := time.NewTicker(time.Minute)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-tick.C:
					worker.Tick(ctx, now)
				}
			}
		}()
	}

	// PI_GATEWAY_ADDR wins outright, and PI_SERVER_PORT is then not consulted at
	// all. That precedence is deliberate — the address form is more specific — but
	// it means a set-but-ignored port is an otherwise invisible deploy failure: the
	// container listens somewhere other than where the deploy config said. Say which
	// source won, once, at startup.
	addr := envOr("PI_GATEWAY_ADDR", ":"+envOr("PI_SERVER_PORT", "8643"))
	if from := os.Getenv("PI_GATEWAY_ADDR"); from != "" {
		logger.Printf("pi-gateway: listening on %s (from PI_GATEWAY_ADDR; PI_SERVER_PORT is ignored when it is set)", addr)
	} else {
		logger.Printf("pi-gateway: listening on %s (from PI_SERVER_PORT, default 8643)", addr)
	}
	httpSrv := &http.Server{
		Addr: addr,
		// No WriteTimeout: an assistant turn legitimately outlives any fixed
		// limit, and SSE keeps the connection open for the whole turn. The turn
		// timeout in the gateway bounds that instead — and it starts only AFTER the
		// body is decoded, which is why the read side needs its own bound.
		//
		// ReadTimeout covers the whole request read, headers and body. A client
		// that dribbles a body in holds a connection outside every other limit here,
		// so it is bounded explicitly. 30s is generous for a 4 MiB cap.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Bounds keep-alive connections between requests, which would otherwise
		// sit open indefinitely holding a slot.
		IdleTimeout: 120 * time.Second,
		Handler:     srv,
	}

	go func() {
		<-ctx.Done()
		logger.Printf("shutting down")
		// A short drain, deliberately. TurnTimeout is minutes, so waiting for
		// in-flight turns here would mean a deploy blocks for as long as the
		// longest conversation. A turn interrupted by SIGTERM ends in the app as a
		// dropped stream and resumes from its session next time; blocking shutdown
		// for that would be worse. Raise it if turns ever need to complete.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Printf("listening on %s for profiles %v", addr, srv.Profiles())
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// discoverProfiles lists directories under dir, each one a Pi profile.
//
// A directory qualifies when it holds an AGENTS.md: that file is what makes a
// directory a profile rather than a stray folder, and its presence is what the
// profile-config PR adds for each profile.
//
// A directory named "v1" is skipped. Routing decides "no profile segment" from the
// first path segment being the API version, so such a profile would be
// unreachable: every request would be read as the default profile instead. Skipping
// it here surfaces the mistake at startup rather than serving it silently wrong.
func discoverProfiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("profile directory %s does not exist", dir)
		}
		return nil, err
	}

	var profiles []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "AGENTS.md")); err != nil {
			// Any failure skips the profile, but they are not equally likely to be
			// intended. A missing file is the normal case — a directory without an
			// AGENTS.md is not a profile. Anything else (permissions, a broken
			// symlink, an I/O error) means the profile probably was meant to be
			// served and cannot be, and staying silent there turns into "the profile
			// is missing" after a deploy that quietly dropped it.
			if !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr,
					"[gateway] skipping profile %q: cannot read AGENTS.md: %v\n",
					e.Name(), err)
			}
			continue
		}
		if gateway.IsReservedProfileName(e.Name()) {
			// Skipped rather than served: routing reads a leading segment matching a
			// reserved name as "no profile", so this agent would start, hold a
			// process, and be unreachable from every request.
			fmt.Fprintf(os.Stderr,
				"[gateway] skipping profile %q: %q is reserved as a path segment (%s)\n",
				e.Name(), e.Name(), strings.Join(gateway.ReservedProfileNames, ", "))
			continue
		}
		profiles = append(profiles, e.Name())
	}
	return profiles, nil
}

// withoutEnv returns base with every entry for key removed, case-insensitively on
// the name. Used to keep a secret out of a child process's environment.
func withoutEnv(base []string, key string) []string {
	out := make([]string, 0, len(base))
	for _, e := range base {
		name, _, ok := strings.Cut(e, "=")
		if ok && strings.EqualFold(name, key) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// profileSkillDir is where a profile's own skills live: <profileDir>/skills.
//
// Kept as a named function so the path convention has one definition, and so the
// existence check and the argument below cannot disagree about it.
func profileSkillDir(profileDir string) string {
	return filepath.Join(profileDir, "skills")
}

// skillArgs returns --skill arguments for the skills a profile actually has.
//
// Returns nothing when the profile has none: Pi errors on a --skill path that does not
// exist, so an unconditional argument would stop a skill-less profile from starting.
// No count of how many profiles are in that state — it goes stale the moment someone
// adds a skill.
//
// "Has some" means at least one immediate subdirectory contains a SKILL.md, not merely
// that the directory has entries. A stray README or .DS_Store in skills/ would
// otherwise opt a profile into --skill pointing at a directory with no valid skill in
// it, and what Pi does with that is untested — so the question is not asked.
func skillArgs(skillsDir string) []string {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		// Absent is the expected case and needs no comment. Anything else is a
		// misconfiguration — a permissions or I/O problem — and silently running
		// without skills hides it, so it is reported at startup rather than discovered
		// later as a profile that mysteriously lost its skills.
		if !os.IsNotExist(err) {
			warnf("cannot read skills directory %s: %v", skillsDir, err)
		}
		return nil
	}
	// The loop does not return on the first skill it finds. ReadDir sorts by name, so
	// an early return meant a broken skill sorting after a healthy one was never
	// visited and never reported — the profile loaded fine and the operator was never
	// told part of it would not be there.
	found := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		switch _, err := os.Stat(filepath.Join(skillsDir, e.Name(), "SKILL.md")); {
		case err == nil:
			found = true
		case !os.IsNotExist(err):
			// Same reasoning as the ReadDir above, applied to the per-skill check. A
			// subdirectory that exists but whose SKILL.md cannot be stat'd — a
			// permissions problem, say — is a real skill this gateway would silently
			// not load. Reporting it keeps that failure the same shape as every other
			// "configured but unreachable" case here: said once, at startup, rather
			// than discovered later as a skill that mysteriously stopped working.
			//
			// Only non-IsNotExist errors reach this: a subdirectory without a
			// SKILL.md is not a skill and is not an error, and warning about each one
			// would bury the real failures.
			warnf("cannot stat %s: %v", filepath.Join(skillsDir, e.Name(), "SKILL.md"), err)
		}
	}
	if !found {
		return nil
	}
	// The parent is passed once rather than one --skill per skill: Pi walks a --skill
	// directory recursively, so it is equivalent and keeps the argument list fixed
	// regardless of how many skills a profile gains.
	return []string{"--skill", skillsDir}
}

// gatewayConfig maps the environment onto the gateway's configuration.
//
// Extracted from run() so the env-to-config mapping is testable. It was inline, and
// nothing verified that a given variable reached the field it configures: removing the
// InventoryTimeout line left every test green, because the test exercised secondsEnv
// directly and never the wiring. A budget that cannot be shown to reach its field is
// not configurable in any useful sense.
func gatewayConfig(password, agentDir, defaultProfile, crashDir string, cronStore gateway.JobStore, cronFire gateway.CronFire, runStore runs.Store, approvalStore approvals.Store, memoryStore memory.Store, teachStore teach.Store, teachRunner gateway.TeachRunner, connectorStore connectors.Store, connectorOAuth *connectors.OAuthConfig, logf func(string, ...any)) gateway.Config {
	return gateway.Config{
		Password: password,
		// The toolsets inventory reads <agentDir>/mcp.json, so the agent dir has to
		// reach the gateway as well as the Pi children. run() has already resolved it
		// and it defaults to /opt/pi.
		AgentDir:       agentDir,
		DefaultProfile: defaultProfile,
		// Crash files land under the agent dir (bind-mounted, so they
		// survive container restarts). Resolved in run(), not derived in
		// New, so the mapping stays testable like every other field.
		CrashDir: crashDir,
		// Cron backend and manual-run entry, both owned by run(): the
		// store needs env and the fire path needs the worker.
		CronJobs: cronStore,
		CronFire: cronFire,
		// Resumable run history, owned by run(): it needs env and the
		// startup reconciliation that marks dead runs interrupted.
		RunStore: runStore,
		// Approval + audit stores, owned by run() for the same reason.
		ApprovalStore: approvalStore,
		// Per-profile memory, owned by run() for the same reason.
		MemoryStore: memoryStore,
		// Taught skills, owned by run() for the same reason. The runner is
		// the cron runner's sibling (same key/model, no job store); the
		// materializer closes over the agent dir to reach the runtime tree.
		TeachStore:  teachStore,
		TeachRunner: teachRunner,
		TeachMaterialize: func(profile string, skills []teach.Skill) (int, []string, error) {
			return teach.Materialize(teach.RuntimeDir(agentDir, profile), skills)
		},
		// First connectors (#316), owned by run() for the same reason.
		// Nil store means gateway.New falls back to memory; nil OAuth
		// means connect answers 501 like the v1 foundation.
		ConnectorStore: connectorStore,
		ConnectorOAuth: connectorOAuth,
		// Each budget is configurable for the same reason: the right value depends on
		// the host, and changing it should not mean rebuilding the binary.
		TurnTimeout:       secondsEnv("PI_TURN_TIMEOUT", 600),
		KeepaliveInterval: secondsEnvAllowDisabled("PI_KEEPALIVE", 20),
		InventoryTimeout:  secondsEnv("PI_INVENTORY_TIMEOUT", 30),
		Logf:              logf,
	}
}

// secondsEnv reads a seconds-valued environment variable.
//
// An unparsable or non-positive value falls back to the default AND says so: a
// typo like PI_TURN_TIMEOUT=abc would otherwise be indistinguishable from a
// deliberate default, and would quietly use the wrong timeout.
func secondsEnv(key string, fallback int) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return time.Duration(fallback) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		warnf("%s=%q is not a positive number of seconds; using %ds", key, v, fallback)
		return time.Duration(fallback) * time.Second
	}
	return time.Duration(n) * time.Second
}

// secondsEnvAllowDisabled is secondsEnv except that an explicit 0 means "off"
// rather than "bad value". Used for PI_KEEPALIVE, whose documented contract is
// that 0 disables the comments — secondsEnv would have warned about 0 and used
// the 20s default, so the documented setting silently did nothing.
//
// Negative stays an error, because "turn keepalives off every -1 seconds" is a
// configuration mistake rather than a request to disable them.
func secondsEnvAllowDisabled(key string, fallback int) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return time.Duration(fallback) * time.Second
	}
	n, err := strconv.Atoi(v)
	switch {
	case err != nil:
		warnf("%s=%q is not a number of seconds; using %ds", key, v, fallback)
		return time.Duration(fallback) * time.Second
	case n > 0:
		return time.Duration(n) * time.Second
	case n == 0:
		return gateway.KeepaliveDisabled
	default:
		warnf("%s=%q is negative; using %ds", key, v, fallback)
		return time.Duration(fallback) * time.Second
	}
}

// warnf reports a configuration problem before the logger exists.
var warnf = func(format string, args ...any) {
	log.Printf("[gateway] warning: "+format, args...)
}

// tail trims s to at most n characters, keeping the end, which is where a crash
// explanation lives.
//
// Sliced by rune, not byte: stderr carries multi-byte text and a byte slice can
// cut a character in half, leaving a replacement character in a diagnostic.
