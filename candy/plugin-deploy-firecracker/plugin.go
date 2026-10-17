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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opencharly/sdk"
	pb "github.com/opencharly/spec/proto"
	"github.com/opencharly/spec/spec"
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
// stablePluginsPath and pluginsEnvVar are the delivery contract, and they are the SAME two values
// sdk/deploykit's CompileShellHookStep uses for a Containerfile venue (StablePluginsPath and
// PluginsEnvVar there). They are spelled a second time here because this plugin is an out-of-tree
// module; see opencharly/sdk#399 for the debt and its sweep.
const (
	stablePluginsPath = "/tmp/charly-plugins"
	pluginsEnvVar     = "CHARLY_PLUGIN_DIR"
)

// DeclaredStepContract implements spec.StepContractCarrier for the `step:plugin-tree` capability.
//
// THIS METHOD IS WHY THE STEP CAN DISPATCH AT ALL. A class:step capability is only routed to this
// plugin's OpExecute when the provider also carries a contract - the host's construct path
// (charly/charly/host_build_construct_step.go) resolves the word, requires a StepContractCarrier,
// and otherwise hands the step to the host walk, which refuses it: "RunHostStep: OpStep verb is
// not act-capable". Measured on a live bed before this method existed.
//
// The declared values are the deploy-acting triple a step of this kind needs, and Emits is false
// because there is no build-time fragment: a Containerfile venue gets its plugin hook from its own
// build (install_build.go's CompileShellHookStep), and this step exists for the venues that have no
// build step at all.
//
// sdk.StepContract is an ALIAS for spec.StepContract (sdk/schema.go), so this needs no new import.
func (provider) DeclaredStepContract() (sdk.StepContract, bool) {
	return sdk.StepContract{
		Scope: spec.ScopeSystem,
		Venue: spec.VenueHostNative,
		Gate:  spec.GateNone,
		Emits: false,
	}, true
}

func providedCapabilities() []sdk.ProvidedCapability {
	return []sdk.ProvidedCapability{
		{Class: "deploy", Word: "firecracker", InputDef: "", Lifecycle: true},
		// AND the venue-side prerequisite that deploy target needs. `charly firecracker` is an
		// out-of-process command plugin, so a plugin-backed plan step needs a plugin TREE at the
		// venue - and the only delivery mechanism that exists writes the variable into a
		// Containerfile at image-build time, which a `kind: cloud_image` venue does not have.
		// This step fills StablePluginsPath over the venue executor's file-copy and only then
		// exports CHARLY_PLUGIN_DIR. No input: there is one tree, one path, one variable and one
		// order - but WHICH plugins is not something a step can infer, so the consumer names them.
		// StepContract is what makes this capability ROUTEABLE, not merely visible: a class:step
		// capability is only handed to this plugin's OpExecute when the host finds the contract
		// HERE, on the advertised capability. A method on the provider struct does not travel -
		// this plugin is out-of-process and the host sees only what Describe replies with, which
		// BuildCapabilitiesWithRequires builds from exactly this slice. Measured: with the
		// contract absent the deploy refuses the step with "RunHostStep: OpStep verb is not
		// act-capable".
		{Class: "step", Word: "plugin-tree", InputDef: "#PluginTreeInput", StepContract: &spec.StepContract{
			Scope: spec.ScopeSystem, Venue: spec.VenueHostNative, Gate: spec.GateNone, Emits: false,
		}},
	}
}

type provider struct{ pb.UnimplementedProviderServer }

// Invoke lands the deployment the host hands over. The host decodes the request into a
// venue plus the plan views before calling, so the provider's input is that envelope and
// its output is a DeployReply — for this substrate the boot itself is driven through the
// CLI firecracker plugin, and its output is the deployment's evidence.
func (provider) Invoke(ctx context.Context, req *pb.InvokeRequest) (*pb.InvokeReply, error) {
	// This plugin serves TWO classes, so the class is the dispatch - the pattern
	// plugin-firecracker's own provider uses for its command and verb classes. The deploy
	// target is the default because it was the only class until plugin-tree joined it.
	if req.GetClass() == "step" {
		return invokePluginTreeStep(ctx, req)
	}
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

// invokePluginTreeStep is the `step:plugin-tree` half: it delivers the plugin TREE that a
// plugin-backed plan step needs at a venue that has no image-build step.
//
// WHY IT EXISTS. `charly firecracker` is an out-of-process command plugin, so a plan step that
// invokes it needs a plugin tree AT THE VENUE, in the shell the step runs in. The one delivery
// mechanism that existed writes `CHARLY_PLUGIN_DIR` into a Containerfile at image-build time
// (sdk/deploykit's CompileShellHookStep), and a `kind: cloud_image` venue is not built from one -
// measured: such a guest has no `CHARLY_PLUGIN_DIR` and no `/tmp/charly-plugins` while its
// `charly` binary is present and working. This step is the missing writer.
//
// FILL FIRST, EXPORT SECOND, and export only if the fill succeeded: install_build.go's own
// comment states the rule this follows - "pointing the loader at a path no deploy filled would
// claim a capability the deployment does not have".
//
// THE SOURCE IS THIS PLUGIN'S OWN DIRECTORY. charly's loader searches a directory of plugin
// executables (bakedPluginDir, `/usr/lib/charly/plugins`), and `CHARLY_PLUGIN_DIR` PREPENDS a
// directory ahead of it. A plugin loaded from such a directory therefore already sits beside the
// tree it must deliver - so this reads its own directory rather than inventing a cache path.
func invokePluginTreeStep(ctx context.Context, req *pb.InvokeRequest) (*pb.InvokeReply, error) {
	var in struct {
		Plugins []string `json:"plugins"`
	}
	if raw := req.GetParamsJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("plugin-tree: decode input: %w", err)
		}
	}
	if len(in.Plugins) == 0 {
		return nil, fmt.Errorf("plugin-tree: no plugins named; the consumer must say which tree to deliver")
	}

	store, err := pluginStoreDir()
	if err != nil {
		return nil, fmt.Errorf("plugin-tree: locate the plugin store: %w", err)
	}

	if err := os.MkdirAll(stablePluginsPath, 0o755); err != nil {
		return nil, fmt.Errorf("plugin-tree: create %s: %w", stablePluginsPath, err)
	}

	// FILL FIRST. Every name must resolve, to exactly one built plugin, before anything is written
	// and well before anything is exported.
	type pair struct{ src, dest string }
	var planned []pair
	for _, name := range in.Plugins {
		// The destination is the name the LOADER searches. charly's core states the rule
		// (plugin_loader.go: "bakedPluginFileName is the filename a baked plugin binary takes under
		// bakedPluginDir. It keys by the plugin candy's LEAF name"), and the leaf is what the
		// consumer names here, so Base is the whole derivation. sdk/buildcache.SafePluginBinName
		// applies the same rule to a full candy KEY that may carry slashes and colons - a leaf
		// cannot, so it is not needed, and importing it would pin this plugin to a newer sdk than
		// its go.mod carries for a transformation that is the identity on a leaf.
		leaf := filepath.Base(name)
		prefixed, err := filepath.Glob(filepath.Join(store, leaf+"-*"))
		if err != nil {
			return nil, fmt.Errorf("plugin-tree: search for %s: %w", leaf, err)
		}
		// Each built plugin is a TRIPLE in the store - `<leaf>-<key>`, `.lock` and `.stamp` -
		// and more than one build of the same plugin can be present at once. Measured: six
		// entries for one plugin, two of them real binaries. So the search takes the NEWEST
		// binary and ignores the sidecars: the build the project resolved most recently is the
		// one it is about to use, and a sidecar is never a plugin.
		var matches []string
		for _, m := range prefixed {
			if strings.HasSuffix(m, ".lock") || strings.HasSuffix(m, ".stamp") {
				continue
			}
			if fi, statErr := os.Stat(m); statErr != nil || !fi.Mode().IsRegular() {
				continue
			}
			matches = append(matches, m)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("plugin-tree: %s: no built plugin in %s - refusing to deliver a tree without it",
				leaf, store)
		}
		sort.Slice(matches, func(a, b int) bool {
			fa, ea := os.Stat(matches[a])
			fb, eb := os.Stat(matches[b])
			if ea != nil || eb != nil {
				return matches[a] > matches[b]
			}
			return fa.ModTime().After(fb.ModTime())
		})
		planned = append(planned, pair{src: matches[0], dest: filepath.Join(stablePluginsPath, leaf)})
	}

	for _, p := range planned {
		data, err := os.ReadFile(p.src)
		if err != nil {
			return nil, fmt.Errorf("plugin-tree: read %s: %w", p.src, err)
		}
		if err := os.WriteFile(p.dest, data, 0o755); err != nil {
			return nil, fmt.Errorf("plugin-tree: write %s: %w", p.dest, err)
		}
	}

	// EXPORT SECOND, and only now. install_build.go states the rule this follows: naming a path no
	// deploy filled would claim a capability the deployment does not have.
	//
	// WHERE, AND WHY NOT /etc. Measured: writing /etc/profile.d/charly-plugins.sh fails with
	// "permission denied" - this provider runs as the DEPLOYING USER, not as root, and a plugin
	// that needs root to state a capability is a plugin that cannot state it on the hosts that
	// matter. So the hook goes to the user's own shell startup files, which are writable by
	// definition, and the tree stays where the loader searches. Whether a LATER plan step's shell
	// reads these files is the open question the consumer bed answers; it is not answered here.
	hook := fmt.Sprintf("export %s=%s\n", pluginsEnvVar, stablePluginsPath)
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("plugin-tree: locate the home directory for the hook: %w", err)
	}
	wrote := 0
	for _, rc := range []string{".profile", ".bashrc"} {
		path := filepath.Join(home, rc)
		// APPEND, never truncate: these are the operator's own shell startup files and
		// os.WriteFile would replace every line of them. Read first, then append only if the
		// exact line is not already there, so repeated deploys do not stack copies.
		existing, readErr := os.ReadFile(path)
		if readErr != nil && !os.IsNotExist(readErr) {
			continue
		}
		if strings.Contains(string(existing), hook) {
			wrote++
			continue
		}
		f, openErr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if openErr != nil {
			continue
		}
		_, writeErr := f.WriteString(hook)
		f.Close()
		if writeErr == nil {
			wrote++
		}
	}
	if wrote == 0 {
		return nil, fmt.Errorf("plugin-tree: delivered %d plugin(s) to %s but could not write any shell hook in %s",
			len(planned), stablePluginsPath, home)
	}

	payload, _ := json.Marshal(map[string]any{
		"delivered": len(planned),
		"tree":      stablePluginsPath,
		"env":       pluginsEnvVar,
		"store":     store,
	})
	return &pb.InvokeReply{ResultJson: payload}, nil
}

// pluginStoreDir is where the deploying charly keeps the plugins it has built. A built plugin is
// stored as `<leaf>-<key>` - the leaf under the SDK's own flattening plus the build cache's key -
// which is why the destination drops the suffix and the source carries it.
func pluginStoreDir() (string, error) {
	if d := os.Getenv("CHARLY_PLUGIN_STORE"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "charly", "plugins"), nil
}
