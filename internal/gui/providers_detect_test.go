package gui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// The editor's Detect asks which APIs answer at the URL typed, with the
// form as it stands: a new provider's key, or a saved one's when none is
// typed. Nothing is saved by it. A Save's modelPrefs carry the API picked
// for a model, and the editor is told it back.
func TestProviderDetect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	var auths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		// Gemini's API is asked with the key in x-goog-api-key
		auth := r.Header.Get("Authorization")
		if g := r.Header.Get("x-goog-api-key"); g != "" {
			auth = "goog " + g
		}
		auths = append(auths, r.Method+" "+r.URL.Path+" "+auth)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/chat/completions":
			io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
		case "/v1/messages":
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"hi"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	post := func(action, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/provider/"+action, strings.NewReader(body)))
		return w
	}
	detect := func(body string) []provider.Detection {
		t.Helper()
		w := post("detect", body)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		var got struct{ Results []provider.Detection }
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Results
	}
	oks := func(rs []provider.Detection) string {
		var s []string
		for _, r := range rs {
			if r.OK {
				s = append(s, string(r.Protocol)+"="+r.Base)
			}
		}
		return strings.Join(s, " ")
	}
	want := "chat=" + up.URL + "/v1 anthropic=" + up.URL
	if got := oks(detect(`{"typed":true,"key":"sk-new","base":"` + up.URL + `","model":"m"}`)); got != want {
		t.Fatalf("new: %s", got)
	}
	if ps := provider.All(); len(ps) != 0 {
		t.Fatalf("detect saved %v", ps)
	}
	if w := post("save", `{"id":"relay","name":"Relay","key":"sk-saved","chat":"`+up.URL+`/v1","anthropic":"`+up.URL+`","models":["m","n"],"new":true}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	mu.Lock()
	auths = nil
	mu.Unlock()
	if got := oks(detect(`{"id":"relay","typed":true,"key":"","base":"` + up.URL + `/v1","model":"m"}`)); got != want {
		t.Fatalf("saved: %s", got)
	}
	mu.Lock()
	for _, a := range auths {
		if !strings.HasSuffix(a, "Bearer sk-saved") && !strings.HasSuffix(a, "goog sk-saved") {
			t.Fatalf("asked %s, not with the saved key", a)
		}
	}
	mu.Unlock()
	if w := post("detect", `{"typed":true,"key":"k","base":""}`); w.Code == 200 || !strings.Contains(w.Body.String(), "base URL") {
		t.Fatalf("no URL: %d %s", w.Code, w.Body)
	}

	// the API picked for a model, with the Save
	if w := post("save", `{"id":"relay","from":"relay","name":"Relay","chat":"`+up.URL+`/v1","anthropic":"`+up.URL+`","models":["m","n"],"modelPrefs":{"m":{"api":"anthropic"}}}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := settings.Load().ModelAPIs["relay/m"]; got != "anthropic" {
		t.Fatalf("kept %q", got)
	}
	p, _ := provider.Find("relay")
	info := providerInfo(*p, nil)
	for _, m := range info.Models {
		if want := map[string]string{"m": "anthropic"}[m.ID]; m.API != want {
			t.Fatalf("%s told api %q", m.ID, m.API)
		}
	}
	if w := post("save", `{"id":"relay","from":"relay","name":"Relay","chat":"`+up.URL+`/v1","anthropic":"`+up.URL+`","models":["m","n"],"modelPrefs":{"n":{"api":"responses"}}}`); w.Code == 200 || !strings.Contains(w.Body.String(), "responses") {
		t.Fatalf("an API with no URL: %d %s", w.Code, w.Body)
	}
}

// A saved address the form replaced is asked nowhere: detect posted with
// the URL typed over the saved one (chat = base = B) asks B as each API
// takes it (DetectBase) with the saved key, and A, the address saved, is
// asked nothing. The provider on disk keeps A: a detect saves nothing.
// Asked with a model and, separately, model by model (detectModels).
func TestProviderDetectAsksTheURLTypedNotTheSavedOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// A is the address saved; it answers everything, so a probe that
	// reaches it shows in the log
	var amu sync.Mutex
	var aGot []string
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		amu.Lock()
		aGot = append(aGot, r.Method+" "+r.URL.Path)
		amu.Unlock()
		io.WriteString(w, `{}`)
	}))
	defer a.Close()
	// B is the URL typed over it
	var bmu sync.Mutex
	var bGot []string
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		bmu.Lock()
		// Gemini's API is asked with the key in x-goog-api-key
		auth := r.Header.Get("Authorization")
		if g := r.Header.Get("x-goog-api-key"); g != "" {
			auth = "goog " + g
		}
		bGot = append(bGot, r.Method+" "+r.URL.Path+" "+auth)
		bmu.Unlock()
		io.WriteString(w, `{}`)
	}))
	defer b.Close()
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	post := func(action, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/provider/"+action, strings.NewReader(body)))
		return w
	}
	// the saved Chat is A's root, bare: its own spelling is asked at
	// /chat/completions, so a probe at B must be spelled B/v1 to show the
	// rewrite, and one at A cannot be mistaken for it
	if w := post("save", `{"id":"relay","name":"Relay","key":"sk-saved","chat":"`+a.URL+`","models":["m"],"new":true}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, body := range []string{
		`{"id":"relay","typed":true,"key":"","chat":"` + b.URL + `","base":"` + b.URL + `","model":"m"}`,
		`{"id":"relay","typed":true,"key":"","chat":"` + b.URL + `","base":"` + b.URL + `","detectModels":["m"]}`,
	} {
		amu.Lock()
		aGot = nil
		amu.Unlock()
		bmu.Lock()
		bGot = nil
		bmu.Unlock()
		w := post("detect", body)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		var got struct {
			Results []provider.Detection
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Results) != 4 || got.Results[0].Protocol != provider.Chat || !got.Results[0].OK || got.Results[0].Base != b.URL+"/v1" {
			t.Fatalf("chat answered %+v", got.Results[0])
		}
		amu.Lock()
		if len(aGot) != 0 {
			t.Fatalf("the saved address was asked %v", aGot)
		}
		amu.Unlock()
		bmu.Lock()
		var chats []string
		for _, x := range bGot {
			if strings.HasSuffix(x, "/chat/completions Bearer sk-saved") {
				chats = append(chats, x)
			}
		}
		bmu.Unlock()
		if len(chats) != 1 || chats[0] != "POST /v1/chat/completions Bearer sk-saved" {
			t.Fatalf("the URL typed was asked %v, not once at its /v1 with the saved key", chats)
		}
	}
	p, err := provider.Find("relay")
	if err != nil {
		t.Fatal(err)
	}
	if p.Chat != a.URL {
		t.Fatalf("the saved Chat moved to %s", p.Chat)
	}
}

// A base DetectBase cannot spell (a path alone) is refused by the URL
// check, not turned into an empty form field typed() would take for
// "unchanged" and answer from the saved address.
func TestProviderDetectRefusesABaseItCannotSpell(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	var got []string
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		io.WriteString(w, `{}`)
	}))
	defer a.Close()
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	post := func(action, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/provider/"+action, strings.NewReader(body)))
		return w
	}
	if w := post("save", `{"id":"relay","name":"Relay","key":"sk-saved","chat":"`+a.URL+`","models":["m"],"new":true}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, body := range []string{
		`{"id":"relay","typed":true,"key":"","chat":"/chat/completions","base":"/chat/completions","model":"m"}`,
		`{"id":"relay","typed":true,"key":"","chat":"/chat/completions","base":"/chat/completions","detectModels":["m"]}`,
	} {
		mu.Lock()
		got = nil
		mu.Unlock()
		w := post("detect", body)
		if w.Code != 400 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		mu.Lock()
		asked := len(got)
		mu.Unlock()
		if asked != 0 {
			t.Fatalf("the saved address was asked %v", got)
		}
	}
	if p, err := provider.Find("relay"); err != nil || p.Chat != a.URL {
		t.Fatalf("the saved Chat is %q (%v)", p.Chat, err)
	}
}
