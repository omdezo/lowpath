package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lowpath/internal/demo"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// newDemoServer runs the API against the simulated network, like -demo.
func newDemoServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store := openTestStore(t)
	games, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	sim, err := demo.Start(ctx, demo.DefaultPaths(), demo.DirectProfile(), discard)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Config{Store: store, Games: games, Network: DemoNetwork{N: sim}, Sim: sim, Platform: newFake(t).Platform(), Log: discard, Session: testSession})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.Close(); ts.Close() })
	return ts, store
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// testSession is the session secret test servers use; do sends it unless
// hdr sets "Cookie" ("" for no cookie at all).
const testSession = "test-session"

func do(t *testing.T, ts *httptest.Server, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", sessionCookie+"="+testSession)
	for k, v := range hdr {
		switch {
		case k == "Host":
			req.Host = v
		case k == "Cookie" && v == "":
			req.Header.Del("Cookie")
		default:
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// call is do with the CSRF header, decoding a JSON reply into out (if set).
func call(t *testing.T, ts *httptest.Server, method, path, body string, out any) int {
	t.Helper()
	code, b := do(t, ts, method, path, body, csrf)
	if out != nil && code < 300 {
		if err := json.Unmarshal([]byte(b), out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, b)
		}
	}
	return code
}

var csrf = map[string]string{"X-Lowpath": "1"}

func TestGuardBlocksForeignHostAndCSRF(t *testing.T) {
	ts, _ := newDemoServer(t)
	if code, _ := do(t, ts, "GET", "/api/bootstrap", "", map[string]string{"Host": "attacker.example"}); code != 403 {
		t.Errorf("foreign Host: got %d, want 403", code)
	}
	if code, _ := do(t, ts, "POST", "/api/relays", `{"label":"x","addr":"203.0.113.1:40000"}`, nil); code != 403 {
		t.Errorf("POST without X-Lowpath: got %d, want 403", code)
	}
	if code, _ := do(t, ts, "DELETE", "/api/relays/x", "", nil); code != 403 {
		t.Errorf("DELETE without X-Lowpath: got %d, want 403", code)
	}
	if code, _ := do(t, ts, "GET", "/api/bootstrap", "", nil); code != 200 {
		t.Errorf("plain local GET: got %d, want 200", code)
	}
}

// Any local program can reach the port; only the app's window holds the
// session cookie, which it gets once through LaunchPath.
func TestSessionRequired(t *testing.T) {
	ts, srv := newRealServer(t, newFake(t))
	noCookie := map[string]string{"Cookie": ""}
	if code, body := do(t, ts, "GET", "/api/bootstrap", "", noCookie); code != 401 || !strings.Contains(body, `"unauthorized"`) {
		t.Errorf("GET without the session: %d %s, want 401 unauthorized", code, body)
	}
	if code, _ := do(t, ts, "GET", "/api/diagnostics", "", noCookie); code != 401 {
		t.Errorf("diagnostics without the session: %d, want 401", code)
	}
	wrong := map[string]string{"X-Lowpath": "1", "Cookie": sessionCookie + "=guess"}
	if code, _ := do(t, ts, "POST", "/api/relays", `{"label":"x","addr":"203.0.113.1:40000"}`, wrong); code != 401 {
		t.Errorf("POST with a wrong session: %d, want 401", code)
	}
	if code, body := do(t, ts, "GET", "/api/version", "", noCookie); code != 200 || !strings.Contains(body, Version) {
		t.Errorf("version check: %d %s, want it open so a second copy finds this one", code, body)
	}

	// The window's first page trades the one-time token for the cookie.
	launch := srv.LaunchPath()
	if !strings.HasPrefix(launch, "/api/session?t=") {
		t.Fatalf("launch path %q", launch)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noFollow.Get(ts.URL + launch)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	var got *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			got = c
		}
	}
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" || got == nil {
		t.Fatalf("launch: %d to %q, cookie %v", res.StatusCode, res.Header.Get("Location"), got)
	}
	if got.Value != testSession || !got.HttpOnly || got.SameSite != http.SameSiteStrictMode || !got.Expires.IsZero() || got.MaxAge != 0 {
		t.Errorf("session cookie %+v, want the session as an HttpOnly, SameSite=Strict, in-memory cookie", got)
	}
	// The token is spent: another program that sees it later gets nothing.
	if code, _ := do(t, ts, "GET", launch, "", noCookie); code != 403 {
		t.Errorf("launch token reused: %d, want 403", code)
	}
	if p := srv.LaunchPath(); p != "/" {
		t.Errorf("launch path after use %q, want /", p)
	}

	// A second start of the app gets a fresh one-time link for the window
	// it opens; asking again before it is used gives the same link.
	again := srv.NewLaunchPath()
	if again == launch || !strings.HasPrefix(again, "/api/session?t=") || srv.NewLaunchPath() != again {
		t.Fatalf("new launch path %q (first %q)", again, launch)
	}
	if res, err := noFollow.Get(ts.URL + again); err != nil {
		t.Error(err)
	} else if res.Body.Close(); res.StatusCode != http.StatusSeeOther {
		t.Errorf("fresh launch link: %s, want it to sign in", res.Status)
	}
	if code, _ := do(t, ts, "GET", again, "", noCookie); code != 403 {
		t.Errorf("fresh launch link reused: %d, want 403", code)
	}
}

// Lowpath has no accounts, trials or plans: Boost works from the first run.
func TestApplyNeedsNoAccount(t *testing.T) {
	ts, _ := newDemoServer(t)
	for _, path := range []string{"/api/auth/signup", "/api/auth/login", "/api/auth/logout", "/api/account/plan", "/api/account/expire-trial"} {
		if code, _ := do(t, ts, "POST", path, "{}", csrf); code != 404 {
			t.Errorf("POST %s: got %d, want 404", path, code)
		}
	}
	for _, path := range []string{"/api/bootstrap", "/api/diagnostics"} {
		if code, body := do(t, ts, "GET", path, "", nil); code != 200 || strings.Contains(body, `"account"`) {
			t.Errorf("GET %s: %d, mentions an account: %v", path, code, strings.Contains(body, `"account"`))
		}
	}

	// Demo mode always runs proxy mode, whatever the setting says.
	if code, body := do(t, ts, "POST", "/api/apply", `{"game_id":"cs2","mode":"smart"}`, csrf); code != 200 || !strings.Contains(body, `"status":"optimizing"`) || !strings.Contains(body, `"capture_mode":"proxy"`) {
		t.Fatalf("apply on first run: %d %s", code, body)
	}
	if code, _ := do(t, ts, "POST", "/api/apply", `{"game_id":"cs2","mode":"specific","route_id":"eu-central"}`, csrf); code != 200 {
		t.Errorf("apply a specific route: got %d", code)
	}
	if code, _ := do(t, ts, "POST", "/api/apply", `{"game_id":"cs2","mode":"specific","route_id":"nope"}`, csrf); code != 400 {
		t.Errorf("unknown route: got %d, want 400", code)
	}
}

// State files from builds that had local accounts still load; the old
// fields are dropped the next time the file is written.
func TestStoreIgnoresOldAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"accounts":{"a@b.test":{"email":"a@b.test","salt":"00","hash":"00","plan":"trial"}},"current":"a@b.test","favorites":["cs2"],"settings":{"theme":"light","capture_mode":"proxy"}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open old state: %v", err)
	}
	if fav := store.Favorites(); len(fav) != 1 || fav[0] != "cs2" {
		t.Errorf("favorites %v, want [cs2]", fav)
	}
	if st := store.Settings(); st.Theme != "light" || st.CaptureMode != CaptureProxy {
		t.Errorf("settings %+v", st)
	}
	if _, err := store.ToggleFavorite("valorant"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "accounts") || strings.Contains(string(b), "a@b.test") {
		t.Errorf("rewritten state still holds the old account: %s", b)
	}
}
