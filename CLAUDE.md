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

Run locally with `direnv exec . ./pws` (the `.envrc` resolves secrets from 1Password via `op read --cache "op://Parents/WeatherLink/API/{key,secret}"`). Copy `.envrc` and populate the required env vars before running the binary directly:

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
3. `discoverStationID()` runs once at startup in `main()` (fails fast on bad key/network) and caches the ID via double-checked locking under `stationIDMutex`
4. `fetchWeatherData()` calls `GET /current/{stationID}` and passes the raw response through `convertWLToLegacy()`, which extracts ISS current conditions — sensor type 43/struct 23 (WeatherLink Console) or 45/struct 10 (WeatherLink Live); barometric pressure comes from sensor type 242/struct 19; local report time is built from the `tz_offset` field in the sensor data
5. `GET /static/` serves embedded CSS and SVG logos

### Decisions & Gotchas

- **Env var fallback** in `readAPIConfig()`/`readRandomSecret()` exists only so local dev doesn't need `/mnt/secrets/` files; production always reads the mounted files. Env var names are derived from the secret filename via `strings.ToUpper()` (`api_secret` → `API_SECRET`) — keep the two in sync.
- **`convertWLToLegacy()` reshapes the WeatherLink v2 response into the internal `weatherObservation`** (a frozen copy of the old Wunderground shape).
- **Wind speed is rounded to int** (`Imperial.WindSpeed` is `int`), so WeatherLink's fractional mph is rounded (e.g. 20.69 → 21).
- **Single-station assumption**: `discoverStationID()` uses `Stations[0]`; an API key with multiple stations always picks the first.
- **Sensor type 43/struct 23 is WeatherLink Console** — the original conversion only handled 45/struct 10 (WeatherLink Live) and failed against a Console station until both were supported.

### Caching & Rate Limits

- `weatherCache` uses `sync.RWMutex` for thread safety
- `shouldFetchNewData()` enforces a 30-minute minimum interval (≤ 2 API calls/hour)
- `isDataFresh()` rejects observations older than 35 minutes; handler returns HTTP 503 if no valid cached data exists

### CI/CD

GitHub Actions (`release.yml`) builds with Docker Buildx for `linux/amd64` and pushes to `ghcr.io/methridge/ocp-pws` on semver tags (`v*.*.*`), tagging major, minor, patch, and `latest`.

- **Releasing**: push an annotated semver tag (`git tag -a vX.Y.Z -m "..." && git push origin vX.Y.Z`) to trigger a release. `v2.0.0` was the WeatherLink v2 conversion (breaking: env vars renamed, station auto-discovery).
- **Testing the workflow without a new tag**: the workflow has a `workflow_dispatch` input — `gh workflow run release.yml -f tag=vX.Y.Z` re-runs the build against an existing tag. A plain push to `main` does **not** trigger it (semver-tag-gated).
- **Action versions**: pinned to Node.js 24-compatible majors (`checkout@v6`, `setup-buildx-action@v4`, `login-action@v4`, `metadata-action@v6`, `build-push-action@v7`) — GitHub forces Node 20 actions to Node 24 on 2026-06-16.
