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
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
	// short enough for a prompt; the response says when it clipped.
	MaxReadRunes = 12000
	// sessionTTL prunes idle browser profiles: cookies and state older than
	// this with no fresh use go away on server start.
	sessionTTL = 30 * 24 * time.Hour
)

var (
	store   *sessions
	profile string
	exports string
)

func fail(err error) (*mcp.CallToolResult, map[string]any, error) {
	return nil, map[string]any{"ok": false, "error": err.Error()}, nil
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
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("only http and https urls are allowed, not %q", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("urls with credentials are not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("url has no host")
	}
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("cannot resolve %q", host)
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, fmt.Errorf("non-public address is not allowed (%s resolves there)", host)
		}
	}
	return u, nil
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

func inDenyList(ip net.IP) bool {
	// Normalize first: an IPv4-mapped IPv6 address must match the v4
	// ranges, not slip past them in 16-byte form.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, cidr := range denyCIDRs {
		if _, net, err := net.ParseCIDR(cidr); err == nil && net.Contains(ip) {
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

func (ss *sessions) get(profile string) *session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s, ok := ss.byProfile[profile]
	if !ok {
		s = &session{dir: filepath.Join(ss.agentDir, ".runtime", "browser", profile)}
		ss.byProfile[profile] = s
	}
	// Startup prune covers fresh boots; this covers months-old processes
	// (the server lives as long as Pi keeps it spawned). One walk a day.
	if time.Since(ss.lastPrune) > 24*time.Hour {
		ss.lastPrune = time.Now()
		go pruneSessions(filepath.Join(ss.agentDir, ".runtime", "browser"))
	}
	return s
}

// ensure launches Chromium on first use, not at server start: the entrypoint
// `pi mcp list` gate must stay fast, and a profile that never browses must
// never pay for a browser.
func (s *session) ensure(chrome string) (context.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return s.browser, nil
	}
	ctxDir := filepath.Join(s.dir, "ctx")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		return nil, fmt.Errorf("session dir: %v", err)
	}
	// Every byte through the filtering proxy: re-validated per connection,
	// so redirects and DNS rebindings face the same gate as the first URL.
	// Ephemeral loopback port, closed never (process-lifetime server), and
	// <-loopback bypass removal so even localhost goes through the check.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("proxy listen: %v", err)
	}
	go http.Serve(ln, &filteringProxy{check: publicIP})
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
		return nil, fmt.Errorf("chromium launch failed: %v", err)
	}
	s.alloc, s.cancel, s.browserCancel, s.browser, s.started = alloc, cancel, browserCancel, browser, true
	s.proxyLn = ln
	// Resume the shot counter past restarts so evidence never overwrites.
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "shot-%d.png", &n); err == nil && n > s.shots {
			s.shots = n
		}
	}
	return s.browser, nil
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
			bctx, err := store.get(profile).ensure(store.chrome)
			if err != nil {
				return fail(err)
			}
			navCtx, cancel := context.WithTimeout(bctx, navTimeout)
			defer cancel()
			if err := chromedp.Do(navCtx, chromedp.Navigate(u.String())); err != nil {
				return fail(fmt.Errorf("navigation failed: %v", err))
			}
			title, err := chromedp.Run(navCtx, chromedp.Title())
			if err != nil {
				return fail(fmt.Errorf("title read failed: %v", err))
			}
			return result(map[string]any{"ok": true, "url": u.String(), "title": title})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "browser_read",
		Description: "Read the current page as text: URL, title and body content up to the cap. Use it after goto, or to re-read after the page changed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, map[string]any, error) {
			bctx, err := store.get(profile).ensure(store.chrome)
			if err != nil {
				return fail(err)
			}
			readCtx, cancel := context.WithTimeout(bctx, readTimeout)
			defer cancel()
			url, err := chromedp.Run(readCtx, chromedp.Location())
			if err == nil && (url == "" || url == "about:blank") {
				return result(map[string]any{"ok": false, "error": "no page yet: browser_goto first"})
			}
			if err != nil {
				return fail(fmt.Errorf("read failed: %v", err))
			}
			title, err := chromedp.Run(readCtx, chromedp.Title())
			if err != nil {
				return fail(fmt.Errorf("read failed: %v", err))
			}
			text, err := chromedp.Run(readCtx,
				chromedp.Evaluate[string](`document.body ? document.body.innerText : ""`))
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
				"ok": true, "url": url, "title": title,
				"text": text, "truncated": truncated,
			})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "browser_screenshot",
		Description: "Capture the current page as PNG into the session dir. Working file, not evidence: use browser_save_evidence for the exports bundle the Storage tab shows."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in shotInput) (*mcp.CallToolResult, map[string]any, error) {
			sess := store.get(profile)
			bctx, err := sess.ensure(store.chrome)
			if err != nil {
				return fail(err)
			}
			shotCtx, cancel := context.WithTimeout(bctx, readTimeout)
			defer cancel()
			var buf []byte
			if in.FullPage {
				buf, err = chromedp.Run(shotCtx, chromedp.FullScreenshot(90))
			} else {
				buf, err = chromedp.Run(shotCtx, chromedp.CaptureScreenshot())
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
			sess := store.get(profile)
			bctx, err := sess.ensure(store.chrome)
			if err != nil {
				return fail(err)
			}
			evCtx, cancel := context.WithTimeout(bctx, readTimeout)
			defer cancel()
			url, err := chromedp.Run(evCtx, chromedp.Location())
			if err != nil {
				return fail(fmt.Errorf("evidence capture failed: %v", err))
			}
			title, err := chromedp.Run(evCtx, chromedp.Title())
			if err != nil {
				return fail(fmt.Errorf("evidence capture failed: %v", err))
			}
			text, err := chromedp.Run(evCtx,
				chromedp.Evaluate[string](`document.body ? document.body.innerText : ""`))
			if err != nil {
				return fail(fmt.Errorf("evidence capture failed: %v", err))
			}
			buf, err := chromedp.Run(evCtx, chromedp.FullScreenshot(90))
			if err != nil {
				return fail(fmt.Errorf("evidence capture failed: %v", err))
			}
			dir, err := evidenceDir()
			if err != nil {
				return fail(err)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fail(err)
			}
			text = collapseLines(text)
			if utf8.RuneCountInString(text) > MaxReadRunes {
				text = string([]rune(text)[:MaxReadRunes])
			}
			note := strings.TrimSpace(in.Note)
			md := "# " + orDefault(title, url) + "\n\n"
			if note != "" {
				md += note + "\n\n"
			}
			md += "Source: " + url + "\n\n" + text + "\n"
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
				"profile": profile, "url": url, "title": title,
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

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func collapseLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

// evidenceDir makes a fresh timestamped evidence directory, jailed under the
// exports root. The clock value is generated, never client-supplied, and the
// profile passed the safe-name gate at startup.
func evidenceDir() (string, error) {
	// Millisecond precision: two saves in the same second must not share
	// a dir and silently overwrite each other's shot.png.
	name := profile + "-" + time.Now().UTC().Format("20060102-150405.000")
	// exports already ends in /browser (default /exports/browser): the
	// timestamped dir hangs directly under it, never nested twice.
	return jailPath(exports, name)
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
