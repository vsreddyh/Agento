// Command browser is the web-browsing MCP server (#312).
// Storage: none of its own — sessions persist as Chromium profile dirs,
// evidence lands in the exports tree, and the navigation log is the
// mcp-gate audit trail (every call's args carry its URL).
// Runs over stdio for MCP clients.
//
// What this is for: read-only web tasks the agent can cite — open a page,
// read it, screenshot it, save the evidence bundle. What it is NOT for:
// logging in anywhere (v1 has no click/type tools, so a password can never
// be pasted — a login wall stops the task with a report instead), buying
// things, or fetching from non-public addresses (fail closed, see checkURL).
//
// Every navigation holds for approval (the policy table gates browser_goto):
// the human approves each domain with the URL in front of them. Reads,
// screenshots and evidence saves run free once the page is approved.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chromedp/chromedp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// navTimeout bounds one navigation: the turn's own timeout bounds the
	// task, this bounds the page.
	navTimeout = 30 * time.Second
	// readTimeout bounds one read or screenshot the same way.
	readTimeout = 30 * time.Second
	// MaxReadRunes bounds extracted page text. Long enough for an article,
	// short enough for a prompt; the response says when it clipped. The JS
	// pre-cap fast path compares UTF-16 units (t.length <= cap needs no
	// array); units never exceed code points, so a short string skips the
	// Array.from spread and only long ones pay for code-point slicing.
	MaxReadRunes = 12000
)

var (
	// sessionTTL prunes idle browser profiles: cookies and state older than
	// this with no fresh use go away on server start. A var (not const) so
	// tests can force the idle path without waiting 30 days.
	sessionTTL = 30 * 24 * time.Hour
	store      *sessions
	profile    string
	exports    string
)

// fail reports an operational error on both channels, exactly like
// result() does for validation errors: Pi surfaces Content as the tool
// result text, so a Content-less failure would read as a blank tool result
// in the transcript while the structured map held the reason.
func fail(err error) (*mcp.CallToolResult, map[string]any, error) {
	out := map[string]any{"ok": false, "error": err.Error()}
	b, _ := json.Marshal(out)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: out,
	}, out, nil
}

func result(out map[string]any) (*mcp.CallToolResult, map[string]any, error) {
	b, _ := json.Marshal(out)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: out,
	}, out, nil
}

// checkURL validates a navigation target. Only http(s), no credentials, no
// non-public addresses: the container shares a network with cloud metadata
// and local services, and a browser that fetches those is an SSRF proxy no
// approval sheet can see (the URL would read as harmless text). Anything
// these rules reject fails here, before Chromium ever dials.
func checkURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad url: %v", err)
	}
	// Lowercased: url.Parse preserves case and "HTTPS://x" would otherwise
	// fail closed with a confusing message about a scheme that looks right.
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("only http and https urls are allowed, not %q", u.Scheme)
	}
	// Normalize in place: what the gate approved matches what Chromium
	// dials and what the audit log records, modulo URL case (the audit keeps
	// the raw arg, Chromium gets the lowercased scheme).
	u.Scheme = scheme
	// Web ports only: the approval gate is per-domain, not per-port, and a
	// https://host:22/ URL would otherwise buy a raw TCP tunnel the human
	// never approved as anything but a web page.
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return nil, fmt.Errorf("only ports 80 and 443 are allowed, not %q", port)
	}
	if u.User != nil {
		return nil, fmt.Errorf("urls with credentials are not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("url has no host")
	}
	// Bounded: the tool call must not hang past navTimeout on a dead
	// resolver before Chromium is even involved.
	resCtx, resCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resCancel()
	ips, err := net.DefaultResolver.LookupIP(resCtx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("cannot resolve %q", host)
	}
	// Any-may-pass, like the proxy's dial: a multi-record host with one
	// stale/parked A record still browses via its public addresses (the
	// dial checks each connection afresh). Rejecting on any-bad would
	// refuse CDNs for one dead record while the proxy could serve them.
	for _, ip := range ips {
		if publicIP(ip) {
			return u, nil
		}
	}
	return nil, fmt.Errorf("no public address for %q", host)
}

// denyCIDRs is the explicit no-browse list. Method checks (IsPrivate and
// friends) depend on the Go version's tables — CGNAT and TEST-NET entries
// arrived late — so the ranges that matter are spelled out and pinned by
// TestPublicIPRejectsNonPublic instead of inherited.
var denyCIDRs = []string{
	"127.0.0.0/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"100.64.0.0/10",
	"169.254.0.0/16", "fe80::/10",
	"224.0.0.0/4", "ff00::/8",
	"0.0.0.0/8", "192.0.0.0/24",
	"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24",
	"240.0.0.0/4",
	"fc00::/7",
}

// denyNets is denyCIDRs parsed once at init: per-connection parsing would
// burn cycles on the hot path, and a typo'd CIDR must fail loud at startup,
// not silently skip per connection.
var denyNets = func() []net.IPNet {
	out := make([]net.IPNet, 0, len(denyCIDRs))
	for _, cidr := range denyCIDRs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("browser: bad deny CIDR " + cidr + ": " + err.Error())
		}
		out = append(out, *n)
	}
	return out
}()

func inDenyList(ip net.IP) bool {
	// Normalize first: an IPv4-mapped IPv6 address must match the v4
	// ranges, not slip past them in 16-byte form.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range denyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// publicIP reports whether ip is a safe navigation target: global unicast
// minus the explicit deny list above.
func publicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() {
		return false
	}
	return !inDenyList(ip)
}

// safeName keeps generated path segments inside their directory: the profile
// comes from the environment and evidence dirs from the clock, but defense
// here means a hostile value can never escape through Join.
var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func jailPath(base, name string) (string, error) {
	if !safeName.MatchString(name) {
		return "", fmt.Errorf("unsafe path segment %q", name)
	}
	return filepath.Join(base, name), nil
}

// session is one profile's persistent browser: one context, one page at a
// time, cookies and storage surviving across turns in the profile dir.
type session struct {
	mu     sync.Mutex
	alloc  context.Context
	cancel context.CancelFunc
	// starting/ready single-flight concurrent first use: the second caller
	// waits instead of launching a second Chromium.
	starting bool
	ready    chan struct{}
	// launchErr carries a failed launch to concurrent waiters so they
	// report the cause instead of a generic failure.
	launchErr error
	// lastUse is written in get() under the sessions lock; evictIdle reads
	// it under the same lock. Never touched from tool handlers directly.
	lastUse time.Time
	// browserCancel stops the tab context. Stored, not dropped: a session
	// that is ever torn down (idle prune, future close tool) cancels its
	// own context instead of leaking the tab.
	browserCancel context.CancelFunc
	browser       context.Context
	started       bool
	dir           string
	shots         int
	// proxyLn serves the filtering proxy for this session's Chromium. One
	// listener per session (not one global): the check needs no per-profile
	// state today, but a listener dying with its session keeps the failure
	// domain obvious in the log.
	proxyLn net.Listener
}

// sessions keys browser sessions by profile. The MCP server is one process
// shared like the memory server, so AGENTO_PROFILE scopes the same way.
type sessions struct {
	mu        sync.Mutex
	byProfile map[string]*session
	agentDir  string
	chrome    string
	// lastPrune throttles the lazy prune below: every tool call passes
	// through get, and a directory walk per call would be pure overhead.
	lastPrune time.Time
}

func (ss *sessions) get(profile string) (*session, error) {
	ss.mu.Lock()
	// Validated here, not just at startup: the signature invites
	// non-global callers, and collapsing bad inputs to one shared dir
	// would mix state across callers. Fail closed instead.
	if !safeName.MatchString(profile) {
		ss.mu.Unlock()
		return nil, fmt.Errorf("refusing unsafe profile %q", profile)
	}
	s, ok := ss.byProfile[profile]
	if !ok {
		s = &session{dir: filepath.Join(ss.agentDir, ".runtime", "browser", profile)}
		ss.byProfile[profile] = s
	}
	// Under s.mu as well as ss.mu: evictIdle's spare re-check reads
	// lastUse holding only s.mu, so a bare write here races it. Lock order
	// everywhere is ss.mu -> s.mu; this nests the same way.
	s.mu.Lock()
	s.lastUse = time.Now()
	s.mu.Unlock()
	// Touch the dir AND the context subdir: mtime is the only clock the
	// startup disk walk can read, and it stats ctx/ when present — touching
	// only the parent would leave a daily-used profile looking idle after
	// a restart. Failures ignored: this is a hint, not a write.
	now := time.Now()
	_ = os.Chtimes(s.dir, now, now)
	_ = os.Chtimes(filepath.Join(s.dir, "ctx"), now, now)
	// Eviction (live sessions, lastUse-aware) runs daily here. The
	// filesystem walk runs ONLY at server start (main): its mtime test
	// cannot tell a daily-used profile with a stable file set from an
	// abandoned one, and deleting a running browser's dir from under it is
	// exactly what evictIdle exists to prevent.
	// Prune outside the global lock: eviction does listener closes and
	// multi-second RemoveAlls that must not block other profiles' get().
	// This session just refreshed lastUse, so the prune cannot take it.
	prune := false
	if time.Since(ss.lastPrune) > 24*time.Hour {
		ss.lastPrune = time.Now()
		prune = true
	}
	ss.mu.Unlock()
	if prune {
		ss.evictIdle()
	}
	return s, nil
}

// evictIdle cancels sessions idle past the TTL: their context dirs go AND
// their Chromium + proxy listener stop. Disk-only pruning would leave a
// months-old browser running with its profile dir deleted from under it,
// and live browsers would accumulate one Chromium per touched profile with
// no idle close path (no close tool by design). Takes ss.mu only to
// snapshot the idle set and to delete afterwards: teardown IO (listener
// close, multi-second RemoveAll of a Chromium Cache) runs lock-free so one
// abandoned profile never blocks every other profile's get(). A session
// touched between snapshot and teardown is re-checked under its own lock
// and spared; delete re-checks map identity under ss.mu.
func (ss *sessions) evictIdle() {
	cutoff := time.Now().Add(-sessionTTL)
	ss.mu.Lock()
	type doomedSession struct {
		name string
		s    *session
	}
	var doomed []doomedSession
	for name, s := range ss.byProfile {
		s.mu.Lock()
		isIdle := !s.lastUse.After(cutoff)
		s.mu.Unlock()
		if isIdle {
			doomed = append(doomed, doomedSession{name, s})
		}
	}
	ss.mu.Unlock()
	for _, d := range doomed {
		s := d.s
		// Explicit unlock before IO, not defer: teardown below is slow.
		s.mu.Lock()
		if s.lastUse.After(cutoff) {
			// Touched while snapshotted: spared, not torn down.
			s.mu.Unlock()
			continue
		}
		log.Printf("browser: evicting idle session %s", d.name)
		if s.browserCancel != nil {
			s.browserCancel()
		}
		if s.cancel != nil {
			s.cancel()
		}
		if s.proxyLn != nil {
			s.proxyLn.Close()
		}
		dir := s.dir
		s.mu.Unlock()
		if dir != "" {
			os.RemoveAll(dir)
		}
		ss.mu.Lock()
		if ss.byProfile[d.name] == s {
			delete(ss.byProfile, d.name)
		}
		ss.mu.Unlock()
	}
}

// ensure launches Chromium on first use, not at server start: the entrypoint
// `pi mcp list` gate must stay fast, and a profile that never browses must
// never pay for a browser.
// starting guards concurrent first use: the second caller waits on ready
// instead of launching a second Chromium.
func (s *session) ensure(chrome string) (context.Context, error) {
	s.mu.Lock()
	if s.started {
		b := s.browser
		s.mu.Unlock()
		return b, nil
	}
	if s.starting {
		ch := s.ready
		s.mu.Unlock()
		<-ch
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.started {
			return s.browser, nil
		}
		// No retry here: N waiters relaunching on one failure is a
		// thundering herd on exactly the broken-binary path, and the next
		// tool call recovers anyway. launchErr carries the real cause
		// (kept fresh — publish clears it on every success).
		if s.launchErr != nil {
			return nil, s.launchErr
		}
		return nil, fmt.Errorf("concurrent browser launch failed")
	}
	s.starting = true
	s.ready = make(chan struct{})
	dir := s.dir
	s.mu.Unlock()

	bctx, btabCancel, balloc, bcancel, bln, berr := launchBrowser(dir, chrome)

	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.ready)
	s.starting = false
	if berr != nil {
		s.launchErr = berr
		return nil, berr
	}
	s.publish(bctx, btabCancel, balloc, bcancel, bln)
	return s.browser, nil
}

// publish installs a launched browser under the session lock: contexts,
// listener, cleared error and the resumed shot counter travel together, so
// the first-launch and retry paths cannot diverge again.
func (s *session) publish(bctx context.Context, btabCancel context.CancelFunc, balloc context.Context, bcancel context.CancelFunc, bln net.Listener) {
	// Cleared on success: a stale launch error must not outlive the retry
	// that fixed it for the next reader to trip over.
	s.launchErr = nil
	s.alloc, s.cancel, s.browserCancel, s.browser, s.started = balloc, bcancel, btabCancel, bctx, true
	s.proxyLn = bln
	// Resume the shot counter past restarts so a new process never
	// overwrites the previous run's shot-001.png.
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "shot-%d.png", &n); err == nil && n > s.shots {
			s.shots = n
		}
	}
}

// launchBrowser starts Chromium outside any session lock: a slow launch must
// not serialize reads and screenshots that do not need it yet. (They still
// wait on ready above — they just do not hold the lock while doing so.)
func launchBrowser(dir, chrome string) (bctx context.Context, btabCancel context.CancelFunc, balloc context.Context, bcancel context.CancelFunc, bln net.Listener, berr error) {
	ctxDir := filepath.Join(dir, "ctx")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("session dir: %v", err)
	}
	// Every byte through the filtering proxy: re-validated per connection,
	// so redirects and DNS rebindings face the same gate as the first URL.
	// Ephemeral loopback port, closed never (process-lifetime server), and
	// <-loopback bypass removal so even localhost goes through the check.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("proxy listen: %v", err)
	}
	// Serve errors log on exit: a dead listener makes Chromium fail
	// opaquely at the next navigation, and silence would hide the cause.
	go func() {
		if err := http.Serve(ln, &filteringProxy{check: publicIP}); err != nil {
			log.Printf("browser: proxy listener closed: %v", err)
		}
	}()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome),
		// Headless: there is no display in the container, and a headed
		// browser has no user to show it to (takeover is a documented
		// non-goal for v1).
		chromedp.Flag("headless", true),
		// Root in the container, matching the gateway it replaces: the
		// sandbox refuses to start as uid 0, so headless shell would exit
		// before the first navigation with no line explaining why.
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		// /dev/shm is small under podman/docker and full-page screenshots
		// are exactly the workload that crashes with "session deleted"
		// without this.
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.ProxyServer("http://"+ln.Addr().String()),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
		chromedp.UserDataDir(ctxDir),
	)
	// alloc is rooted at Background deliberately: the server is a
	// process-lifetime stdio process, and the browser must outlive any one
	// tool call's timeout. Process exit tears everything down at once.
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browser, browserCancel := chromedp.NewContext(alloc)
	// Prove launch now, not on the first approved navigation: a missing
	// chromium binary must fail the tool call that needs it, loudly, rather
	// than parking an approval for a browser that can never start.
	if err := chromedp.Do(browser, chromedp.Navigate("about:blank")); err != nil {
		cancel()
		ln.Close()
		return nil, nil, nil, nil, nil, fmt.Errorf("chromium launch failed: %v", err)
	}
	return browser, browserCancel, alloc, cancel, ln, nil
}

func (s *session) shotPath() string {
	// Locked: two concurrent screenshots for one profile must not share a
	// counter value and overwrite each other's PNG.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shots++
	return filepath.Join(s.dir, fmt.Sprintf("shot-%03d.png", s.shots))
}

type gotoInput struct {
	// Full http(s) URL. Holds for approval with this URL in front of the
	// human; public addresses only (see checkURL).
	URL string `json:"url"`
}

type readInput struct {
	// Cap on extracted text in runes. Defaults to MaxReadRunes; the response
	// says when it clipped.
	MaxChars int `json:"max_chars,omitempty"`
}

type shotInput struct {
	// Full-page capture instead of the viewport. Defaults to viewport.
	FullPage bool `json:"full_page,omitempty"`
}

// MaxNote bounds an evidence note in runes: a huge model note would land
// verbatim in page.md and meta.json for symmetry with the 12k read cap.
const MaxNote = 500

type evidenceInput struct {
	// One line on what this evidence shows, stored in meta.json and the
	// markdown header. Quote the finding rather than generalising it.
	Note string `json:"note,omitempty"`
}

func main() {
	// Same profile plumbing as the memory server: one binary shared by every
	// profile in mcp.json, so the profile travels with the process tree from
	// the gateway's Pi child environment.
	profile = strings.TrimSpace(os.Getenv("AGENTO_PROFILE"))
	if profile == "" {
		profile = "chat"
	}
	if !safeName.MatchString(profile) {
		log.Fatalf("browser: refusing unsafe profile %q", profile)
	}
	agentDir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR"))
	if agentDir == "" {
		agentDir = "/opt/pi"
	}
	exports = strings.TrimSpace(os.Getenv("BROWSER_EXPORTS"))
	if exports == "" {
		exports = "/exports/browser"
	}
	chrome := strings.TrimSpace(os.Getenv("CHROME_PATH"))
	if chrome == "" {
		chrome = "chromium"
	}
	pruneSessions(filepath.Join(agentDir, ".runtime", "browser"))
	store = &sessions{byProfile: map[string]*session{}, agentDir: agentDir, chrome: chrome}

	s := mcp.NewServer(&mcp.Implementation{Name: "browser", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "browser_goto",
		Description: "Open a page in the profile's persistent browser. Public http(s) only; login walls stop the call with a report (v1 cannot fill forms, so no password is ever pasted). Holds for approval with the URL shown."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in gotoInput) (*mcp.CallToolResult, map[string]any, error) {
			u, err := checkURL(in.URL)
			if err != nil {
				return result(map[string]any{"ok": false, "error": err.Error()})
			}
			sess, err := store.get(profile)
			if err != nil {
				return fail(err)
			}
			bctx, err := sess.ensure(store.chrome)
			if err != nil {
				return fail(err)
			}
			// Own budgets per step, like read and evidence: a slow
			// navigation must not starve the title read that proves where
			// it landed.
			if err := runStepVoid(bctx, navTimeout, func(ctx context.Context) error {
				return chromedp.Do(ctx, chromedp.Navigate(u.String()))
			}); err != nil {
				return fail(fmt.Errorf("navigation failed: %v", err))
			}
			title, err := runStep(bctx, readTimeout, func(ctx context.Context) (string, error) {
				return chromedp.Run(ctx, chromedp.Title())
			})
			if err != nil {
				return fail(fmt.Errorf("title read failed: %v", err))
			}
			// Actual landing URL, not the requested one: redirects
			// (http→https, trailing slashes, shortlinks) move the browser,
			// and citing the requested URL would cite the wrong page.
			landed, err := runStep(bctx, readTimeout, func(ctx context.Context) (string, error) {
				return chromedp.Run(ctx, chromedp.Location())
			})
			if err != nil {
				return fail(fmt.Errorf("location read failed: %v", err))
			}
			return result(map[string]any{
				"ok": true, "url": landed, "requested_url": u.String(), "title": title,
			})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "browser_read",
		Description: "Read the current page as text: URL, title and body content up to the cap. Use it after goto, or to re-read after the page changed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, map[string]any, error) {
			sess, err := store.get(profile)
			if err != nil {
				return fail(err)
			}
			bctx, err := sess.ensure(store.chrome)
			if err != nil {
				return fail(err)
			}
			// Own budgets per step, like evidence: location, title and
			// text each get 30s instead of sharing one deadline.
			// pageURL, not url: net/url stays usable in these closures
			// for any next edit that needs to parse.
			pageURL, err := runStep(bctx, readTimeout, func(ctx context.Context) (string, error) {
				return chromedp.Run(ctx, chromedp.Location())
			})
			if err == nil && (pageURL == "" || pageURL == "about:blank") {
				return result(map[string]any{"ok": false, "error": "no page yet: browser_goto first"})
			}
			if err != nil {
				return fail(fmt.Errorf("read failed: %v", err))
			}
			title, err := runStep(bctx, readTimeout, func(ctx context.Context) (string, error) {
				return chromedp.Run(ctx, chromedp.Title())
			})
			if err != nil {
				return fail(fmt.Errorf("read failed: %v", err))
			}
			text, err := runStep(bctx, readTimeout, func(ctx context.Context) (string, error) {
				return chromedp.Run(ctx,
					chromedp.Evaluate[string](`(() => { const t = document.body ? document.body.innerText : ""; if (t.length <= 100000) return t; return Array.from(t).slice(0, 100000).join(""); })()`))
			})
			if err != nil {
				return fail(fmt.Errorf("read failed: %v", err))
			}
			text = collapseLines(text)
			cap := in.MaxChars
			if cap <= 0 || cap > MaxReadRunes {
				cap = MaxReadRunes
			}
			truncated := false
			if utf8.RuneCountInString(text) > cap {
				text = string([]rune(text)[:cap])
				truncated = true
			}
			return result(map[string]any{
				"ok": true, "url": pageURL, "title": title,
				"text": text, "truncated": truncated,
			})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "browser_screenshot",
		Description: "Capture the current page as PNG into the session dir. Working file, not evidence: use browser_save_evidence for the exports bundle the Storage tab shows."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in shotInput) (*mcp.CallToolResult, map[string]any, error) {
			sess, err := store.get(profile)
			if err != nil {
				return fail(err)
			}
			bctx, err := sess.ensure(store.chrome)
			if err != nil {
				return fail(err)
			}
			var buf []byte
			if in.FullPage {
				buf, err = runStep(bctx, readTimeout, func(ctx context.Context) ([]byte, error) {
					return chromedp.Run(ctx, chromedp.FullScreenshot(90))
				})
			} else {
				buf, err = runStep(bctx, readTimeout, func(ctx context.Context) ([]byte, error) {
					return chromedp.Run(ctx, chromedp.CaptureScreenshot())
				})
			}
			if err != nil {
				return fail(fmt.Errorf("screenshot failed: %v", err))
			}
			path := sess.shotPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fail(err)
			}
			if err := os.WriteFile(path, buf, 0o644); err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "path": path})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "browser_save_evidence",
		Description: "Bundle the current page into exports: PNG screenshot + markdown + meta.json (URL, title, time, note) in a timestamped dir the Storage tab shows. Cite the returned paths and URLs in the answer."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in evidenceInput) (*mcp.CallToolResult, map[string]any, error) {
			sess, err := store.get(profile)
			if err != nil {
				return fail(err)
			}
			bctx, err := sess.ensure(store.chrome)
			if err != nil {
				return fail(err)
			}
			// Each step gets its own budget, not one shared deadline: a
			// slow page that spends the whole budget on the read would
			// flake the full-page screenshot — exactly the long pages
			// evidence exists for.
			pageURL, err := runStep(bctx, readTimeout, func(ctx context.Context) (string, error) {
				return chromedp.Run(ctx, chromedp.Location())
			})
			if err != nil {
				return fail(fmt.Errorf("evidence capture failed: %v", err))
			}
			title, err := runStep(bctx, readTimeout, func(ctx context.Context) (string, error) {
				return chromedp.Run(ctx, chromedp.Title())
			})
			if err != nil {
				return fail(fmt.Errorf("evidence capture failed: %v", err))
			}
			text, err := runStep(bctx, readTimeout, func(ctx context.Context) (string, error) {
				return chromedp.Run(ctx,
					chromedp.Evaluate[string](`(() => { const t = document.body ? document.body.innerText : ""; if (t.length <= 100000) return t; return Array.from(t).slice(0, 100000).join(""); })()`))
			})
			if err != nil {
				return fail(fmt.Errorf("evidence capture failed: %v", err))
			}
			buf, err := runStep(bctx, readTimeout, func(ctx context.Context) ([]byte, error) {
				return chromedp.Run(ctx, chromedp.FullScreenshot(90))
			})
			if err != nil {
				return fail(fmt.Errorf("evidence capture failed: %v", err))
			}
			// Created by evidenceDir (Mkdir + exist-retry): the path is
			// fresh, so no MkdirAll here — an idempotent create would
			// silently accept a colliding dir and clobber the earlier save.
			dir, err := evidenceDir()
			if err != nil {
				return fail(err)
			}
			text = collapseLines(text)
			if utf8.RuneCountInString(text) > MaxReadRunes {
				text = string([]rune(text)[:MaxReadRunes])
			}
			note := strings.TrimSpace(in.Note)
			if utf8.RuneCountInString(note) > MaxNote {
				note = strings.TrimSpace(string([]rune(note)[:MaxNote]))
			}
			md := "# " + orDefault(title, pageURL) + "\n\n"
			if note != "" {
				md += note + "\n\n"
			}
			md += "Source: " + pageURL + "\n\n" + text + "\n"
			files := map[string]string{}
			for name, data := range map[string][]byte{
				"shot.png": buf, "page.md": []byte(md),
			} {
				p := filepath.Join(dir, name)
				if err := os.WriteFile(p, data, 0o644); err != nil {
					return fail(err)
				}
				files[name] = p
			}
			meta, _ := json.Marshal(map[string]any{
				"profile": profile, "url": pageURL, "title": title,
				"at": time.Now().UnixMilli(), "note": note,
			})
			mp := filepath.Join(dir, "meta.json")
			if err := os.WriteFile(mp, meta, 0o644); err != nil {
				return fail(err)
			}
			files["meta.json"] = mp
			return result(map[string]any{"ok": true, "dir": dir, "files": files})
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "browser:", err)
		os.Exit(1)
	}
}

// runStep runs one browser action under its own timeout off the session
// context. Callers pass bctx (the unbounded session context), never an
// already-budgeted one: nesting timeouts would silently shrink the step.
// The budget is a parameter so navigation keeps navTimeout while reads use
// readTimeout, instead of one constant drifting from its comment.
func runStep[T any](bctx context.Context, budget time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(bctx, budget)
	defer cancel()
	return fn(ctx)
}

// runStepVoid is runStep for error-only actions (navigations): same budget,
// no value to thread through.
func runStepVoid(bctx context.Context, budget time.Duration, fn func(context.Context) error) error {
	_, err := runStep(bctx, budget, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}

// evSeq numbers evidence dirs within the process: timestamps collide
// under scripted loops and retries, counters do not (wrapping at a million
// is fine — the millisecond prefix already separates eras).
var evSeq atomic.Uint64

// fnv32 abbreviates a long profile for generated dirnames: 8 hex chars,
// enough that a shared 24-char prefix plus same-ms saves stay distinct.
func fnv32(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// collapseLines trims each line and collapses blank runs to one: paragraph
// breaks survive as a single blank line instead of merging everything into
// one newline-joined run.
func collapseLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, l := range lines {
		// Preserve leading indentation: code blocks, ASCII tables and
		// nested lists lose their structure if every line is TrimSpaced.
		// Only trailing whitespace goes; blank runs still collapse to one.
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, " \t\r"))
			blank = false
		} else if !blank && len(out) > 0 {
			out = append(out, "")
			blank = true
		}
	}
	// No trailing blank: the input's final newline is a terminator, not a
	// paragraph.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// evidenceDir makes a fresh timestamped evidence directory, jailed under the
// exports root. The clock value is generated, never client-supplied, and the
// profile passed the safe-name gate at startup.
func evidenceDir() (string, error) {
	// Millisecond precision: two saves in the same second must not share
	// a dir and silently overwrite each other's shot.png. The profile is
	// capped first: safeName allows 64 chars and the timestamp adds 20, so
	// a long-but-legal profile would fail its own jail below.
	// Hashed cap: two profiles sharing a 32-char prefix (legal) plus
	// same-ms saves (scripted) must not collide. 24 + 1 + 8 + 1 + 19 + 7
	// fits the 64-char jail with room; the per-process counter closes the
	// last hole a timestamp alone leaves open.
	p := profile
	if len(p) > 24 {
		p = p[:24] + "-" + fnv32(p)
	}
	// The dir is created here with Mkdir, not MkdirAll at the call site:
	// MkdirAll is idempotent, so a counter regression (wrap + clock
	// step-back) would return an existing dir and silently clobber the
	// earlier save. Mkdir fails on Exist and the loop retries with the next
	// counter value, so a returned path is always fresh.
	// exports already ends in /browser (default /exports/browser): the
	// timestamped dir hangs directly under it, never nested twice.
	if err := os.MkdirAll(exports, 0o755); err != nil {
		return "", err
	}
	// 10 tries is a bound, not a tunable: with ms timestamps + a 1M
	// counter, exhaustion needs 10 same-ms collisions or a clock step-back
	// landing on a live counter value 10 times running. Negligible for v1;
	// on exhaustion the save fails loudly instead of clobbering.
	var last string
	for i := 0; i < 10; i++ {
		name := p + "-" + time.Now().UTC().Format("20060102-150405.000") +
			"-" + fmt.Sprintf("%06d", evSeq.Add(1)%1000000)
		d, err := jailPath(exports, name)
		if err != nil {
			return "", err
		}
		if err := os.Mkdir(d, 0o755); err == nil {
			return d, nil
		} else if !os.IsExist(err) {
			return "", err
		}
		last = d
	}
	return "", fmt.Errorf("evidence dir collision, last tried %q", last)
}

// pruneSessions drops idle browser profiles: a context dir untouched for
// longer than the TTL is cookies for sites nobody visits, and keeping them
// is state without an owner. Runs once at server start; failures only log,
// because a prune must never stop the server from serving.
func pruneSessions(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-sessionTTL)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(base, e.Name())
		st, err := os.Stat(filepath.Join(p, "ctx"))
		if err != nil {
			st, err = os.Stat(p)
		}
		if err != nil || st.ModTime().After(cutoff) {
			continue
		}
		log.Printf("browser: pruning idle session %s", e.Name())
		if err := os.RemoveAll(p); err != nil {
			log.Printf("browser: prune failed for %s: %v", e.Name(), err)
		}
	}
}
