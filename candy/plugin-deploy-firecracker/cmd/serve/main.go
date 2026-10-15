// Command serve is the OUT-OF-PROCESS entrypoint for this deploy plugin: a thin shim
// serving the importable provider over go-plugin gRPC via sdk.Serve. The SAME
// NewProvider()/NewMeta() compile INTO charly in-process when listed in compiled_plugins;
// this binary is host-built and connected only when they are not, so placement is
// invisible above the registry.
package main

import (
	deployfirecracker "github.com/opencharly/plugin-deploy-firecracker/candy/plugin-deploy-firecracker"
	"github.com/opencharly/sdk"
)

func main() { sdk.Serve(deployfirecracker.NewProvider(), deployfirecracker.NewMeta()) }
