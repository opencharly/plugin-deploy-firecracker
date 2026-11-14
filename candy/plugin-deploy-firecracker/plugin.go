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
	"bytes"
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
// stablePluginsPath is where this step delivers, and it is the SAME path sdk/deploykit uses for a
// Containerfile venue (StablePluginsPath there). It is spelled a second time here because this
// plugin is an out-of-tree module; see opencharly/sdk#399 for the debt and its sweep.
//
// The variable that points a loader at it is NOT set here: that is the product's to do, and the
// reason is filed at opencharly/charly#977.
const stablePluginsPath = "/tmp/charly-plugins"

func providedCapabilities() []sdk.ProvidedCapability {
	return []sdk.ProvidedCapability{
		{Class: "deploy", Word: "firecracker", InputDef: "", Lifecycle: true},
		// AND the venue-side prerequisite that deploy target needs. `charly firecracker` is an
		// out-of-process command plugin, so a plugin-backed plan step needs a plugin TREE at the
		// venue - and the only delivery mechanism that exists writes the variable into a
		// Containerfile at image-build time, which a `kind: cloud_image` venue does not have.
		// This step FILLS StablePluginsPath and does not export it: which layer owns a step's
		// environment is the product's call (opencharly/charly#977). The input is the plugin
		// NAMES, because WHICH plugins a deployment needs is not something a step can infer - and
		// the SOURCES come from the engine's own resolution (charly#1131), not from a scan.
		// StepContract is what makes this capability ROUTEABLE, not merely visible: a class:step
		// capability is only handed to this plugin's OpExecute when the host finds the contract
		// HERE, on the advertised capability. A method on the provider struct does not travel -
		// this plugin is out-of-process and the host sees only what Describe replies with, which
		// BuildCapabilitiesWithRequires builds from exactly this slice. Measured: with the
		// contract absent the deploy refuses the step with "RunHostStep: OpStep verb is not
		// act-capable".
		// PluginPaths is the opt-in that makes the step RESOLVABLE without a guess: the host hands
		// it the connected plugin set it already resolved (opencharly/charly#1131), so the sources
		// arrive as paths instead of being re-derived from a directory layout this plugin does not
		// own. It must be declared HERE — the host acts on the DECLARED capability, and a flag that
		// only exists in this repo's Go would never reach it.
		{Class: "step", Word: "plugin-tree", InputDef: "#PluginTreeInput", StepContract: &spec.StepContract{
			Scope: spec.ScopeSystem, Venue: spec.VenueHostNative, Gate: spec.GateNone, Emits: false,
			PluginPaths: true,
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
// mechanism that existed writes `CHARLY_PLUGIN_DIR` into a Containerfile at image-build time (sdk/deploykit's
// CompileShellHookStep), and a `kind: cloud_image` venue is not built from one - measured: such a
// guest has no `CHARLY_PLUGIN_DIR` and no `/tmp/charly-plugins` while its `charly` binary is
// present and working. This step is the missing writer.
//
// WHERE THE BINARIES COME FROM - the engine, not a directory scan. opencharly/charly#1131: the
// deploying charly ALREADY knows which built binary serves which plugin - it resolved that when it
// connected the plugin, and prints it (`served from <path> (build <sha>); source <dir>`). A step
// that has only a plugin NAME used to re-derive it by listing a store and taking the newest match,
// which is a guess: a stale build wins whenever it was written last, the store's layout is the
// engine's private business, and on a cloud_image venue the store is not even on the machine that
// runs the step. So the engine now hands the connected set over in this step's OWN input
// (`plugin_paths`, declared in #PluginTreeInput), and this handler resolves each authored name
// against PATHS IT WAS TOLD.
//
// THREE refusal conditions, each with its own provenance:
//   - NO SET AT ALL (`plugin_paths` absent, or empty): NEW. The engine did not supply the
//     connected set, so this step cannot know what to deliver. It refuses and says so - there is
//     no fallback scan, because a fallback is how the guess comes back.
//   - A NAME THE ENGINE DID NOT CONNECT: the INHERITED fill-first refusal - the same intent as
//     the old "no built plugin in <store>" branch, with the engine's resolution as its source of
//     truth instead of a store listing. Its message names what the engine DID connect.
//   - AN AMBIGUOUS LEAF (two entries with the same leaf): NEW. It refuses instead of picking one
//     by modification time - the old store scan silently took the newest match, and that choice is
//     exactly the guess this change removes.
//
// All three keep the fill-first contract: deliver nothing rather than a tree that is missing a
// member or holds the wrong one.
//
// IT FILLS AND DOES NOT EXPORT. install_build.go states the rule the two halves obey - "pointing
// the loader at a path no deploy filled would claim a capability the deployment does not have" -
// and this step satisfies the first half of it. The second half, naming the path in the
// environment a later step runs in, belongs to the product: opencharly/charly#977.
func invokePluginTreeStep(ctx context.Context, req *pb.InvokeRequest) (*pb.InvokeReply, error) {
	var in struct {
		Plugins []string `json:"plugins"`
		// PluginPaths is the CONNECTED PLUGIN SET the engine resolved for this deployment, injected
		// under spec.ConnectedPluginsInputKey because this step's StepContract declares
		// PluginPaths. It is the engine's own wire type, not a copy: whichever side renames a field,
		// both sides move (opencharly/charly#1131).
		PluginPaths []spec.DeployConnectedPlugin `json:"plugin_paths"`
	}
	if raw := req.GetParamsJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("plugin-tree: decode input: %w", err)
		}
	}
	if len(in.Plugins) == 0 {
		return nil, fmt.Errorf("plugin-tree: no plugins named; the consumer must say which tree to deliver")
	}
	// NO SET, NO DELIVERY. An older deploying charly does not inject the key at all, and an empty
	// set means it resolved no plugins for this deployment; both are conditions this step cannot
	// paper over. Naming them separately is what makes the failure readable: an EMPTY set is a
	// deployment whose plugin closure is wrong, an ABSENT one is an engine that cannot tell us.
	if len(in.PluginPaths) == 0 {
		return nil, fmt.Errorf("plugin-tree: the deploying charly supplied no connected plugin set "+
			"(input key %q); this step resolves its sources from the engine's own resolution "+
			"(opencharly/charly#1131) and refuses to guess one", spec.ConnectedPluginsInputKey)
	}

	// FILL FIRST. Every name must resolve to exactly one served binary before anything is written.
	type pair struct {
		src, dest string
		providers []string
	}
	var planned []pair
	for _, name := range in.Plugins {
		// The destination is the name the LOADER searches: the plugin's LEAF. charly's core states
		// the rule (bakedPluginFileName keys a baked binary by the candy's leaf name), and the leaf
		// is what the consumer names here.
		leaf := filepath.Base(name)
		var matches []spec.DeployConnectedPlugin
		for _, p := range in.PluginPaths {
			if p.Leaf == leaf {
				matches = append(matches, p)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("plugin-tree: %s: the engine connected no plugin with this leaf "+
				"(connected: %s) - refusing to deliver a tree without it", leaf, connectedLeaves(in.PluginPaths))
		case 1:
			if matches[0].ViewPath == "" {
				return nil, fmt.Errorf("plugin-tree: %s: the connected entry names no served binary", leaf)
			}
			planned = append(planned, pair{
				src:       matches[0].ViewPath,
				dest:      filepath.Join(stablePluginsPath, leaf),
				providers: matches[0].Providers,
			})
		default:
			// Two served binaries for one leaf is a question this step may not answer by recency -
			// that heuristic is the defect #1131 exists to delete. Refuse and name both paths.
			return nil, fmt.Errorf("plugin-tree: %s: the engine connected %d different binaries for this "+
				"leaf (%s); refusing to choose between them", leaf, len(matches), connectedPaths(matches))
		}
	}

	// THE VENUE, NOT THIS PROCESS. ExecutorForInvoke resolves the host executor TRANSPORT-
	// INVISIBLY, which is the property this step needs on both placements: an IN-PROC
	// (compiled-in) plugin gets it from the context via ContextWithExecutor, and an
	// OUT-OF-PROCESS plugin falls back to the go-plugin broker id the host passed in THIS
	// request. Either way PutFile is a plain os.WriteFile on a host: local venue and scp-plus-
	// install into the guest on a vm: one.
	//
	// READING THE SOURCE STAYS LOCAL: the served binary lives on the machine that connected the
	// plugin, so the bytes are here even when the destination is not. The plugin-side PutFile takes
	// CONTENT, not a local path, so the read and the write live on different sides.
	ex, err := sdk.ExecutorForInvoke(ctx, req.GetExecutorBrokerId())
	if err != nil {
		return nil, fmt.Errorf("plugin-tree: resolve the venue executor: %w", err)
	}
	for _, p := range planned {
		// THE MANIFEST, BESIDE THE BINARY - NOT OPTIONAL. A tree whose binaries carry no
		// `<leaf>.providers` manifest cannot serve a bare command/verb WORD at all: the loader maps a
		// word to a file THROUGH that manifest, so the invocation answers with charly's USAGE text
		// while every file it needs is present. MEASURED 2026-11-13 on a disposable guest: the tree
		// was delivered, CHARLY_PLUGIN_DIR pointed at it, `charly firecracker --help` still printed
		// the usage text; adding the manifest made the same command resolve.
		//
		// The BYTES come from spec.PluginManifest - the ONE serializer the baked-image emitter writes
		// too - and the WORDS come from the engine's connected entry, so this step never re-derives
		// what the plugin serves.
		manifest := spec.PluginManifest(p.providers)
		if manifest == nil {
			return nil, fmt.Errorf("plugin-tree: %s: the engine connected it with NO provider words, so nothing could address the delivered binary; refusing to deliver a tree that cannot resolve", filepath.Base(p.dest))
		}
		data, err := os.ReadFile(p.src)
		if err != nil {
			return nil, fmt.Errorf("plugin-tree: read %s: %w", p.src, err)
		}
		if err := ex.PutFile(ctx, p.dest, data, 0o755, false); err != nil {
			return nil, fmt.Errorf("plugin-tree: deliver to %s: %w", p.dest, err)
		}
		if err := ex.PutFile(ctx, p.dest+spec.ManifestSuffix, manifest, spec.ManifestMode, false); err != nil {
			return nil, fmt.Errorf("plugin-tree: deliver the manifest to %s%s: %w", p.dest, spec.ManifestSuffix, err)
		}
	}

	// READ IT BACK. A step that writes two files and cannot tell whether BOTH arrived is how an
	// intermittent gate is born: an earlier arm of this bed observed the binary present and the
	// manifest not, and the check - not the step - had to discover it. Verifying here turns a
	// partial delivery into a step failure that names the file, at the venue, in the step's own
	// output, instead of a mystery a later assertion has to reconstruct.
	verified := make([]string, 0, 2*len(planned))
	for _, p := range planned {
		leaf := filepath.Base(p.dest)
		got, err := ex.GetFile(ctx, p.dest, false)
		if err != nil {
			return nil, fmt.Errorf("plugin-tree: %s: read back after delivery: %w", leaf, err)
		}
		if len(got) == 0 {
			return nil, fmt.Errorf("plugin-tree: %s: EMPTY after delivery - the venue did not take the binary", leaf)
		}
		manPath := p.dest + spec.ManifestSuffix
		gotMan, err := ex.GetFile(ctx, manPath, false)
		if err != nil {
			return nil, fmt.Errorf("plugin-tree: %s: read back the manifest after delivery: %w", leaf, err)
		}
		if want := spec.PluginManifest(p.providers); !bytes.Equal(gotMan, want) {
			return nil, fmt.Errorf("plugin-tree: %s: the manifest at the venue is %q, want %q - the tree would serve no word", leaf, gotMan, want)
		}
		verified = append(verified, p.dest, manPath)
	}

	// NO EXPORT, DELIBERATELY. The environment a deployment's step runs in is the PRODUCT's to
	// set, not a plugin's to inject into the operator's shell startup files - the first version of
	// this handler appended `export ...` to ~/.profile and ~/.bashrc, which is a "works in my shell
	// / not in my shell" shim and was rejected as R3. So this step has ONE job: put the plugins at
	// the path the loader searches, which it does and which the candy's check asserts.
	//
	// Pointing the loader at that path is opencharly/charly#977.

	payload, _ := json.Marshal(map[string]any{
		"delivered": len(planned),
		"tree":      stablePluginsPath,
		"sources":   len(in.PluginPaths),
		"verified":  verified,
	})
	return &pb.InvokeReply{ResultJson: payload}, nil
}

// connectedLeaves renders the connected set's leaves for a refusal message, so the reader can see
// WHAT the engine did resolve instead of only that their name was not among it.
func connectedLeaves(set []spec.DeployConnectedPlugin) string {
	leaves := make([]string, 0, len(set))
	for _, p := range set {
		leaves = append(leaves, p.Leaf)
	}
	sort.Strings(leaves)
	return strings.Join(leaves, ", ")
}

// connectedPaths renders the binary paths of an ambiguous leaf set, for the same reason.
func connectedPaths(matches []spec.DeployConnectedPlugin) string {
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		paths = append(paths, m.ViewPath)
	}
	return strings.Join(paths, ", ")
}
