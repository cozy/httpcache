// Package httpcache provides a http.RoundTripper implementation that works as a
// mostly RFC-compliant cache for http responses.
//
// It is only suitable for use as a 'private' cache (i.e. for a web-browser or an API-client
// and not for a shared proxy).
//
package httpcache

import (
	"bufio"
	"bytes"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cozy/httpcache/lru"
)

const (
	stale = iota
	fresh
	transparent
)

// XFromCache is the header added to responses that are returned from the cache
const XFromCache = "X-From-Cache"

var cacheableResponseCodes = map[int]struct{}{
	http.StatusOK:                   {}, // 200
	http.StatusNonAuthoritativeInfo: {}, // 203
	http.StatusNoContent:            {}, // 204
	http.StatusMultipleChoices:      {}, // 300
	http.StatusMovedPermanently:     {}, // 301
	http.StatusNotFound:             {}, // 404
	http.StatusGone:                 {}, // 410
}

// A Cache interface is used by the Transport to store and retrieve responses.
type Cache interface {
	// Get returns the []byte representation of a cached response and a bool
	// set to true if the value isn't empty
	Get(key string) (responseBytes []byte, ok bool)
	// Set stores the []byte representation of a response against a key
	Set(key string, responseBytes []byte)
	// Delete removes the value associated with the key
	Delete(key string)
}

// cacheKey returns the cache key for req.
func cacheKey(req *http.Request) string {
	if req.Method == http.MethodGet {
		return req.URL.String()
	} else {
		return req.Method + " " + req.URL.String()
	}
}

// CachedResponse returns the cached http.Response for req if present, and nil
// otherwise.
func CachedResponse(c Cache, req *http.Request) (resp *http.Response, err error) {
	cachedVal, ok := c.Get(cacheKey(req))
	if !ok {
		return
	}
	b := bytes.NewBuffer(cachedVal)
	return http.ReadResponse(bufio.NewReader(b), req)
}

// MemoryCache is an implemtation of Cache that stores responses in an in-memory map.
type MemoryCache struct {
	mu    sync.RWMutex
	items *lru.Cache
}

// Get returns the []byte representation of the response and true if present, false if not
func (c *MemoryCache) Get(key string) (resp []byte, ok bool) {
	c.mu.Lock()
	resp, ok = c.items.Get(lru.Key(key))
	c.mu.Unlock()
	return resp, ok
}

// Set saves response resp to the cache with key
func (c *MemoryCache) Set(key string, resp []byte) {
	c.mu.Lock()
	c.items.Add(lru.Key(key), resp)
	c.mu.Unlock()
}

// Delete removes key from the cache
func (c *MemoryCache) Delete(key string) {
	c.mu.Lock()
	c.items.Remove(lru.Key(key))
	c.mu.Unlock()
}

// NewMemoryCache returns a new Cache that will store items in an in-memory map
func NewMemoryCache(maxEntries int) *MemoryCache {
	return &MemoryCache{items: lru.New(maxEntries)}
}

// Transport is an implementation of http.RoundTripper that will return values from a cache
// where possible (avoiding a network request) and will additionally add validators (etag/if-modified-since)
// to repeated requests allowing servers to return 304 / Not Modified
type Transport struct {
	// The RoundTripper interface actually used to make requests
	// If nil, http.DefaultTransport is used
	Transport http.RoundTripper
	Cache     Cache
	// If true, responses returned from the cache will be given an extra header, X-From-Cache
	MarkCachedResponses bool
}

// NewTransport returns a new Transport with the
// provided Cache implementation and MarkCachedResponses set to true
func NewTransport(c Cache) *Transport {
	return &Transport{Cache: c, MarkCachedResponses: true}
}

// Client returns an *http.Client that caches responses.
func (t *Transport) Client() *http.Client {
	return &http.Client{Transport: t}
}

// RoundTrip takes a Request and returns a Response
//
// If there is a fresh Response already in cache, then it will be returned without connecting to
// the server.
//
// If there is a stale Response, then any validators it contains will be set on the new request
// to give the server a chance to respond with NotModified. If this happens, then the cached Response
// will be returned.
func (t *Transport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	cacheKey := cacheKey(req)
	cacheable := (req.Method == http.MethodGet || req.Method == http.MethodHead) && req.Header.Get("range") == ""
	var cachedResp *http.Response
	if cacheable {
		cachedResp, err = CachedResponse(t.Cache, req)
	}

	transport := t.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	if cacheable && cachedResp != nil && err == nil {
		if t.MarkCachedResponses {
			cachedResp.Header.Set(XFromCache, "1")
		}

		// Can only use cached value if the new request doesn't Vary significantly
		switch getFreshness(cachedResp.Header, req.Header) {
		case fresh:
			return cachedResp, nil
		case stale:
			var req2 *http.Request
			// Add validators if caller hasn't already done so
			etag := cachedResp.Header.Get("etag")
			if etag != "" && req.Header.Get("etag") == "" {
				req2 = cloneRequest(req)
				req2.Header.Set("if-none-match", etag)
			}
			lastModified := cachedResp.Header.Get("last-modified")
			if lastModified != "" && req.Header.Get("last-modified") == "" {
				if req2 == nil {
					req2 = cloneRequest(req)
				}
				req2.Header.Set("if-modified-since", lastModified)
			}
			if req2 != nil {
				req = req2
			}
		}

		resp, err = transport.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotModified {
			// Replace the 304 response with the one from cache, but update with some new headers
			endToEndHeaders := getEndToEndHeaders(resp.Header)
			for _, header := range endToEndHeaders {
				cachedResp.Header[header] = resp.Header[header]
			}
			respBytes, err := httputil.DumpResponse(cachedResp, true)
			if err == nil {
				t.Cache.Set(cacheKey, respBytes)
			}
			return cachedResp, nil
		}
	} else {
		reqCacheControl := parseCacheControl(req.Header)
		if _, ok := reqCacheControl["only-if-cached"]; ok {
			resp = newGatewayTimeoutResponse(req)
		} else {
			resp, err = transport.RoundTrip(req)
			if err != nil {
				return nil, err
			}
		}
	}

	storeable := cacheable && canStore(resp.StatusCode,
		parseCacheControl(req.Header),
		parseCacheControl(resp.Header))
	if storeable {
		if req.Method == http.MethodGet && resp.StatusCode != http.StatusNoContent {
			// Delay caching until EOF is reached.
			resp.Body = &cachingReadCloser{
				R: resp.Body,
				OnClose: func(b []byte) {
					resp := *resp
					resp.Body = ioutil.NopCloser(bytes.NewReader(b))
					respBytes, err := httputil.DumpResponse(&resp, true)
					if err == nil {
						t.Cache.Set(cacheKey, respBytes)
					}
				},
			}
		} else {
			respBytes, err := httputil.DumpResponse(resp, true)
			if err == nil {
				t.Cache.Set(cacheKey, respBytes)
			}
		}
	} else if cachedResp != nil {
		t.Cache.Delete(cacheKey)
	}
	return resp, nil
}

type realClock struct{}

func (c *realClock) since(d time.Time) time.Duration {
	return time.Since(d)
}

type timer interface {
	since(d time.Time) time.Duration
}

var clock timer = &realClock{}

// getFreshness will return one of fresh/stale/transparent based on the cache-control
// values of the request and the response
//
// fresh indicates the response can be returned
// stale indicates that the response needs validating before it is returned
// transparent indicates the response should not be used to fulfil the request
//
// Because this is only a private cache, 'public' and 'private' in cache-control aren't
// signficant. Similarly, smax-age isn't used.
func getFreshness(respHeaders, reqHeaders http.Header) (freshness int) {
	respCacheControl := parseCacheControl(respHeaders)
	reqCacheControl := parseCacheControl(reqHeaders)
	if _, ok := reqCacheControl["no-cache"]; ok {
		return transparent
	}
	if _, ok := respCacheControl["no-cache"]; ok {
		return stale
	}
	if _, ok := reqCacheControl["only-if-cached"]; ok {
		return fresh
	}

	date, ok := parseDate(respHeaders)
	if !ok {
		return stale
	}
	currentAge := clock.since(date)

	var err error
	var lifetime time.Duration
	var zeroDuration time.Duration

	// If a response includes both an Expires header and a max-age directive,
	// the max-age directive overrides the Expires header, even if the Expires header is more restrictive.
	if maxAge, ok := respCacheControl["max-age"]; ok {
		lifetime, err = parseDuration(maxAge)
		if err != nil {
			lifetime = zeroDuration
		}
	} else {
		expiresHeader := respHeaders.Get("expires")
		if expiresHeader != "" {
			var expires time.Time
			expires, err = time.Parse(http.TimeFormat, expiresHeader)
			if err != nil {
				lifetime = zeroDuration
			} else {
				lifetime = expires.Sub(date)
			}
		}
	}

	if maxAge, ok := reqCacheControl["max-age"]; ok {
		// the client is willing to accept a response whose age is no greater than the specified time in seconds
		lifetime, err = parseDuration(maxAge)
		if err != nil {
			lifetime = zeroDuration
		}
	}

	if minfresh, ok := reqCacheControl["min-fresh"]; ok {
		//  the client wants a response that will still be fresh for at least the specified number of seconds.
		minfreshDuration, err := parseDuration(minfresh)
		if err == nil {
			currentAge = currentAge + minfreshDuration
		}
	}

	if maxstale, ok := reqCacheControl["max-stale"]; ok {
		// Indicates that the client is willing to accept a response that has exceeded its expiration time.
		// If max-stale is assigned a value, then the client is willing to accept a response that has exceeded
		// its expiration time by no more than the specified number of seconds.
		// If no value is assigned to max-stale, then the client is willing to accept a stale response of any age.
		//
		// Responses served only because of a max-stale value are supposed to have a Warning header added to them,
		// but that seems like a  hassle, and is it actually useful? If so, then there needs to be a different
		// return-value available here.
		if maxstale == "" {
			return fresh
		}
		maxstaleDuration, err := parseDuration(maxstale)
		if err == nil {
			currentAge = currentAge - maxstaleDuration
		}
	}

	if lifetime > currentAge {
		return fresh
	}

	return stale
}

func getEndToEndHeaders(respHeaders http.Header) []string {
	// These headers are always hop-by-hop
	hopByHopHeaders := map[string]struct{}{
		"Connection":          {},
		"Keep-Alive":          {},
		"Proxy-Authenticate":  {},
		"Proxy-Authorization": {},
		"Te":                  {},
		"Trailers":            {},
		"Transfer-Encoding":   {},
		"Upgrade":             {},
	}

	for _, extra := range strings.Split(respHeaders.Get("connection"), ",") {
		// any header listed in connection, if present, is also considered hop-by-hop
		if strings.Trim(extra, " ") != "" {
			hopByHopHeaders[http.CanonicalHeaderKey(extra)] = struct{}{}
		}
	}
	endToEndHeaders := []string{}
	for respHeader := range respHeaders {
		if _, ok := hopByHopHeaders[respHeader]; !ok {
			endToEndHeaders = append(endToEndHeaders, respHeader)
		}
	}
	return endToEndHeaders
}

func canStore(code int, reqCacheControl, respCacheControl cacheControl) (canStore bool) {
	if _, ok := cacheableResponseCodes[code]; !ok {
		return false
	}
	if _, ok := respCacheControl["no-store"]; ok {
		return false
	}
	if _, ok := reqCacheControl["no-store"]; ok {
		return false
	}
	return true
}

func newGatewayTimeoutResponse(req *http.Request) *http.Response {
	var braw bytes.Buffer
	braw.WriteString("HTTP/1.1 504 Gateway Timeout\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(&braw), req)
	if err != nil {
		panic(err)
	}
	return resp
}

// cloneRequest returns a clone of the provided *http.Request.
// The clone is a shallow copy of the struct and its Header map.
// (This function copyright goauth2 authors: https://code.google.com/p/goauth2)
func cloneRequest(r *http.Request) *http.Request {
	// shallow copy of the struct
	r2 := new(http.Request)
	*r2 = *r
	// deep copy of the Header
	r2.Header = make(http.Header)
	for k, s := range r.Header {
		r2.Header[k] = s
	}
	return r2
}

type cacheControl map[string]string

func parseCacheControl(headers http.Header) cacheControl {
	cc := cacheControl{}
	ccHeader := headers.Get("Cache-Control")
	for _, part := range strings.Split(ccHeader, ",") {
		part = strings.Trim(part, " ")
		if part == "" {
			continue
		}
		if strings.ContainsRune(part, '=') {
			keyval := strings.Split(part, "=")
			cc[strings.Trim(keyval[0], " ")] = strings.Trim(keyval[1], ",")
		} else {
			cc[part] = ""
		}
	}
	return cc
}

// cachingReadCloser is a wrapper around ReadCloser R that calls OnClose
// handler with a full copy of the content read from R when close is
// called.
type cachingReadCloser struct {
	// Underlying ReadCloser.
	R io.ReadCloser
	// OnClose is called with a copy of the content of R when EOF is reached.
	OnClose func([]byte)

	err error
	buf bytes.Buffer // buf stores a copy of the content of R.
}

// Read reads the next len(p) bytes from R or until R is drained. The
// return value n is the number of bytes read. If R has no data to
// return, err is io.EOF and OnClose is called with a full copy of what
// has been read so far.
func (r *cachingReadCloser) Read(p []byte) (n int, err error) {
	n, err = r.R.Read(p)
	r.buf.Write(p[:n])
	if r.err == nil {
		r.err = err
	}
	return n, err
}

func (r *cachingReadCloser) Close() error {
	errc := r.R.Close()
	if errc == nil && r.err == io.EOF {
		r.OnClose(r.buf.Bytes())
	}
	return errc
}

// NewMemoryCacheTransport returns a new Transport using the in-memory cache implementation
func NewMemoryCacheTransport(maxEntries int) *Transport {
	c := NewMemoryCache(maxEntries)
	t := NewTransport(c)
	return t
}

func parseDuration(s string) (time.Duration, error) {
	i, err := strconv.ParseInt(s, 10, 0)
	if err != nil {
		return 0, err
	}
	return time.Duration(i) * time.Second, nil
}

func parseDate(respHeaders http.Header) (date time.Time, ok bool) {
	dateHeader := respHeaders.Get("date")
	if dateHeader == "" {
		return
	}
	var err error
	date, err = time.Parse(http.TimeFormat, dateHeader)
	ok = (err == nil)
	return
}
