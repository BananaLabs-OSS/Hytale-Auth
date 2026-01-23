# Hytale-Auth

Token service for Hytale server authentication.

From [BananaLabs OSS](https://github.com/bananalabs-oss).

## Overview

Hytale-Auth handles OAuth flow with Hytale's session service to generate server identity and session tokens. Used as a pre-start hook by Bananagine.

## Quick Start
```bash
# Requires refresh token file
go run ./main.go
```

Runs on `:3002`.

## API Reference

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/tokens` | Generate fresh server tokens |

**Response:**
```json
{
  "env": {
    "HYTALE_SERVER_IDENTITY_TOKEN": "eyJ...",
    "HYTALE_SERVER_SESSION_TOKEN": "eyJ..."
  }
}
```

## Token Flow

1. Bananagine calls `/tokens` as pre-start hook
2. Hytale-Auth uses stored refresh token
3. Exchanges for access token via OAuth
4. Creates server session with Hytale API
5. Returns identity + session tokens
6. Tokens injected into container environment

## Configuration

Requires a refresh token file in the working directory. See Hytale documentation for obtaining refresh tokens.

## License

MIT