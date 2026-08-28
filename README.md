# Hytale-Auth

Token service for Hytale server authentication.

From [BananaLabs OSS](https://github.com/bananalabs-oss).

## Overview

Hytale-Auth handles OAuth 2.0 device-code flow with Hytale's auth + session services to generate server identity and session tokens. Used as a pre-start hook by Bananagine.

The deployed form is a **Pulp application** (`application/`): Lua owns the OAuth workflow, generic shared engines own outbound HTTP and scoped persistence, and `api-cell/` is a thin inbound HTTP adapter. A legacy standalone native service (`main.go` at repo root) remains as a reference implementation.

## Quick Start (Pulp application)

Build the API adapter and shared engines, then run the application through the deployment host:

```bash
cd api-cell
GOOS=wasip1 GOARCH=wasm go build -o hytale-auth-api.wasm .
```

Build and run the deployment host:

```bash
cd pulp-deployment
go build -o hytale-auth-deployment .
./hytale-auth-deployment
```

On first run, check the host logs for the device-authorization URL and code, then approve in your browser. Tokens persist automatically.

## API Reference

| Method | Endpoint  | Description                  |
| ------ | --------- | ---------------------------- |
| `GET`  | `/`       | Authorization status         |
| `GET`  | `/tokens` | Generate fresh server tokens |
| `GET`  | `/health` | Health check                 |

**GET /tokens Response:**

```json
{
  "env": {
    "HYTALE_SERVER_IDENTITY_TOKEN": "eyJ...",
    "HYTALE_SERVER_SESSION_TOKEN": "eyJ..."
  }
}
```

## Configuration (application)

Inbound authentication is set in `api-cell/pulp.cell.toml`; Hytale endpoint and OAuth parameters are application values in `application/lua-orchestrator.cell.toml`.

| Key             | Description                                                                 | Default |
| --------------- | --------------------------------------------------------------------------- | ------- |
| `service_token` | Shared secret gating `GET /tokens`. Empty = auth off (for Bananagine hook). | `""`    |

When `service_token` is non-empty, callers must send it as `X-Service-Token`. Set in lockstep with the Bananagine pre-start hook configuration.

## Configuration (native reference service)

Priority: CLI flags > environment variables > config file > defaults

| Setting        | Flag         | Env Var    | Default |
| -------------- | ------------ | ---------- | ------- |
| HTTP port      | `--port`     | `PORT`     | `8081`  |
| Data directory | `--data-dir` | `DATA_DIR` | `.`     |

## Docker (native)

```yaml
hytale-auth:
  image: ghcr.io/bananalabs-oss/hytale-auth:latest
  ports:
    - "8081:8081"
  volumes:
    - ./hytale-auth-data:/data
  environment:
    - DATA_DIR=/data
```

First run: check logs for authorization URL, approve in browser. Restarts work automatically.

## Token Flow

1. On startup, verifies stored refresh token
2. If invalid/missing, starts device authorization flow
3. User approves via browser; profile UUID fetched automatically
4. Bananagine calls `/tokens` as pre-start hook
5. Exchanges refresh token for access token
6. Creates server session with Hytale API
7. Returns identity + session tokens
8. Tokens injected into game-server container environment

## License

MIT
