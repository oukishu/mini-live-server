# mini-live-server

A tiny zero-dependency dev server in Go — static file serving, API reverse proxy, page route rewriting, and mock REST endpoints, all driven by one `config.json`.

## Features

- **Static file server** — serves any directory as-is, no build step
- **API reverse proxy** — forward a path prefix (e.g. `/api`) to a real backend
- **Route rewriting** — map arbitrary request paths to specific HTML files (useful for SPA-style routing without a real router)
- **Mock REST API** — define fake endpoints with custom status, delay, and JSON response, no backend required
- **Single config file** — everything above is configured through one `config.json`, generated visually with the included **[`mini-live-server.html`](https://oukishu.github.io/mini-live-server-editor.html)** editor
- **No dependencies** — pure Go standard library, single binary

## Quick start

```bash
go build -o mini-live-server main.go
./mini-live-server -c ./config.json
```

Or run directly without building:

```bash
go run main.go -c ./config.json
```

## Command-line flags

| Flag | Default | Description |
|---|---|---|
| `-c`, `-config` | `./config.json` | Path to the config file |
| `-dir` | *(from config)* | Static file root directory — overrides `config.json` if set |
| `-port` | *(from config)* | Listening port — overrides `config.json` if set |
| `-api` | *(from config)* | API route prefix — overrides `config.json` if set |
| `-target` | *(from config)* | Backend proxy target URL — overrides `config.json` if set |

Flags only override `config.json` when explicitly passed — omitted flags never clobber values from the config file.

## config.json

```json
{
  "dir": "./public",
  "port": 8080,
  "api": "/api",
  "target": "http://localhost:3000",
  "routes": {
    "/old-page": "/new-page.html"
  },
  "mocks": [
    {
      "path": "/api/user",
      "method": "GET",
      "status": 200,
      "delay_ms": 100,
      "response": { "id": 1, "name": "test" }
    }
  ]
}
```

| Field | Type | Description |
|---|---|---|
| `dir` | string | Static file root directory |
| `port` | number | Listening port |
| `api` | string | API route prefix that gets proxied |
| `target` | string | Backend URL that `api` requests are forwarded to (leave empty to disable proxying) |
| `routes` | object | Map of request path → target file path, rewritten before static file lookup |
| `mocks` | array | List of mock endpoints; each takes `path`, `method` (empty matches any), `status`, `delay_ms`, and `response` |

Any field omitted from `config.json` falls back to a built-in default (`dir=./public`, `port=8080`, `api=/api`, `target=http://localhost:3000`, `routes={}`, `mocks=[]`).

## Generating config.json visually

Rather than hand-writing the JSON, open **[`mini-live-server.html`](https://oukishu.github.io/mini-live-server-editor.html)** — it's a self-contained static page with no build step and no server required. Click the link to open it directly in your browser (or just double-click the file locally), fill in the fields, and:

- **Export** to download a ready-to-use `config.json`
- **Import** an existing `config.json` to edit it further

The page includes a live JSON preview with syntax highlighting so you can see exactly what will be written before exporting.

## License

MIT
