package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencharly/sdk"
	pb "github.com/opencharly/spec/proto"
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

// seedStore writes ONE built plugin into a throwaway plugin store, in the shape the
// deploying charly leaves behind: `<leaf>-<key>` beside its `.lock` and `.stamp` sidecars.
// The store location is the documented override, so nothing here touches the real cache.
func seedStore(t *testing.T, leaf, key string, content []byte) {
	t.Helper()
	store := t.TempDir()
	built := filepath.Join(store, leaf+"-"+key)
	if err := os.WriteFile(built, content, 0o755); err != nil {
		t.Fatalf("seed the built plugin %s: %v", built, err)
	}
	for _, sidecar := range []string{".lock", ".stamp"} {
		if err := os.WriteFile(built+sidecar, nil, 0o600); err != nil {
			t.Fatalf("seed the sidecar %s%s: %v", built, sidecar, err)
		}
	}
	t.Setenv("CHARLY_PLUGIN_STORE", store)
}

// TestPluginTreeStepDeliversToTheVenue is the in-repo control the A2/R10 verdict asked for,
// and it is the one that distinguishes the handler rather than the bed:
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
// The last case is the fill-first contract: a name with no built plugin in the store must be
// refused BEFORE anything is delivered, because a tree delivered without one of its members
// is the fake fix this step exists to avoid.
func TestPluginTreeStepDeliversToTheVenue(t *testing.T) {
	// Binary-safe content on purpose: a plugin binary is not text, and a delivery that
	// mangled bytes would still "succeed" against a string-shaped assertion.
	payload := []byte("\x7fELF\x02\x01\x01\x00 plugin-firecracker built binary \xff\xfe\x00")
	seedStore(t, "plugin-firecracker", "3f2a1c9d", payload)

	rec := &venueRecorder{}
	ctx := sdk.ContextWithExecutor(context.Background(), sdk.NewInProcExecutor(rec))

	reply, err := invokePluginTreeStep(ctx, &pb.InvokeRequest{
		ParamsJson: []byte(`{"plugins":["plugin-firecracker"]}`),
	})
	if err != nil {
		t.Fatalf("the step refused a store that holds the plugin: %v", err)
	}

	if len(rec.delivered) != 1 {
		t.Fatalf("the venue received %d file(s), want exactly 1: the tree must be delivered TO THE VENUE through PutFile, not written to this process's filesystem", len(rec.delivered))
	}

	got := rec.delivered[0]
	// The destination is the name the LOADER opens — the LEAF, not the store's hash-suffixed
	// entry — under the path the loader searches first.
	wantPath := filepath.Join(stablePluginsPath, "plugin-firecracker")
	if got.path != wantPath {
		t.Fatalf("delivered to %q, want %q (the loader opens the leaf name under this path)", got.path, wantPath)
	}
	if !bytes.Equal(got.content, payload) {
		t.Fatalf("delivered content = %q, want the built plugin's bytes %q", got.content, payload)
	}
	if got.mode != 0o755 {
		t.Fatalf("delivered mode = %#o, want 0o755 (a plugin must arrive executable)", got.mode)
	}
	if got.ownerRoot {
		t.Fatal("delivered as root:root; this tree belongs to the venue's charly user, not to root")
	}

	var out map[string]any
	if err := json.Unmarshal(reply.GetResultJson(), &out); err != nil {
		t.Fatalf("ResultJson is not the documented payload: %v", err)
	}
	if out["delivered"] != float64(1) || out["tree"] != stablePluginsPath {
		t.Fatalf("ResultJson reports %v/%v, want 1 delivered under %q", out["delivered"], out["tree"], stablePluginsPath)
	}

	// FILL FIRST: an unresolvable name is refused, and nothing is delivered on the way to the
	// refusal — a partially delivered tree is worse than none, because the loader finds it.
	rec.delivered = nil
	if _, err := invokePluginTreeStep(ctx, &pb.InvokeRequest{
		ParamsJson: []byte(`{"plugins":["plugin-absent"]}`),
	}); err == nil {
		t.Fatal("the step accepted a plugin name with no built plugin in the store")
	}
	if len(rec.delivered) != 0 {
		t.Fatalf("the refused step still delivered %d file(s) to the venue", len(rec.delivered))
	}
}
