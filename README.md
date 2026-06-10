# Personal Weather Station Current Data

Web app that displays current conditions from a Davis Instruments Personal Weather
Station via the [WeatherLink v2 API](https://weatherlink.github.io/v2-api/). Designed
to run in [OpenShift](https://www.openshift.com/) with
[HashiCorp Vault](https://www.vaultproject.io) and the
[Vault Secrets Operator](https://developer.hashicorp.com/vault/docs/deploy/kubernetes/vso)
for secret management.

## Prerequisites

- Davis Instruments PWS with a WeatherLink Console or WeatherLink Live data logger
- WeatherLink v2 API key and secret (from [weatherlink.com](https://www.weatherlink.com))
- Go 1.24+ (for local development)
- [direnv](https://direnv.net/) + [1Password CLI](https://developer.1password.com/docs/cli/) (for the default `.envrc` setup)

## Local Development

Copy `.envrc` and populate the required variables, then run:

```bash
direnv exec . ./pws
```

The app serves on `http://localhost:8080`.

### Environment Variables

| Variable | Required | Purpose |
|---|---|---|
| `API` | Yes | WeatherLink v2 base URL (`https://api.weatherlink.com/v2`) |
| `KEY` | Yes | WeatherLink API key |
| `API_SECRET` | Yes | WeatherLink API secret |
| `RANDOM_SECRET` | Yes | Secret value displayed in the UI |
| `DEBUG` | No | Set to any value to enable verbose logging |
| `FETCH_BUFFER_SECONDS` | No | Cache buffer window in seconds (default: 30) |

In production, these are mounted as files at `/mnt/secrets/{api,key,api_secret,rsec,fetch_buffer}`
by the Vault Secrets Operator. The app falls back to env vars if the secret files are absent.

## Build & Run

```bash
go build -o pws .          # build binary
go test ./...              # run tests
task build-container       # build Docker image (multi-stage → scratch, UPX-compressed)
task run-container         # run container locally
```

The container image is built for `linux/amd64` and uses a `scratch` base with UPX compression
for minimal size. The `task run-container` command loads credentials from `.envrc` via direnv
or 1Password CLI.

## CI/CD

GitHub Actions (`release.yml`) builds and pushes to `ghcr.io/methridge/ocp-pws` on semver tags
(`v*.*.*`), tagging major, minor, patch, and `latest`.
