# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build -o pws .        # build binary locally
go test ./...            # run tests
task build-container     # build Docker image (multi-stage → scratch, UPX-compressed)
task run-container       # run container locally (sources .envrc / 1Password)
```

Binary serves on `:8080`.

### Local Development

Copy `.envrc` and populate the required env vars before running the binary directly:

| Variable | Purpose |
|---|---|
| `API` | WeatherLink v2 base URL |
| `KEY` | WeatherLink API key |
| `API_SECRET` | WeatherLink API secret |
| `RANDOM_SECRET` | Secret displayed in the UI |
| `DEBUG` | Optional — enable verbose logging |
| `FETCH_BUFFER_SECONDS` | Optional — cache buffer window (default 30s) |

## Architecture

**Single-file Go app** (`main.go`), no external dependencies — stdlib only.

### Secrets & Config

- **Production**: Vault Secrets Operator mounts credentials at `/mnt/secrets/{api,key,api_secret,rsec,fetch_buffer}`
- **Local dev**: if a secrets file is missing, `readAPIConfig()` falls back to the uppercase env var (`API`, `KEY`, `API_SECRET`); `readRandomSecret()` falls back to `RANDOM_SECRET`

### Embedded Assets

`//go:embed` bundles `static/` (CSS, SVGs) and `templates/` (HTML) into the binary at compile time. Changes to those directories require a rebuild.

### Request Flow

1. `GET /` → single handler renders `templates/index.html` with processed `Index` struct
2. `getCachedWeatherData()` checks `weatherCache` — fetches from WeatherLink only if > 30 min since last call
3. `discoverStationID()` calls `GET /stations` once (mutex-protected), then caches the station ID
4. `fetchWeatherData()` calls `GET /current/{stationID}` and passes the raw response through `convertWLToLegacy()`, which extracts ISS current conditions — sensor type 43/struct 23 (WeatherLink Console) or 45/struct 10 (WeatherLink Live); barometric pressure comes from sensor type 242/struct 19
5. `GET /static/` serves embedded CSS and SVG logos

### Caching & Rate Limits

- `weatherCache` uses `sync.RWMutex` for thread safety
- `shouldFetchNewData()` enforces a 30-minute minimum interval (≤ 2 API calls/hour)
- `isDataFresh()` rejects observations older than 35 minutes; handler returns HTTP 503 if no valid cached data exists

### CI/CD

GitHub Actions (`release.yml`) builds with Docker Buildx for `linux/amd64` and pushes to `ghcr.io/methridge/ocp-pws` on semver tags (`v*.*.*`), tagging major, minor, patch, and `latest`.
