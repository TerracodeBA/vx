package web_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// findChrome returns a headless Chrome or Chromium: VX_CHROME, one on PATH,
// or one Playwright downloaded. "" when there is none.
func findChrome() string {
	if c := os.Getenv("VX_CHROME"); c != "" {
		return c
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome", "headless_shell", "chrome-headless-shell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	home, _ := os.UserHomeDir()
	for _, pat := range []string{
		"ms-playwright/chromium_headless_shell-*/chrome-*/chrome-headless-shell",
		"ms-playwright/chromium_headless_shell-*/chrome-*/headless_shell",
		"ms-playwright/chromium-*/chrome-*/chrome",
	} {
		if m, _ := filepath.Glob(filepath.Join(home, ".cache", pat)); len(m) > 0 {
			return m[len(m)-1]
		}
	}
	return ""
}

// cdp is a minimal Chrome DevTools Protocol client for one page.
type cdp struct {
	ws     *websocket.Conn
	mu     sync.Mutex
	id     int
	wait   map[int]chan json.RawMessage
	events chan string
}

func startChrome(t *testing.T) *cdp {
	t.Helper()
	bin := findChrome()
	if bin == "" {
		t.Skip("no headless Chrome or Chromium (set VX_CHROME)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "--headless", "--no-sandbox", "--disable-gpu", "--hide-scrollbars", "--remote-debugging-port=0",
		"--remote-allow-origins=*", "--user-data-dir="+t.TempDir(), "about:blank")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Skipf("cannot start %s: %v", bin, err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		re := regexp.MustCompile(`DevTools listening on (ws://\S+)`)
		for sc.Scan() {
			if m := re.FindStringSubmatch(sc.Text()); m != nil {
				found <- m[1]
			}
		}
	}()
	var browserWS string
	select {
	case browserWS = <-found:
	case <-time.After(20 * time.Second):
		t.Skip("headless Chrome did not start in time")
	}
	u, _ := url.Parse(browserWS)
	// The first page target.
	var targets []struct{ Type, WebSocketDebuggerURL string }
	for range 50 {
		resp, err := httpGetJSON("http://"+u.Host+"/json/list", &targets)
		if err == nil && resp {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	page := ""
	for _, tg := range targets {
		if tg.Type == "page" {
			page = tg.WebSocketDebuggerURL
		}
	}
	if page == "" {
		t.Fatal("headless Chrome has no page")
	}
	ws, err := websocket.Dial(page, "", "http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	ws.MaxPayloadBytes = 64 << 20
	c := &cdp{ws: ws, wait: map[int]chan json.RawMessage{}, events: make(chan string, 64)}
	t.Cleanup(func() { ws.Close() })
	go c.read()
	return c
}

func httpGetJSON(u string, v any) (bool, error) {
	resp, err := http.Get(u)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v) == nil, nil
}

func (c *cdp) read() {
	for {
		var msg struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := websocket.JSON.Receive(c.ws, &msg); err != nil {
			return
		}
		if msg.Method != "" {
			select {
			case c.events <- msg.Method:
			default:
			}
			continue
		}
		c.mu.Lock()
		ch := c.wait[msg.ID]
		delete(c.wait, msg.ID)
		c.mu.Unlock()
		if ch != nil {
			if msg.Error != nil {
				b, _ := json.Marshal(map[string]string{"__error": msg.Error.Message})
				ch <- b
			} else {
				ch <- msg.Result
			}
		}
	}
}

func (c *cdp) call(method string, params any, out any) error {
	c.mu.Lock()
	c.id++
	id := c.id
	ch := make(chan json.RawMessage, 1)
	c.wait[id] = ch
	c.mu.Unlock()
	if params == nil {
		params = map[string]any{}
	}
	if err := websocket.JSON.Send(c.ws, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case res := <-ch:
		var e struct {
			Err string `json:"__error"`
		}
		if json.Unmarshal(res, &e) == nil && e.Err != "" {
			return fmt.Errorf("%s: %s", method, e.Err)
		}
		if out != nil {
			return json.Unmarshal(res, out)
		}
		return nil
	case <-time.After(30 * time.Second):
		return errors.New(method + ": no answer")
	}
}

func (c *cdp) navigate(u string) error {
	for len(c.events) > 0 {
		<-c.events
	}
	if err := c.call("Page.navigate", map[string]any{"url": u}, nil); err != nil {
		return err
	}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev := <-c.events:
			if ev == "Page.loadEventFired" {
				return nil
			}
		case <-deadline:
			return errors.New("page did not load: " + u)
		}
	}
}

func (c *cdp) eval(expr string, out any) error {
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := c.call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return errors.New(res.ExceptionDetails.Text)
	}
	return json.Unmarshal(res.Result.Value, out)
}

func (c *cdp) screenshot(path string, width int) error {
	var h float64
	if err := c.eval(`Math.min(document.documentElement.scrollHeight, 6000)`, &h); err != nil {
		return err
	}
	var shot struct{ Data string }
	if err := c.call("Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": true,
		"clip": map[string]any{"x": 0, "y": 0, "width": width, "height": h, "scale": 1}}, &shot); err != nil {
		return err
	}
	b, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// phoneCheck runs in the page: page-level horizontal scroll, elements that
// stick out of the screen outside a scrolling box, tap targets under 40px
// and text under 12px.
const phoneCheck = `(() => {
  const de = document.documentElement, vw = de.clientWidth;
  const out = {scroll: de.scrollWidth - vw, wide: [], small: [], tiny: []};
  const name = el => el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") + (el.className && typeof el.className === "string" ? "." + el.className.trim().split(/\s+/).join(".") : "") + (el.textContent ? " [" + el.textContent.trim().replace(/\s+/g, " ").slice(0, 30) + "]" : "");
  const hidden = el => { for (let p = el; p && p !== document.body; p = p.parentElement) { const cs = getComputedStyle(p); if (cs.display === "none" || cs.visibility === "hidden") return true; if (p.tagName === "DETAILS" && !p.open && el !== p && !el.closest("summary")) return true; } return false; };
  const scroller = el => { for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) { const o = getComputedStyle(p).overflowX; if (o === "auto" || o === "scroll" || o === "hidden" || o === "clip") return true; } return false; };
  const drawer = el => el.closest(".sidebar") && !document.body.classList.contains("menu-open");
  for (const el of document.querySelectorAll("body *")) {
    if (drawer(el)) continue;
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0 || hidden(el)) continue;
    if (r.right > vw + 1 && !scroller(el)) out.wide.push(name(el) + " right=" + Math.round(r.right));
  }
  for (const el of document.querySelectorAll("button, .btn, a.side-link, .menu-btn, input:not([type=hidden]):not([type=checkbox]):not([type=radio]), select, textarea, summary")) {
    if (drawer(el) || hidden(el)) continue;
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) continue;
    if (r.height < 39.5) out.small.push(name(el) + " h=" + r.height.toFixed(1));
  }
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  while (walker.nextNode()) {
    const n = walker.currentNode, el = n.parentElement;
    if (!n.textContent.trim() || !el || drawer(el) || hidden(el)) continue;
    const fs = parseFloat(getComputedStyle(el).fontSize);
    if (fs < 11.5) out.tiny.push(name(el) + " " + fs + "px");
  }
  out.wide = [...new Set(out.wide)].slice(0, 15); out.small = [...new Set(out.small)].slice(0, 15); out.tiny = [...new Set(out.tiny)].slice(0, 15);
  return out;
})()`

// Every page at phone widths in a real browser: no page-level horizontal
// scroll (diffs and tables scroll inside their box), nothing sticks out,
// tap targets at least 40px and text at least 12px. Screenshots at 390px
// go to VX_SCREENSHOTS when set. Skips without a headless Chrome.
func TestPhoneScreenshots(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	c := startChrome(t)
	e := newPhoneEnv(t)
	dir := os.Getenv("VX_SCREENSHOTS")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(e.srv.URL)
	if err := c.call("Page.enable", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.call("Network.enable", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.call("Network.setCookie", map[string]any{"name": "vx_token_" + u.Port(), "value": e.token, "url": e.srv.URL, "httpOnly": true}, nil); err != nil {
		t.Fatal(err)
	}
	pages := append([]string{"/ui/offline"}, e.pages...)
	for _, width := range []int{360, 390, 430} {
		if err := c.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": 844, "deviceScaleFactor": 1, "mobile": true}, nil); err != nil {
			t.Fatal(err)
		}
		for _, p := range pages {
			if err := c.navigate(e.srv.URL + p); err != nil {
				t.Fatal(err)
			}
			// Open every file's diff, as a person reading the change does.
			var opened int
			if err := c.eval(`(async () => { const ds = document.querySelectorAll("details.fd"); ds.forEach(d => d.open = true); await new Promise(r => setTimeout(r, 400)); return ds.length; })()`, &opened); err != nil {
				t.Fatal(err)
			}
			var res struct {
				Scroll            float64
				Wide, Small, Tiny []string
			}
			if err := c.eval(phoneCheck, &res); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			if res.Scroll > 0 {
				t.Errorf("%dpx %s: the page scrolls sideways by %.0fpx", width, p, res.Scroll)
			}
			if len(res.Wide) > 0 {
				t.Errorf("%dpx %s: sticks out of the screen:\n  %s", width, p, strings.Join(res.Wide, "\n  "))
			}
			if len(res.Small) > 0 {
				t.Errorf("%dpx %s: tap targets under 40px:\n  %s", width, p, strings.Join(res.Small, "\n  "))
			}
			if len(res.Tiny) > 0 {
				t.Errorf("%dpx %s: text under 12px:\n  %s", width, p, strings.Join(res.Tiny, "\n  "))
			}
			if width == 390 {
				// Stable names: ids and hashes left out.
				name := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f-]{27}|b3(:|%3a)[0-9a-f]+|slug=[a-z0-9-]+`).ReplaceAllString(strings.ToLower(p), "")
				name = strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(name, "-"), "-")
				if err := c.screenshot(filepath.Join(dir, name+".png"), width); err != nil {
					t.Errorf("screenshot %s: %v", p, err)
				}
			}
		}
	}
	// The menu opens as a drawer and lists For you first.
	if err := c.navigate(e.srv.URL + "/ui"); err != nil {
		t.Fatal(err)
	}
	var first string
	if err := c.eval(`(async () => { document.querySelector(".menu-btn").click(); await new Promise(r => setTimeout(r, 300)); const a = document.querySelector(".side-nav a"); const r = a.getBoundingClientRect(); return r.left >= 0 && r.right <= innerWidth ? a.textContent.trim() : "offscreen"; })()`, &first); err != nil {
		t.Fatal(err)
	}
	if first != "For you" {
		t.Errorf("first menu entry = %q", first)
	}
	if dir == os.Getenv("VX_SCREENSHOTS") {
		_ = c.screenshot(filepath.Join(dir, "menu-open.png"), 390)
	}
}
