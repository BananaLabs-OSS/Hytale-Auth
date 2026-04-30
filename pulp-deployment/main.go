package main

import (
	_ "github.com/BananaLabs-OSS/Pulp-ext-fs"
	_ "github.com/BananaLabs-OSS/Pulp-ext-http"

	"github.com/BananaLabs-OSS/Pulp/run"
)

func main() { run.Main() }
