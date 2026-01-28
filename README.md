# Hytale-Auth

Token service for Hytale server authentication.

From [BananaLabs OSS](https://github.com/bananalabs-oss).

## Overview

Hytale-Auth handles OAuth flow with Hytale's session service to generate server identity and session tokens. Used as a pre-start hook by Bananagine.

## Quick Start
```bash
go run ./main.go
```

On first run, visit the displayed URL to authorize. Tokens are saved automatically.

## API Reference

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/` | Authorization status |
| `GET` | `/tokens` | Generate fresh server tokens |
| `GET` | `/health` | Health check |

**GET /tokens Response:**
```json
{
  "env": {
    "HYTALE_SERVER_IDENTITY_TOKEN": "eyJ...",
    "HYTALE_SERVER_SESSION_TOKEN": "eyJ..."
  }
}
```

## Configuration

Priority: CLI flags > environment variables > config file > defaults

| Setting | Flag | Env Var | Default |
|---------|------|---------|---------|
| HTTP port | `--port` | `PORT` | `3002` |
| Data directory | `--data-dir` | `DATA_DIR` | `.` |

**config.json (optional):**
```json
{
  "port": "3002",
  "data_dir": "/data"
}
```

**Examples:**
```bash
# Defaults
./hytale-auth

# CLI flags
./hytale-auth --port 8080 --data-dir ./auth-data

# Environment variables
PORT=8080 DATA_DIR=/data ./hytale-auth
```

## Docker
```yaml
hytale-auth:
  image: ghcr.io/bananalabs-oss/hytale-auth:latest
  ports:
    - "3002:3002"
  volumes:
    - ./hytale-auth-data:/data
  environment:
    - DATA_DIR=/data
```

First run: check logs for authorization URL, approve in browser. Restarts work automatically.



## Token Flow

1. On startup, verifies stored refresh token
2. If invalid/missing, starts device authorization flow
3. User approves via browser, profile UUID fetched automatically
4. Bananagine calls `/tokens` as pre-start hook
5. Exchanges refresh token for access token
6. Creates server session with Hytale API
7. Returns identity + session tokens
8. Tokens injected into container environment

## License

MIT