# mini-live-server

A single-file, zero-dependency local dev server: static file serving + Mock API + path rewriting + reverse proxy (with CORS).

## Quick start

```bash
go build -o mini-live-server mini-live-server.go
./mini-live-server -config=config.json
```

Without a `config.json`, it starts with built-in defaults (listens on `:8080`, static directory `./public`).

Open **[`mini-live-server.html`](https://oukishu.github.io/mini-live-server-editor.html)** in a browser, fill in the form, and the JSON updates live on the right. Click **Export config.json** to download it. Use **Import config.json** to load an existing config file back into the form and keep editing.

## Command-line flags

Flags only override the matching config.json field when explicitly passed — an unpassed flag never clobbers a file-based setting with its zero value.

```
-config, -c   Path to config file (default ./config.json)
-bind         Listen address, e.g. :8080
-dir          Static files root directory
-port         Port number, equivalent to -bind ":<port>" (legacy, kept for compatibility)
-api          Deprecated: paired with -target, folded into router
-target       Deprecated: single backend URL, folded into router
-cors         Enable CORS handling
-origin       Access-Control-Allow-Origin value
```

`router`, `routes`, and `mocks` are currently config.json-only; there are no corresponding flags.

## Request handling order

Each request is matched in a fixed order; the first match wins:

1. **Mock** — path + method matches an entry in `mocks`, returns the canned response directly
2. **Proxy** — path matches the longest prefix in `router`, forwarded to that backend
3. **Routes** — path matches the `routes` rewrite table, rewritten to the target file path
4. **Static** — served from the `dir` static directory; a directory without `index.html` returns 403 (no directory listing)

## config.json fields

Proxying has exactly one concept: `router` — **path prefix → backend URL**. A single-backend proxy is just a router with one rule; add more rules to fan requests out to multiple backends by prefix, longest prefix wins.

```jsonc
{
  // ---- listening ----
  "bind": ":8080",          // listen address, takes priority over port
  "port": 8080,             // legacy field, equivalent to bind ":<port>"

  // ---- static files ----
  "dir": "./public",        // static root; leave blank to disable static serving (proxy-only setups)

  // ---- proxy routing: prefix -> backend URL, the only proxy concept ----
  "router": {
    "/api": "http://localhost:3000",       // single-backend setups just need this one entry
    "/api/v2": "http://127.0.0.1:9000",    // or fan out to multiple backends by prefix
    "/upload": "http://127.0.0.1:9100"
  },

  // ---- CORS (from the original gocors) ----
  "cors": true,
  "origin": "",              // leave blank to reflect the request's Origin header, or hardcode one

  // ---- static path rewriting ----
  "routes": {
    "/about": "/about/index.html"
  },

  // ---- mock endpoints ----
  "mocks": [
    {
      "path": "/api/ping",
      "method": "GET",       // leave blank to match any method
      "status": 200,
      "delay_ms": 0,
      "response": { "ok": true }
    }
  ]
}
```

`api` / `target` can still appear in config.json (see migration notes below), but they're only folded into a `router` rule at load time. At runtime the program no longer distinguishes "single-backend mode" from "multi-backend mode" — it's all `router`.