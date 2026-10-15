// The `deploy:firecracker` deploy-target provider: a THIN DRIVER over the command
// candy/plugin-firecracker already serves.
//
// This plugin deliberately does not re-implement the boot. plugin-firecracker's CLI owns
// `charly firecracker launch`, which starts the VMM, writes the whole lease into it over
// the API and starts the guest; a deploy target's job is to land that deployment on a
// venue, not to duplicate the VMM path. Serving the class here rather than claiming it in
// plugin-firecracker is what opencharly/charly#916's withdrawal settled: a registry word
// resolves through plugins_refs_generated.go to a deploy-target provider, and a word that
// resolves to nothing is worse than an absent word.
package firecracker

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/opencharly/sdk"
	pb "github.com/opencharly/spec/proto"
)

//go:embed schema/*.cue
var schemaFS embed.FS

// calver is this plugin's own version. It is a constant the plugin owns, not a value read
// from the host: the capability set is built from this schema, so the version and the
// schema must travel together.
const calver = "2026.288.0001"

// NewProvider returns the deploy provider.
func NewProvider() pb.ProviderServer { return &provider{} }

// NewMeta advertises deploy:firecracker with this plugin's own schema. Lifecycle is true:
// the kernel routes a `target: firecracker` deploy here and this provider owns the venue's
// lifecycle on the host side, as the pod and vm deploy targets do.
func NewMeta() pb.PluginMetaServer { return sdk.NewMeta(calver, providedCapabilities(), schemaFS) }

// providedCapabilities is the ONE source of truth for what this plugin serves. NewMeta
// advertises it and declared_words_test.go reads it, so the capability list cannot drift
// from the declaration the candy makes: one is derived from the other by construction.
func providedCapabilities() []sdk.ProvidedCapability {
	return []sdk.ProvidedCapability{
		{Class: "deploy", Word: "firecracker", InputDef: "", Lifecycle: true},
	}
}

type provider struct{ pb.UnimplementedProviderServer }

// Invoke lands the deployment the host hands over. The host decodes the request into a
// venue plus the plan views before calling, so the provider's input is that envelope and
// its output is a DeployReply — for this substrate the boot itself is driven through the
// CLI firecracker plugin, and its output is the deployment's evidence.
func (provider) Invoke(ctx context.Context, req *pb.InvokeRequest) (*pb.InvokeReply, error) {
	venue, err := sdk.DecodeDeployVenue(req.GetEnvJson())
	if err != nil {
		return nil, fmt.Errorf("plugin-deploy-firecracker: decode venue: %w", err)
	}

	// The substrate body is this deploy target's OWN authored config, typed by the schema
	// this plugin embeds (#DeployFirecrackerPlugin). It is read through the typed shape rather
	// than by string keys, so every field this provider USES is named by the schema rather than
	// reached positionally - and a MISSPELLED tag is caught, because the field it should have
	// filled stays empty (that is exactly the `rootDrive` failure recorded below).
	//
	// It does NOT reject a field this provider does not know. The served schema closes with
	// `...` (schema/firecracker.cue), so an unknown authored field is schema-LEGAL, and plain
	// decoding silently ignores it. That is the CORRECT behaviour: the schema, not this
	// provider, is the authority on what an authored body may carry. An earlier version of
	// this comment claimed the opposite and was wrong.
	// The tags MUST match the served schema field-for-field: the schema declares
	// `rootDrive` (camelCase), so a tag of `root_drive` left RootDrive empty and the
	// provider refused every authored deploy. SDD makes the schema the source of truth;
	// the Go shape is derived from it, never the other way round.
	var authored struct {
		Kernel    string `json:"kernel"`
		RootDrive string `json:"rootDrive"`
	}
	if len(venue.Substrate) > 0 {
		if err := json.Unmarshal(venue.Substrate, &authored); err != nil {
			return nil, fmt.Errorf("plugin-deploy-firecracker: decode the authored substrate for %q: %w", venue.DeployName, err)
		}
	}

	// The VMM boots a kernel and a root drive, and nothing else: a subprocess without both
	// is not a deployment, so they are required rather than defaulted.
	if authored.Kernel == "" || authored.RootDrive == "" {
		return nil, fmt.Errorf("plugin-deploy-firecracker: deploy %q needs kernel and rootDrive (the VMM boots both)", venue.DeployName)
	}

	cmd := exec.CommandContext(ctx, "charly", "firecracker", "launch",
		"--kernel", authored.Kernel, "--rootfs", authored.RootDrive)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("plugin-deploy-firecracker: launch %q: %w (output: %s)", venue.DeployName, err, out)
	}

	// ResultJson is the wire field the host reads: the launch's output IS the deployment's
	// evidence, so it is what this provider returns.
	payload, err := json.Marshal(map[string]string{"launch_output": string(out)})
	if err != nil {
		return nil, fmt.Errorf("plugin-deploy-firecracker: marshal the reply: %w", err)
	}

	return &pb.InvokeReply{ResultJson: payload}, nil
}
