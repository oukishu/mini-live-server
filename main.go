package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// MockAPI describes a single fake endpoint served directly from config.json,
// without touching the backend at all.
type MockAPI struct {
	Path     string `json:"path"`
	Method   string `json:"method"`
	Status   int    `json:"status"`
	DelayMS  int    `json:"delay_ms"`
	Response any    `json:"response"`
}

// RouteTarget describes a single router entry: the backend origin to
// forward to, and an optional upstream proxy to dial that backend through.
//
// Accepts two JSON shapes:
//
//	"/api": "http://localhost:3000"
//
// or, when a proxy is needed:
//
//	"/api": {"backend": "http://localhost:3000", "proxy": "socks5://127.0.0.1:1080"}
//
// Proxy is optional — leave it empty (or use the plain string form) to talk
// to the backend directly. Supported proxy schemes: http://, https://,
// socks5://.
type RouteTarget struct {
	Backend string `json:"backend"`
	Proxy   string `json:"proxy"`
}

// UnmarshalJSON allows a router entry to be either a plain backend URL
// string or a {"backend": "...", "proxy": "..."} object.
func (rt *RouteTarget) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		rt.Backend = s
		rt.Proxy = ""
		return nil
	}

	type alias RouteTarget
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*rt = RouteTarget(a)
	return nil
}

// Config is the full, unified configuration. It merges what used to be two
// separate tools:
//   - mini-live-server: static file serving, mock APIs, path rewrites
//   - gocors: CORS handling and multi-prefix reverse proxying
//
// There is exactly one proxy concept: Router, a map of path prefix -> route
// target. A single-backend setup is just a Router with one entry. Each
// entry may independently opt into forwarding through an HTTP or SOCKS5
// proxy.
type Config struct {
	Bind string `json:"bind"` // listen address, e.g. ":8080" or "127.0.0.1:8080"
	Dir  string `json:"dir"`  // static file root; "" disables static serving

	// Router maps a path prefix to a route target. The longest matching
	// prefix wins. A single-backend proxy is just one entry, e.g.
	// {"/api": "http://localhost:3000"}.
	Router map[string]RouteTarget `json:"router"`

	// CORS
	CORS   bool   `json:"cors"`   // enable CORS header handling
	Origin string `json:"origin"` // Access-Control-Allow-Origin; "" reflects the request's Origin header

	Routes map[string]string `json:"routes"` // static path rewrite: request path -> file path
	Mocks  []MockAPI          `json:"mocks"`

	// Deprecated: api/target from the old mini-live-server config shape.
	// Only read at load time and folded into Router; never consulted
	// afterwards. Kept so old config.json files keep working unmodified.
	API    string `json:"api"`
	Target string `json:"target"`
}

const (
	defaultDir  = "./public"
	defaultBind = ":8080"
	defaultAPI  = "/api"
)

// rawConfig mirrors Config but with pointer fields, so we can tell "absent
// from JSON" apart from "present with the zero value".
type rawConfig struct {
	Bind   *string                `json:"bind"`
	Dir    *string                `json:"dir"`
	Router map[string]RouteTarget `json:"router"`
	CORS   *bool                  `json:"cors"`
	Origin *string                `json:"origin"`
	Routes map[string]string      `json:"routes"`
	Mocks  []MockAPI              `json:"mocks"`
	API    *string                `json:"api"`    // deprecated, folded into Router
	Target *string                `json:"target"` // deprecated, folded into Router
}

func loadConfig(filePath string) (*Config, error) {
	cfg := &Config{
		Dir:    defaultDir,
		Bind:   defaultBind,
		Router: map[string]RouteTarget{},
		Routes: map[string]string{},
		Mocks:  []MockAPI{},
	}

	if filePath == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No config file: fall back to defaults, flags may still override.
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %v", err)
	}

	if raw.Bind != nil {
		cfg.Bind = *raw.Bind
	}
	if raw.Dir != nil {
		cfg.Dir = *raw.Dir
	}
	if raw.Router != nil {
		cfg.Router = raw.Router
	}
	if raw.API != nil {
		cfg.API = *raw.API
	}
	if raw.Target != nil {
		cfg.Target = *raw.Target
	}
	if raw.CORS != nil {
		cfg.CORS = *raw.CORS
	}
	if raw.Origin != nil {
		cfg.Origin = *raw.Origin
	}
	if raw.Routes != nil {
		cfg.Routes = raw.Routes
	}
	if raw.Mocks != nil {
		cfg.Mocks = raw.Mocks
	}

	return cfg, nil
}

// resolveBind returns the final listen address, falling back to
// defaultBind when nothing was configured.
func (c *Config) resolveBind() string {
	if strings.TrimSpace(c.Bind) != "" {
		return c.Bind
	}
	return defaultBind
}

// foldLegacyProxyFields folds the deprecated single-backend api/target pair
// into a Router entry, so the rest of the program only ever has to think
// about Router. Called once after config + flags are fully resolved.
// An explicit Router entry for the same prefix always wins.
func (c *Config) foldLegacyProxyFields() {
	if c.Target == "" {
		return
	}
	prefix := c.API
	if strings.TrimSpace(prefix) == "" {
		prefix = defaultAPI
	}
	prefix = "/" + strings.Trim(prefix, "/")

	if c.Router == nil {
		c.Router = map[string]RouteTarget{}
	}
	if _, exists := c.Router[prefix]; !exists {
		c.Router[prefix] = RouteTarget{Backend: c.Target}
		fmt.Printf("Folded deprecated api/target into router: %s -> %s\n", prefix, c.Target)
	}
}

// ---------------------------------------------------------------------------
// Mock API matching
// ---------------------------------------------------------------------------

func findMockAPI(mocks []MockAPI, path string, method string) *MockAPI {
	for i := range mocks {
		m := &mocks[i]
		methodMatch := m.Method == "" || strings.EqualFold(m.Method, method)
		if methodMatch && strings.EqualFold(m.Path, path) {
			return m
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reverse proxy: multi-prefix router, each entry optionally tunneled
// through its own HTTP or SOCKS5 proxy.
// ---------------------------------------------------------------------------

// routeEntry is one resolved router prefix: its backend URL and a
// ready-to-use reverse proxy (already wired to the right transport,
// direct or via an upstream proxy).
type routeEntry struct {
	prefix string
	target *url.URL
	proxy  *httputil.ReverseProxy
}

// routeTable holds resolved route entries, sorted longest prefix first so
// overlapping prefixes resolve deterministically.
type routeTable struct {
	entries []routeEntry
}

// buildTransport returns an http.RoundTripper for a route entry. An empty
// proxyURL means "talk to the backend directly". Supported proxy schemes
// are http://, https:// (forward/CONNECT proxy) and socks5://.
func buildTransport(proxyURL string) (http.RoundTripper, error) {
	base := http.DefaultTransport.(*http.Transport).Clone()

	if strings.TrimSpace(proxyURL) == "" {
		return base, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL %q: %v", proxyURL, err)
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		base.Proxy = http.ProxyURL(u)
		return base, nil

	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("failed to create socks5 dialer for %q: %v", proxyURL, err)
		}
		base.Proxy = nil
		base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
		return base, nil

	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q for %q (use http://, https:// or socks5://)", u.Scheme, proxyURL)
	}
}

func newRouteTable(router map[string]RouteTarget) (*routeTable, error) {
	rt := &routeTable{}

	for prefix, rtgt := range router {
		if strings.TrimSpace(rtgt.Backend) == "" {
			return nil, fmt.Errorf("router prefix %q has no backend", prefix)
		}
		target, err := url.Parse(rtgt.Backend)
		if err != nil {
			return nil, fmt.Errorf("invalid router backend for prefix %q: %v", prefix, err)
		}

		transport, err := buildTransport(rtgt.Proxy)
		if err != nil {
			return nil, fmt.Errorf("router prefix %q: %v", prefix, err)
		}

		dest := target // capture for closure
		p := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = dest.Scheme
				req.URL.Host = dest.Host
				req.Host = dest.Host
				if dest.Path != "" && dest.Path != "/" {
					req.URL.Path = strings.TrimRight(dest.Path, "/") + req.URL.Path
				}
			},
			Transport: transport,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				log.Printf("[Proxy Error] %s: %v", r.URL.Path, err)
				http.Error(w, "Bad Gateway: Failed to proxy request", http.StatusBadGateway)
			},
		}

		rt.entries = append(rt.entries, routeEntry{prefix: prefix, target: target, proxy: p})

		if rtgt.Proxy != "" {
			fmt.Printf("Router: %s -> %s (via proxy %s)\n", prefix, rtgt.Backend, rtgt.Proxy)
		} else {
			fmt.Printf("Router: %s -> %s (direct)\n", prefix, rtgt.Backend)
		}
	}

	sort.Slice(rt.entries, func(i, j int) bool {
		return len(rt.entries[i].prefix) > len(rt.entries[j].prefix)
	})
	return rt, nil
}

func (rt *routeTable) match(path string) (*routeEntry, bool) {
	for i := range rt.entries {
		if strings.HasPrefix(path, rt.entries[i].prefix) {
			return &rt.entries[i], true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// CORS (from gocors)
// ---------------------------------------------------------------------------

func corsMiddleware(cfg *Config, next http.Handler) http.Handler {
	if !cfg.CORS {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := cfg.Origin
		if origin == "" {
			origin = r.Header.Get("Origin")
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods",
				"GET, PUT, POST, HEAD, TRACE, DELETE, PATCH, COPY, LINK, OPTIONS")
		}

		if r.Method == http.MethodOptions {
			if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
				w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	configPath := "./config.json"
	flag.StringVar(&configPath, "config", "./config.json", "Path to config JSON file")
	flag.StringVar(&configPath, "c", "./config.json", "Path to config JSON file (shorthand)")

	bindFlag := flag.String("bind", "", "Listen address, e.g. :8080 (overrides config.json)")
	dirFlag := flag.String("dir", "", "Root directory for static files (overrides config.json)")
	apiFlag := flag.String("api", "", "Deprecated: path prefix paired with -target, folded into router (overrides config.json)")
	targetFlag := flag.String("target", "", "Deprecated: single backend URL, folded into router (overrides config.json)")
	corsFlag := flag.Bool("cors", false, "Enable CORS header handling (overrides config.json)")
	originFlag := flag.String("origin", "", "Access-Control-Allow-Origin value; empty reflects request Origin (overrides config.json)")

	flag.Parse()

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// Only flags explicitly passed on the command line override config.json,
	// so unpassed flags' zero values never clobber file-based settings.
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "bind":
			cfg.Bind = *bindFlag
		case "dir":
			cfg.Dir = *dirFlag
		case "api":
			cfg.API = *apiFlag
		case "target":
			cfg.Target = *targetFlag
		case "cors":
			cfg.CORS = *corsFlag
		case "origin":
			cfg.Origin = *originFlag
		}
	})

	// Fold the deprecated api/target pair into Router so everything past
	// this point only has one proxy concept to deal with.
	cfg.foldLegacyProxyFields()

	addr := cfg.resolveBind()

	// Static serving is optional: a proxy-only deployment (the old gocors
	// use case) simply omits/clears "dir".
	staticEnabled := cfg.Dir != ""
	if staticEnabled {
		if _, err := os.Stat(cfg.Dir); os.IsNotExist(err) {
			log.Printf("Warning: static directory %q does not exist, static serving disabled", cfg.Dir)
			staticEnabled = false
		}
	}

	if len(cfg.Routes) > 0 {
		fmt.Printf("Loaded %d route rewrite rule(s) from %s\n", len(cfg.Routes), configPath)
	}
	if len(cfg.Mocks) > 0 {
		fmt.Printf("Loaded %d mock API endpoint(s) from %s\n", len(cfg.Mocks), configPath)
	}

	rt, err := newRouteTable(cfg.Router)
	if err != nil {
		log.Fatalf("Error parsing router config: %v", err)
	}
	if len(cfg.Router) > 0 {
		fmt.Printf("Loaded %d proxy router prefix(es) from %s\n", len(cfg.Router), configPath)
	}

	proxyEnabled := len(cfg.Router) > 0

	var fileServer http.Handler
	if staticEnabled {
		fileServer = http.FileServer(http.Dir(cfg.Dir))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Step A: mock APIs take priority over everything else.
		if mock := findMockAPI(cfg.Mocks, r.URL.Path, r.Method); mock != nil {
			if mock.DelayMS > 0 {
				time.Sleep(time.Duration(mock.DelayMS) * time.Millisecond)
			}
			status := mock.Status
			if status == 0 {
				status = http.StatusOK
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			if err := json.NewEncoder(w).Encode(mock.Response); err != nil {
				log.Printf("[Mock API Error] Failed to respond to %s: %v", r.URL.Path, err)
			}
			log.Printf("[Mock API Hit] %s %s -> Status %d", r.Method, r.URL.Path, status)
			return
		}

		// Step B: proxy — router prefix match (each entry direct or via
		// its own HTTP/SOCKS5 proxy, per config).
		if proxyEnabled {
			if entry, ok := rt.match(r.URL.Path); ok {
				entry.proxy.ServeHTTP(w, r)
				return
			}
		}

		if !staticEnabled {
			http.NotFound(w, r)
			return
		}

		// Step C: static path rewrite.
		if targetFile, exists := cfg.Routes[r.URL.Path]; exists {
			r.URL.Path = "/" + strings.TrimPrefix(targetFile, "/")
		}

		// Step D: serve static assets/pages.
		filePath := filepath.Join(cfg.Dir, filepath.Clean(r.URL.Path))
		info, err := os.Stat(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		if info.IsDir() {
			indexPath := filepath.Join(filePath, "index.html")
			if _, err := os.Stat(indexPath); os.IsNotExist(err) {
				http.Error(w, "403 Forbidden: Directory listing disabled.", http.StatusForbidden)
				return
			}
		}

		fileServer.ServeHTTP(w, r)
	})

	handler := corsMiddleware(cfg, mux)

	fmt.Printf("Config file: %s\n", configPath)
	fmt.Printf("Server running at: http://localhost%s\n", addr)
	if staticEnabled {
		fmt.Printf("Static directory: %s\n", cfg.Dir)
	}
	if cfg.CORS {
		originDesc := cfg.Origin
		if originDesc == "" {
			originDesc = "(reflects request Origin)"
		}
		fmt.Printf("CORS enabled, Allow-Origin: %s\n", originDesc)
	}

	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal("Server failed to start: ", err)
	}
}
