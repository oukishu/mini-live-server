package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

// Config is the full, unified configuration. It merges what used to be two
// separate tools:
//   - mini-live-server: static file serving, mock APIs, path rewrites
//   - gocors: CORS handling and multi-prefix reverse proxying
//
// There is exactly one proxy concept: Router, a map of path prefix -> backend
// origin. A single-backend setup is just a Router with one entry.
type Config struct {
	Bind string `json:"bind"` // listen address, e.g. ":8080" or "127.0.0.1:8080"
	Port int    `json:"port"` // legacy alternative to Bind (":<port>")
	Dir  string `json:"dir"`  // static file root; "" disables static serving

	// Router maps a path prefix to a backend origin. The longest matching
	// prefix wins. A single-backend proxy is just one entry, e.g.
	// {"/api": "http://localhost:3000"}.
	Router map[string]string `json:"router"`

	// CORS
	CORS   bool   `json:"cors"`   // enable CORS header handling
	Origin string `json:"origin"` // Access-Control-Allow-Origin; "" reflects the request's Origin header

	Routes map[string]string `json:"routes"` // static path rewrite: request path -> file path
	Mocks  []MockAPI         `json:"mocks"`

	// Deprecated: api/target from the old mini-live-server config shape.
	// Only read at load time and folded into Router; never consulted
	// afterwards. Kept so old config.json files keep working unmodified.
	API    string `json:"api"`
	Target string `json:"target"`
}

const (
	defaultDir  = "./public"
	defaultPort = 8080
	defaultAPI  = "/api"
)

// rawConfig mirrors Config but with pointer fields, so we can tell "absent
// from JSON" apart from "present with the zero value".
type rawConfig struct {
	Bind   *string           `json:"bind"`
	Port   *int              `json:"port"`
	Dir    *string           `json:"dir"`
	Router map[string]string `json:"router"`
	CORS   *bool             `json:"cors"`
	Origin *string           `json:"origin"`
	Routes map[string]string `json:"routes"`
	Mocks  []MockAPI         `json:"mocks"`
	API    *string           `json:"api"`    // deprecated, folded into Router
	Target *string           `json:"target"` // deprecated, folded into Router
}

func loadConfig(filePath string) (*Config, error) {
	cfg := &Config{
		Dir:    defaultDir,
		Port:   defaultPort,
		Router: map[string]string{},
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
	if raw.Port != nil {
		cfg.Port = *raw.Port
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

// resolveBind turns Bind/Port into a final listen address, preferring an
// explicit Bind.
func (c *Config) resolveBind() string {
	if strings.TrimSpace(c.Bind) != "" {
		return c.Bind
	}
	port := c.Port
	if port == 0 {
		port = defaultPort
	}
	return fmt.Sprintf(":%d", port)
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
		c.Router = map[string]string{}
	}
	if _, exists := c.Router[prefix]; !exists {
		c.Router[prefix] = c.Target
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
// Reverse proxy: multi-prefix router (from gocors) + single target (legacy)
// ---------------------------------------------------------------------------

// routeTable holds parsed backend URLs keyed by path prefix, sorted longest
// prefix first so overlapping prefixes resolve deterministically.
type routeTable struct {
	prefixes []string
	targets  map[string]*url.URL
}

func newRouteTable(router map[string]string) (*routeTable, error) {
	rt := &routeTable{targets: map[string]*url.URL{}}
	for prefix, raw := range router {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid router target for prefix %q: %v", prefix, err)
		}
		rt.prefixes = append(rt.prefixes, prefix)
		rt.targets[prefix] = u
	}
	sort.Slice(rt.prefixes, func(i, j int) bool {
		return len(rt.prefixes[i]) > len(rt.prefixes[j])
	})
	return rt, nil
}

func (rt *routeTable) match(path string) (*url.URL, bool) {
	for _, prefix := range rt.prefixes {
		if strings.HasPrefix(path, prefix) {
			return rt.targets[prefix], true
		}
	}
	return nil, false
}

// buildProxy creates a single httputil.ReverseProxy driven entirely by the
// prefix -> backend router table.
func buildProxy(rt *routeTable) *httputil.ReverseProxy {
	director := func(req *http.Request) {
		target, ok := rt.match(req.URL.Path)
		if !ok {
			return
		}
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		if target.Path != "" && target.Path != "/" {
			req.URL.Path = strings.TrimRight(target.Path, "/") + req.URL.Path
		}
	}
	return &httputil.ReverseProxy{
		Director: director,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[Proxy Error] %s: %v", r.URL.Path, err)
			http.Error(w, "Bad Gateway: Failed to proxy request", http.StatusBadGateway)
		},
	}
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
	portFlag := flag.Int("port", 0, "Server listening port, legacy alias for -bind (overrides config.json)")
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
		case "port":
			cfg.Port = *portFlag
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
	if len(cfg.Router) > 0 {
		fmt.Printf("Loaded %d proxy router prefix(es) from %s\n", len(cfg.Router), configPath)
	}

	rt, err := newRouteTable(cfg.Router)
	if err != nil {
		log.Fatalf("Error parsing router config: %v", err)
	}

	var proxyHandler http.Handler
	if len(cfg.Router) > 0 {
		proxyHandler = buildProxy(rt)
	}

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

		// Step B: proxy — router prefix match.
		if proxyHandler != nil {
			if _, ok := rt.match(r.URL.Path); ok {
				proxyHandler.ServeHTTP(w, r)
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
