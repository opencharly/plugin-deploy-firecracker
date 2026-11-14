package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencharly/sdk"
	pb "github.com/opencharly/spec/proto"
	"github.com/opencharly/spec/spec"
	"google.golang.org/grpc"
)

// venueRecorder is the FAKE venue this test's step delivers to. It embeds the generated
// ExecutorServiceClient interface — which is how the SDK's own tests satisfy the full shape
// without a socket or a server — and overrides PutFile, the ONLY venue operation this step
// is allowed to perform, to RECORD what was delivered.
//
// Every other venue method falls through to the embedded nil interface and panics. That is
// deliberate: an unexpected venue call from this handler is a defect, and a test that
// silently tolerated one would be the vacuous gate this repository has already paid for.
type venueRecorder struct {
	pb.ExecutorServiceClient
	delivered []delivery
}

type delivery struct {
	path      string
	content   []byte
	mode      uint32
	ownerRoot bool
}

func (v *venueRecorder) PutFile(_ context.Context, in *pb.PutFileRequest, _ ...grpc.CallOption) (*pb.PutFileReply, error) {
	v.delivered = append(v.delivered, delivery{
		path:      in.GetPath(),
		content:   in.GetContent(),
		mode:      in.GetMode(),
		ownerRoot: in.GetOwnerRoot(),
	})
	return &pb.PutFileReply{}, nil
}

// GetFile is the READ-BACK the handler now performs: a step that writes two files must be able to
// say whether both arrived, so the fake venue serves back exactly what it was given. A path the
// venue never received is an error, which is what a partial delivery must look like.
func (v *venueRecorder) GetFile(_ context.Context, in *pb.GetFileRequest, _ ...grpc.CallOption) (*pb.GetFileReply, error) {
	for _, d := range v.delivered {
		if d.path == in.GetPath() {
			return &pb.GetFileReply{Content: d.content}, nil
		}
	}
	return nil, fmt.Errorf("venue has no file at %s", in.GetPath())
}

// servedBinary writes ONE served plugin binary where the deploying charly's connect put it — a
// path this test OWNS, standing in for `plugin-build/views/<leaf>-<key>` — and returns the path.
// The step must read the bytes from HERE; nothing is written into any store, because the store is
// no longer part of this step's contract (opencharly/charly#1131).
func servedBinary(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin-firecracker-3f2a1c9d")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatalf("write the served binary: %v", err)
	}
	return path
}

// stepInput renders the step input the ENGINE sends: the authored plugin names plus the connected
// plugin set it resolved (spec.ConnectedPluginsInputKey). Passing entries explicitly is what makes
// each arm below a statement about the handler rather than about a directory's contents.
func stepInput(t *testing.T, plugins []string, connected []map[string]any) []byte {
	t.Helper()
	in := map[string]any{"plugins": plugins}
	if connected != nil {
		in["plugin_paths"] = connected
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal the step input: %v", err)
	}
	return raw
}

// TestPluginTreeStepDeliversToTheVenue is the in-repo control the A2/R10 verdict asked for, and it
// is the one that distinguishes the handler rather than the bed:
//
//	"a fake/`host: local` executor asserting `PutFile` receives the planned `dest`+`content`
//	 and failing on the old `os.WriteFile`"
//
// It FAILS without this change, twice over and independently:
//
//  1. the venue must RECEIVE the tree. The old handler wrote through os.WriteFile and never
//     called PutFile at all, so `delivered` is empty and the first assertion below fires;
//  2. the bytes must arrive intact, binary-safe, at the leaf name the LOADER searches, with
//     the mode the delivered file needs — an assertion a handler that delivered something
//     else (the hash-suffixed store entry, a truncated read, mode 0644) would also fail.
//
// AND it fails without the ENGINE HALF: the source is a path this test hands over in
// `plugin_paths`, so a handler that still scanned a directory would deliver nothing here.
// TestPluginTreeStepFailsWhenTheVenueTakesOnlyTheBinary is the PARTIAL-DELIVERY arm, and the reason
// the handler reads back what it wrote: a venue that accepts the binary and silently drops the
// manifest must make the STEP fail, by name - not leave a tree whose failure only a later assertion
// discovers. That shape is what produced an intermittent gate (one arm of this bed observed the
// binary present and the manifest missing).
func TestPluginTreeStepFailsWhenTheVenueTakesOnlyTheBinary(t *testing.T) {
	payload := []byte("\x7fELF\x02\x01\x01\x00 plugin-firecracker built binary \xff\xfe\x00")
	served := servedBinary(t, payload)

	// Drops every `.providers` file after "successfully" writing it.
	rec := &partialVenue{}
	ctx := sdk.ContextWithExecutor(context.Background(), sdk.NewInProcExecutor(rec))

	_, err := invokePluginTreeStep(ctx, &pb.InvokeRequest{
		ParamsJson: stepInput(t, []string{"plugin-firecracker"}, []map[string]any{{
			"key":       "plugin-firecracker",
			"leaf":      "plugin-firecracker",
			"view_path": served,
			"providers": []any{"verb:firecracker", "command:firecracker"},
		}}),
	})
	if err == nil {
		t.Fatal("the step accepted a venue that dropped the manifest - a tree that serves no word would have been reported as delivered")
	}
	if !strings.Contains(err.Error(), "plugin-firecracker") {
		t.Fatalf("the failure must NAME the plugin whose delivery was incomplete: %v", err)
	}
}

// partialVenue keeps the binary and loses the manifest.
type partialVenue struct{ venueRecorder }

func (v *partialVenue) PutFile(ctx context.Context, in *pb.PutFileRequest, opts ...grpc.CallOption) (*pb.PutFileReply, error) {
	if strings.HasSuffix(in.GetPath(), spec.ManifestSuffix) {
		return &pb.PutFileReply{}, nil // reported success, stored nothing
	}
	return v.venueRecorder.PutFile(ctx, in, opts...)
}

func TestPluginTreeStepDeliversToTheVenue(t *testing.T) {
	// Binary-safe content on purpose: a plugin binary is not text, and a delivery that
	// mangled bytes would still "succeed" against a string-shaped assertion.
	payload := []byte("\x7fELF\x02\x01\x01\x00 plugin-firecracker built binary \xff\xfe\x00")
	served := servedBinary(t, payload)

	rec := &venueRecorder{}
	ctx := sdk.ContextWithExecutor(context.Background(), sdk.NewInProcExecutor(rec))

	reply, err := invokePluginTreeStep(ctx, &pb.InvokeRequest{
		ParamsJson: stepInput(t, []string{"plugin-firecracker"}, []map[string]any{{
			"key":       "plugin-firecracker",
			"leaf":      "plugin-firecracker",
			"view_path": served,
			"src_dir":   "/src/plugin-firecracker",
			// The WORDS the engine connected it with: a tree cannot serve a command/verb without
			// the manifest built from them (opencharly/charly#1131).
			"providers": []any{"verb:firecracker", "command:firecracker"},
		}}),
	})
	if err != nil {
		t.Fatalf("the step refused a set that holds the plugin: %v", err)
	}

	// TWO files: the binary AND its manifest. A tree that carries only binaries answers every
	// command/verb invocation with charly's usage text, which is the defect this arm exists for.
	if len(rec.delivered) != 2 {
		t.Fatalf("the venue received %d file(s), want exactly 2 (binary + manifest): the tree must be delivered TO THE VENUE through PutFile", len(rec.delivered))
	}

	got := rec.delivered[0]
	// The destination is the name the LOADER opens — the LEAF, not the view's hash-suffixed
	// name — under the path the loader searches first.
	wantPath := filepath.Join(stablePluginsPath, "plugin-firecracker")
	if got.path != wantPath {
		t.Fatalf("delivered to %q, want %q (the loader opens the leaf name under this path)", got.path, wantPath)
	}
	if !bytes.Equal(got.content, payload) {
		t.Fatalf("delivered content = %q, want the served binary's bytes %q", got.content, payload)
	}
	if got.mode != 0o755 {
		t.Fatalf("delivered mode = %#o, want 0o755 (a plugin must arrive executable)", got.mode)
	}
	if got.ownerRoot {
		t.Fatal("delivered as root:root; this tree belongs to the venue's charly user, not to root")
	}

	man := rec.delivered[1]
	if wantManifestPath := wantPath + spec.ManifestSuffix; man.path != wantManifestPath {
		t.Fatalf("manifest delivered to %q, want %q", man.path, wantManifestPath)
	}
	// BYTE-FOR-BYTE, the one format: one `class:word` per line, newline-terminated, mode 0644.
	if want := "verb:firecracker\ncommand:firecracker\n"; string(man.content) != want {
		t.Fatalf("manifest content = %q, want %q", man.content, want)
	}
	if man.mode != 0o644 {
		t.Fatalf("manifest mode = %#o, want 0o644 (data, not a program)", man.mode)
	}

	var out map[string]any
	if err := json.Unmarshal(reply.GetResultJson(), &out); err != nil {
		t.Fatalf("ResultJson is not the documented payload: %v", err)
	}
	if out["delivered"] != float64(1) || out["tree"] != stablePluginsPath {
		t.Fatalf("ResultJson reports %v/%v, want 1 delivered under %q", out["delivered"], out["tree"], stablePluginsPath)
	}
}

// TestPluginTreeStepRefusesRatherThanGuesses covers the conditions under which this step must
// deliver NOTHING, and the difference between them is what a reader needs at a failure line:
//
//   - a name the engine connected no plugin for (a wrong plugin closure in the deployment);
//   - no connected set at all (a deploying charly too old to supply it) — the hard cutover, which
//     must be LOUD rather than silently fall back to a directory scan;
//   - two served binaries for one leaf (the ambiguity a newest-wins scan used to resolve by
//     modification time — exactly the guess #1131 rejects).
func TestPluginTreeStepRefusesRatherThanGuesses(t *testing.T) {
	served := servedBinary(t, []byte("built"))
	rec := &venueRecorder{}
	ctx := sdk.ContextWithExecutor(context.Background(), sdk.NewInProcExecutor(rec))

	absent := []map[string]any{{
		"key": "plugin-deploy-firecracker", "leaf": "plugin-deploy-firecracker", "view_path": served,
	}}

	for _, tc := range []struct {
		name    string
		input   []byte
		wantSub string
	}{
		{
			name:    "a name the engine did not connect",
			input:   stepInput(t, []string{"plugin-absent"}, absent),
			wantSub: "connected no plugin with this leaf",
		},
		{
			name:    "no connected set at all",
			input:   stepInput(t, []string{"plugin-firecracker"}, nil),
			wantSub: "supplied no connected plugin set",
		},
		{
			name: "a connected entry with no provider words",
			input: stepInput(t, []string{"plugin-firecracker"}, []map[string]any{
				{"key": "plugin-firecracker", "leaf": "plugin-firecracker", "view_path": served},
			}),
			wantSub: "connected it with NO provider words",
		},
		{
			name: "two served binaries for one leaf",
			input: stepInput(t, []string{"plugin-firecracker"}, []map[string]any{
				{"key": "plugin-firecracker", "leaf": "plugin-firecracker", "view_path": served},
				{"key": "github.com/opencharly/plugin-firecracker/candy/plugin-firecracker", "leaf": "plugin-firecracker", "view_path": served + "-other"},
			}),
			wantSub: "refusing to choose between them",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec.delivered = nil
			_, err := invokePluginTreeStep(ctx, &pb.InvokeRequest{ParamsJson: tc.input})
			if err == nil {
				t.Fatalf("the step accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("refusal does not name the reason %q: %v", tc.wantSub, err)
			}
			if len(rec.delivered) != 0 {
				t.Fatalf("the refused step still delivered %d file(s) to the venue", len(rec.delivered))
			}
		})
	}
}
