module hytale-auth-deployment

go 1.25.6

require (
	github.com/BananaLabs-OSS/Pulp v0.0.0
	github.com/BananaLabs-OSS/Pulp-ext-fs v0.0.0
	github.com/BananaLabs-OSS/Pulp-ext-http v0.0.0
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/tetratelabs/wazero v1.11.0 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)

replace (
	github.com/BananaLabs-OSS/Pulp => ../../Pulp
	github.com/BananaLabs-OSS/Pulp-ext-fs => ../../Pulp-ext-fs
	github.com/BananaLabs-OSS/Pulp-ext-http => ../../Pulp-ext-http
)
