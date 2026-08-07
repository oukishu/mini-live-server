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
	"strings"
	"time"
)

// MockAPI struct definition
type MockAPI struct {
	Path    string `json:"path"`
	Method  string `json:"method"`
	Status  int    `json:"status"`
	DelayMS int    `json:"delay_ms"`
	Response any    `json:"response"`
}

// Config unified configuration struct corresponding to config.json
type Config struct {
	Dir    string            `json:"dir"`
	Port   int               `json:"port"`
	API    string            `json:"api"`
	Target string            `json:"target"`
	Routes map[string]string `json:"routes"`
	Mocks  []MockAPI         `json:"mocks"`
}

// Default values: used when the field is missing in config.json and not specified via command line
const (
	defaultDir    = "./public"
	defaultPort   = 8080
	defaultAPI    = "/api"
	defaultTarget = "http://localhost:3000"
)

// rawConfig uses pointer fields to parse JSON to distinguish between "field absent" and "field zero-valued"
type rawConfig struct {
	Dir    *string            `json:"dir"`
	Port   *int               `json:"port"`
	API    *string            `json:"api"`
	Target *string            `json:"target"`
	Routes map[string]string  `json:"routes"`
	Mocks  []MockAPI          `json:"mocks"`
}

// loadConfig loads configuration from a JSON file; fills missing fields or non-existent files with hardcoded defaults
func loadConfig(filePath string) (*Config, error) {
	cfg := &Config{
		Dir:    defaultDir,
		Port:   defaultPort,
		API:    defaultAPI,
		Target: defaultTarget,
		Routes: map[string]string{},
		Mocks:  []MockAPI{},
	}

	if filePath == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Configuration file does not exist, use default values directly (can be overridden by command-line args later)
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %v", err)
	}

	if raw.Dir != nil {
		cfg.Dir = *raw.Dir
	}
	if raw.Port != nil {
		cfg.Port = *raw.Port
	}
	if raw.API != nil {
		cfg.API = *raw.API
	}
	if raw.Target != nil {
		cfg.Target = *raw.Target
	}
	if raw.Routes != nil {
		cfg.Routes = raw.Routes
	}
	if raw.Mocks != nil {
		cfg.Mocks = raw.Mocks
	}

	return cfg, nil
}

// findMockAPI checks if there is a matching Mock endpoint for the current request path and method
func findMockAPI(mocks []MockAPI, path string, method string) *MockAPI {
	for _, m := range mocks {
		methodMatch := m.Method == "" || strings.EqualFold(m.Method, method)
		if methodMatch && strings.EqualFold(m.Path, path) {
			return &m
		}
	}
	return nil
}

func main() {
	// 1. -c / -config points to the unified config file (both flags bind to the same variable)
	configPath := "./config.json"
	flag.StringVar(&configPath, "config", "./config.json", "Path to config JSON file")
	flag.StringVar(&configPath, "c", "./config.json", "Path to config JSON file (shorthand)")

	// 2. Keep standalone parameters to override config.json when explicitly passed
	dirFlag := flag.String("dir", "", "Root directory for static files (overrides config.json)")
	portFlag := flag.Int("port", 0, "Server listening port (overrides config.json)")
	apiFlag := flag.String("api", "", "API route prefix (overrides config.json)")
	targetFlag := flag.String("target", "", "Target backend URL for API proxying (overrides config.json)")

	flag.Parse()

	// 3. Load config.json, filling missing fields with default values
	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// 4. Only flags explicitly passed via command line will override config.json values,
	//    preventing unpassed flags' default zero values (e.g., port=0) from accidentally overwriting config file settings
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "dir":
			cfg.Dir = *dirFlag
		case "port":
			cfg.Port = *portFlag
		case "api":
			cfg.API = *apiFlag
		case "target":
			cfg.Target = *targetFlag
		}
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	formattedApiPath := "/" + strings.Trim(cfg.API, "/")

	if _, err := os.Stat(cfg.Dir); os.IsNotExist(err) {
		log.Fatalf("Error: Specified static directory '%s' does not exist!", cfg.Dir)
	}

	if len(cfg.Routes) > 0 {
		fmt.Printf("Loaded %d route rewrite rule(s) from %s\n", len(cfg.Routes), configPath)
	}
	if len(cfg.Mocks) > 0 {
		fmt.Printf("Loaded %d mock API endpoint(s) from %s\n", len(cfg.Mocks), configPath)
	}

	// 5. Configure reverse proxy handler
	var proxyHandler http.Handler
	if cfg.Target != "" {
		targetURL, err := url.Parse(cfg.Target)
		if err != nil {
			log.Fatalf("Invalid proxy target URL format: %v", err)
		}

		proxy := httputil.NewSingleHostReverseProxy(targetURL)
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[Proxy Error] %s -> %s: %v", r.URL.Path, targetURL, err)
			http.Error(w, "Bad Gateway: Failed to proxy request", http.StatusBadGateway)
		}

		proxyHandler = proxy
		fmt.Printf("API Proxy enabled: %s/* -> %s/*\n", formattedApiPath, cfg.Target)
	}

	// 6. Static file handler
	fileServer := http.FileServer(http.Dir(cfg.Dir))

	// 7. Main router logic
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Step A: Priority check for matching Mock RESTful API
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

		// Step B: If no Mock matched, check if request should go through backend API proxy
		if proxyHandler != nil && (r.URL.Path == formattedApiPath || strings.HasPrefix(r.URL.Path, formattedApiPath+"/")) {
			proxyHandler.ServeHTTP(w, r)
			return
		}

		// Step C: Route rewrite logic (matches page mappings)
		if targetFile, exists := cfg.Routes[r.URL.Path]; exists {
			r.URL.Path = "/" + strings.TrimPrefix(targetFile, "/")
		}

		// Step D: Serve static assets/pages
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

	fmt.Printf("Config file: %s\n", configPath)
	fmt.Printf("Web server running at: http://localhost%s\n", addr)
	fmt.Printf("Static directory: %s\n", cfg.Dir)

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal("Server failed to start: ", err)
	}
}