package httpcache

import (
	"bytes"
	"flag"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const defaultMaxEntries = 32

var s struct {
	server    *httptest.Server
	client    http.Client
	transport *Transport
	done      chan struct{} // Closed to unlock infinite handlers.
}

type fakeClock struct {
	elapsed time.Duration
}

func (c *fakeClock) since(t time.Time) time.Duration {
	return c.elapsed
}

func TestMain(m *testing.M) {
	flag.Parse()
	setup()
	code := m.Run()
	teardown()
	os.Exit(code)
}

func setup() {
	tp := NewMemoryCacheTransport(defaultMaxEntries)
	client := http.Client{Transport: tp}
	s.transport = tp
	s.client = client
	s.done = make(chan struct{})

	mux := http.NewServeMux()
	s.server = httptest.NewServer(mux)

	mux.HandleFunc("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
	}))

	mux.HandleFunc("/method", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Write([]byte(r.Method))
	}))

	mux.HandleFunc("/range", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lm := "Fri, 14 Dec 2010 01:01:50 GMT"
		if r.Header.Get("if-modified-since") == lm {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("last-modified", lm)
		if r.Header.Get("range") == "bytes=4-9" {
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(" text "))
			return
		}
		w.Write([]byte("Some text content"))
	}))

	mux.HandleFunc("/nostore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
	}))

	mux.HandleFunc("/etag", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := "124567"
		if r.Header.Get("if-none-match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("etag", etag)
	}))

	mux.HandleFunc("/lastmodified", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lm := "Fri, 14 Dec 2010 01:01:50 GMT"
		if r.Header.Get("if-modified-since") == lm {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("last-modified", lm)
	}))

	mux.HandleFunc("/varyunused", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Vary", "X-Madeup-Header")
		w.Write([]byte("Some text content"))
	}))

	mux.HandleFunc("/cachederror", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := "abc"
		if r.Header.Get("if-none-match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("etag", etag)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Not found"))
	}))

	updateFieldsCounter := 0
	mux.HandleFunc("/updatefields", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Counter", strconv.Itoa(updateFieldsCounter))
		w.Header().Set("Etag", `"e"`)
		updateFieldsCounter++
		if r.Header.Get("if-none-match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte("Some text content"))
	}))

	// Take 3 seconds to return 200 OK (for testing client timeouts).
	mux.HandleFunc("/3seconds", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))

	mux.HandleFunc("/infinite", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			select {
			case <-s.done:
				return
			default:
				w.Write([]byte{0})
			}
		}
	}))

	// [/status/:code] - Respond with a particular status code
	mux.HandleFunc("/status/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statusStr := strings.Trim(strings.TrimPrefix(r.URL.Path, "/status/"), "/")
		statusInt, err := strconv.Atoi(statusStr)
		if err != nil {
			panic(err)
		} else if statusInt == 301 || statusInt == 302 {
			w.Header().Set("Location", "http://example.com")
		}
		w.Header().Set("Cache-Control", "max-age=3600")
		w.WriteHeader(statusInt)
		w.Write([]byte("Some text content"))
	}))
}

func teardown() {
	close(s.done)
	s.server.Close()
}

func resetTest() {
	s.transport.Cache = NewMemoryCache(defaultMaxEntries)
	clock = &realClock{}
}

// TestCacheableMethod ensures that uncacheable method does not get stored
// in cache and get incorrectly used for a following cacheable method request.
func TestCacheableMethod(t *testing.T) {
	resetTest()
	{
		req, err := http.NewRequest("POST", s.server.URL+"/method", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, err = io.Copy(&buf, resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		err = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := buf.String(), "POST"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("response status code isn't 200 OK: %v", resp.StatusCode)
		}
	}
	{
		req, err := http.NewRequest("GET", s.server.URL+"/method", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, err = io.Copy(&buf, resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		err = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := buf.String(), "GET"; got != want {
			t.Errorf("got wrong body %q, want %q", got, want)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("response status code isn't 200 OK: %v", resp.StatusCode)
		}
		if resp.Header.Get(XFromCache) != "" {
			t.Errorf("XFromCache header isn't blank")
		}
	}
}

func TestDontServeHeadResponseToGetRequest(t *testing.T) {
	resetTest()
	url := s.server.URL + "/"
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	req, err = http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get(XFromCache) != "" {
		t.Errorf("Cache should not match")
	}
}

func TestDontStorePartialRangeInCache(t *testing.T) {
	resetTest()
	{
		req, err := http.NewRequest("GET", s.server.URL+"/range", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("range", "bytes=4-9")
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, err = io.Copy(&buf, resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		err = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := buf.String(), " text "; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if resp.StatusCode != http.StatusPartialContent {
			t.Errorf("response status code isn't 206 Partial Content: %v", resp.StatusCode)
		}
	}
	{
		req, err := http.NewRequest("GET", s.server.URL+"/range", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, err = io.Copy(&buf, resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		err = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := buf.String(), "Some text content"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("response status code isn't 200 OK: %v", resp.StatusCode)
		}
		if resp.Header.Get(XFromCache) != "" {
			t.Error("XFromCache header isn't blank")
		}
	}
	{
		req, err := http.NewRequest("GET", s.server.URL+"/range", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, err = io.Copy(&buf, resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		err = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := buf.String(), "Some text content"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("response status code isn't 200 OK: %v", resp.StatusCode)
		}
		if resp.Header.Get(XFromCache) != "1" {
			t.Errorf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
	}
	{
		req, err := http.NewRequest("GET", s.server.URL+"/range", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("range", "bytes=4-9")
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, err = io.Copy(&buf, resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		err = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := buf.String(), " text "; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if resp.StatusCode != http.StatusPartialContent {
			t.Errorf("response status code isn't 206 Partial Content: %v", resp.StatusCode)
		}
	}
}

func TestCacheOnlyIfBodyRead(t *testing.T) {
	resetTest()
	{
		req, err := http.NewRequest("GET", s.server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
		// We do not read the body
		resp.Body.Close()
	}
	{
		req, err := http.NewRequest("GET", s.server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatalf("XFromCache header isn't blank")
		}
	}
}

func TestOnlyReadBodyOnDemand(t *testing.T) {
	resetTest()

	req, err := http.NewRequest("GET", s.server.URL+"/infinite", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.client.Do(req) // This shouldn't hang forever.
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10) // Only partially read the body.
	_, err = resp.Body.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestGetOnlyIfCachedHit(t *testing.T) {
	resetTest()
	{
		req, err := http.NewRequest("GET", s.server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
		_, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		req, err := http.NewRequest("GET", s.server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Add("cache-control", "only-if-cached")
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "1" {
			t.Fatalf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("response status code isn't 200 OK: %v", resp.StatusCode)
		}
	}
}

func TestGetOnlyIfCachedMiss(t *testing.T) {
	resetTest()
	req, err := http.NewRequest("GET", s.server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Add("cache-control", "only-if-cached")
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get(XFromCache) != "" {
		t.Fatal("XFromCache header isn't blank")
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("response status code isn't 504 GatewayTimeout: %v", resp.StatusCode)
	}
}

func TestGetNoStoreRequest(t *testing.T) {
	resetTest()
	req, err := http.NewRequest("GET", s.server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Add("Cache-Control", "no-store")
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
	}
}

func TestGetNoStoreResponse(t *testing.T) {
	resetTest()
	req, err := http.NewRequest("GET", s.server.URL+"/nostore", nil)
	if err != nil {
		t.Fatal(err)
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
	}
}

func TestGetWithEtag(t *testing.T) {
	resetTest()
	req, err := http.NewRequest("GET", s.server.URL+"/etag", nil)
	if err != nil {
		t.Fatal(err)
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
		_, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "1" {
			t.Fatalf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
		// additional assertions to verify that 304 response is converted properly
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("response status code isn't 200 OK: %v", resp.StatusCode)
		}
		if _, ok := resp.Header["Connection"]; ok {
			t.Fatalf("Connection header isn't absent")
		}
	}
}

func TestGetWithLastModified(t *testing.T) {
	resetTest()
	req, err := http.NewRequest("GET", s.server.URL+"/lastmodified", nil)
	if err != nil {
		t.Fatal(err)
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("XFromCache header isn't blank")
		}
		_, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "1" {
			t.Fatalf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
	}
}

func TestGetVaryUnused(t *testing.T) {
	resetTest()
	req, err := http.NewRequest("GET", s.server.URL+"/varyunused", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/plain")
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Vary") == "" {
			t.Fatalf(`Vary header is blank`)
		}
		_, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "1" {
			t.Fatalf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
	}
}

func TestUpdateFields(t *testing.T) {
	resetTest()
	req, err := http.NewRequest("GET", s.server.URL+"/updatefields", nil)
	if err != nil {
		t.Fatal(err)
	}
	var counter, counter2 string
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		counter = resp.Header.Get("x-counter")
		_, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(XFromCache) != "1" {
			t.Fatalf(`XFromCache header isn't "1": %v`, resp.Header.Get(XFromCache))
		}
		counter2 = resp.Header.Get("x-counter")
	}
	if counter == counter2 {
		t.Fatalf(`both "x-counter" values are equal: %v %v`, counter, counter2)
	}
}

// This tests the fix for https://github.com/gregjones/httpcache/issues/74.
// Previously, after validating a cached response, its StatusCode
// was incorrectly being replaced.
func TestCachedErrorsKeepStatus(t *testing.T) {
	resetTest()
	req, err := http.NewRequest("GET", s.server.URL+"/cachederror", nil)
	if err != nil {
		t.Fatal(err)
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(ioutil.Discard, resp.Body)
	}
	{
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("Status code isn't 404: %d", resp.StatusCode)
		}
	}
}

func TestParseCacheControl(t *testing.T) {
	resetTest()
	h := http.Header{}
	for range parseCacheControl(h) {
		t.Fatal("cacheControl should be empty")
	}

	h.Set("cache-control", "no-cache")
	{
		cc := parseCacheControl(h)
		if _, ok := cc["foo"]; ok {
			t.Error(`Value "foo" shouldn't exist`)
		}
		noCache, ok := cc["no-cache"]
		if !ok {
			t.Fatalf(`"no-cache" value isn't set`)
		}
		if noCache != "" {
			t.Fatalf(`"no-cache" value isn't blank: %v`, noCache)
		}
	}
	h.Set("cache-control", "no-cache, max-age=3600")
	{
		cc := parseCacheControl(h)
		noCache, ok := cc["no-cache"]
		if !ok {
			t.Fatalf(`"no-cache" value isn't set`)
		}
		if noCache != "" {
			t.Fatalf(`"no-cache" value isn't blank: %v`, noCache)
		}
		if cc["max-age"] != "3600" {
			t.Fatalf(`"max-age" value isn't "3600": %v`, cc["max-age"])
		}
	}
}

func TestNoCacheRequestExpiration(t *testing.T) {
	resetTest()
	respHeaders := http.Header{}
	respHeaders.Set("Cache-Control", "max-age=7200")

	reqHeaders := http.Header{}
	reqHeaders.Set("Cache-Control", "no-cache")
	if getFreshness(respHeaders, reqHeaders) != transparent {
		t.Fatal("freshness isn't transparent")
	}
}

func TestNoCacheResponseExpiration(t *testing.T) {
	resetTest()
	respHeaders := http.Header{}
	respHeaders.Set("Cache-Control", "no-cache")
	respHeaders.Set("Expires", "Wed, 19 Apr 3000 11:43:00 GMT")

	reqHeaders := http.Header{}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestReqMustRevalidate(t *testing.T) {
	resetTest()
	// not paying attention to request setting max-stale means never returning stale
	// responses, so always acting as if must-revalidate is set
	respHeaders := http.Header{}

	reqHeaders := http.Header{}
	reqHeaders.Set("Cache-Control", "must-revalidate")
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestRespMustRevalidate(t *testing.T) {
	resetTest()
	respHeaders := http.Header{}
	respHeaders.Set("Cache-Control", "must-revalidate")

	reqHeaders := http.Header{}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestFreshExpiration(t *testing.T) {
	resetTest()
	now := time.Now().UTC()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(http.TimeFormat))
	respHeaders.Set("expires", now.Add(time.Duration(2)*time.Second).Format(http.TimeFormat))

	reqHeaders := http.Header{}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	clock = &fakeClock{elapsed: 3 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestMaxAge(t *testing.T) {
	resetTest()
	now := time.Now().UTC()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(http.TimeFormat))
	respHeaders.Set("cache-control", "max-age=2")

	reqHeaders := http.Header{}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	clock = &fakeClock{elapsed: 3 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestMaxAgeZero(t *testing.T) {
	resetTest()
	now := time.Now().UTC()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(http.TimeFormat))
	respHeaders.Set("cache-control", "max-age=0")

	reqHeaders := http.Header{}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestBothMaxAge(t *testing.T) {
	resetTest()
	now := time.Now().UTC()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(http.TimeFormat))
	respHeaders.Set("cache-control", "max-age=2")

	reqHeaders := http.Header{}
	reqHeaders.Set("cache-control", "max-age=0")
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestMinFreshWithExpires(t *testing.T) {
	resetTest()
	now := time.Now().UTC()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(http.TimeFormat))
	respHeaders.Set("expires", now.Add(time.Duration(2)*time.Second).Format(http.TimeFormat))

	reqHeaders := http.Header{}
	reqHeaders.Set("cache-control", "min-fresh=1")
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	reqHeaders = http.Header{}
	reqHeaders.Set("cache-control", "min-fresh=2")
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func TestEmptyMaxStale(t *testing.T) {
	resetTest()
	now := time.Now().UTC()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(http.TimeFormat))
	respHeaders.Set("cache-control", "max-age=20")

	reqHeaders := http.Header{}
	reqHeaders.Set("cache-control", "max-stale")
	clock = &fakeClock{elapsed: 10 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	clock = &fakeClock{elapsed: 60 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}
}

func TestMaxStaleValue(t *testing.T) {
	resetTest()
	now := time.Now().UTC()
	respHeaders := http.Header{}
	respHeaders.Set("date", now.Format(http.TimeFormat))
	respHeaders.Set("cache-control", "max-age=10")

	reqHeaders := http.Header{}
	reqHeaders.Set("cache-control", "max-stale=20")
	clock = &fakeClock{elapsed: 5 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	clock = &fakeClock{elapsed: 15 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != fresh {
		t.Fatal("freshness isn't fresh")
	}

	clock = &fakeClock{elapsed: 30 * time.Second}
	if getFreshness(respHeaders, reqHeaders) != stale {
		t.Fatal("freshness isn't stale")
	}
}

func containsHeader(headers []string, header string) bool {
	for _, v := range headers {
		if http.CanonicalHeaderKey(v) == http.CanonicalHeaderKey(header) {
			return true
		}
	}
	return false
}

func TestGetEndToEndHeaders(t *testing.T) {
	resetTest()
	var (
		headers http.Header
		end2end []string
	)

	headers = http.Header{}
	headers.Set("content-type", "text/html")
	headers.Set("te", "deflate")

	end2end = getEndToEndHeaders(headers)
	if !containsHeader(end2end, "content-type") {
		t.Fatal(`doesn't contain "content-type" header`)
	}
	if containsHeader(end2end, "te") {
		t.Fatal(`doesn't contain "te" header`)
	}

	headers = http.Header{}
	headers.Set("connection", "content-type")
	headers.Set("content-type", "text/csv")
	headers.Set("te", "deflate")
	end2end = getEndToEndHeaders(headers)
	if containsHeader(end2end, "connection") {
		t.Fatal(`doesn't contain "connection" header`)
	}
	if containsHeader(end2end, "content-type") {
		t.Fatal(`doesn't contain "content-type" header`)
	}
	if containsHeader(end2end, "te") {
		t.Fatal(`doesn't contain "te" header`)
	}

	headers = http.Header{}
	end2end = getEndToEndHeaders(headers)
	if len(end2end) != 0 {
		t.Fatal(`non-zero end2end headers`)
	}

	headers = http.Header{}
	headers.Set("connection", "content-type")
	end2end = getEndToEndHeaders(headers)
	if len(end2end) != 0 {
		t.Fatal(`non-zero end2end headers`)
	}
}

type transportMock struct {
	response *http.Response
	err      error
}

func (t transportMock) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	return t.response, t.err
}

// Test that http.Client.Timeout is respected when cache transport is used.
// That is so as long as request cancellation is propagated correctly.
// In the past, that required CancelRequest to be implemented correctly,
// but modern http.Client uses Request.Cancel (or request context) instead,
// so we don't have to do anything.
func TestClientTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timeout test in short mode") // Because it takes at least 3 seconds to run.
	}
	resetTest()
	client := &http.Client{
		Transport: NewMemoryCacheTransport(defaultMaxEntries),
		Timeout:   time.Second,
	}
	started := time.Now().UTC()
	resp, err := client.Get(s.server.URL + "/3seconds")
	taken := time.Since(started)
	if err == nil {
		t.Error("got nil error, want timeout error")
	}
	if resp != nil {
		t.Error("got non-nil resp, want nil resp")
	}
	if taken >= 2*time.Second {
		t.Error("client.Do took 2+ seconds, want < 2 seconds")
	}
}

func TestGetWithStatuseCodes(t *testing.T) {
	statusCaching := map[int]bool{
		// Cacheable status codes
		http.StatusOK:                   true,
		http.StatusNonAuthoritativeInfo: true,
		http.StatusMultipleChoices:      true,
		http.StatusMovedPermanently:     true,
		http.StatusNotFound:             true,
		http.StatusGone:                 true,
		// Some NOT-Cacheable status codes
		http.StatusFound:               false,
		http.StatusNotModified:         false,
		http.StatusBadRequest:          false,
		http.StatusUnauthorized:        false,
		http.StatusInternalServerError: false,
	}

	for statusInt, isCacheable := range statusCaching {
		path := "/status/" + strconv.Itoa(statusInt)
		req, err := http.NewRequest("GET", s.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := s.transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		httputil.DumpResponse(resp, false)
		if resp.Header.Get(XFromCache) != "" {
			t.Fatal("Initial request should not be cached")
		}
		if resp.StatusCode != statusInt {
			t.Errorf("got %d, want %d", resp.StatusCode, statusInt)
		}
		_, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		resp2, err2 := s.transport.RoundTrip(req)
		if err2 != nil {
			t.Fatal(err2)
		}
		httputil.DumpResponse(resp2, false)
		isCached := resp2.Header.Get(XFromCache) == "1"
		if isCacheable != isCached {
			t.Fatalf("Should be cached %s: %t got: %t", path, isCacheable, isCached)
		}
	}
}
