package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/opencharly/spec/proto"
)

// TestInvokeDrivesTheLaunchPath is the control the A2/R10 verdict asked for: it exercises
// Invoke's OWN changed path — decode the authored substrate, require both artefacts, compose
// the launch command, and marshal the reply — instead of only asserting that a declaration
// matches a class.
//
// It FAILS without the change in two independent ways, which is what makes it a control and
// not a restatement:
//
//  1. the substrate below is authored in the SERVED SCHEMA's vocabulary (`rootDrive`,
//     camelCase), which is the source of truth under SDD. With the old `json:"root_drive"`
//     tag RootDrive decoded empty, the provider refused with "needs kernel and rootDrive",
//     and the error branch below is taken;
//  2. the arguments the provider hands the launch command are read back from a stub, so a
//     provider that composed the wrong argv (or never spawned anything) is caught, not
//     assumed.
//
// The stub is a unit-level fake for the subprocess only. It does NOT prove the VMM boots:
// that needs a real venue, and the tier and the PR body say so.
func TestInvokeDrivesTheLaunchPath(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "charly")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\nprintf 'firecracker: launched\\n'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write the launch stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Authored the way a user writes it against the served schema: camelCase rootDrive.
	env := `{"deploy_name":"fc-test","substrate":{"kernel":"vmlinux-6.1.155","rootDrive":"rootfs.ext4"}}`

	reply, err := provider{}.Invoke(context.Background(), &pb.InvokeRequest{EnvJson: []byte(env)})
	if err != nil {
		t.Fatalf("Invoke refused a substrate the served schema accepts: %v", err)
	}

	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the provider never reached the launch command: %v", err)
	}
	got := strings.Fields(string(argv))
	want := []string{"firecracker", "launch", "--kernel", "vmlinux-6.1.155", "--rootfs", "rootfs.ext4"}
	if len(got) != len(want) {
		t.Fatalf("launch argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("launch argv[%d] = %q, want %q (full: %q)", i, got[i], want[i], got)
		}
	}

	var payload map[string]string
	if err := json.Unmarshal(reply.GetResultJson(), &payload); err != nil {
		t.Fatalf("ResultJson is not the documented payload: %v", err)
	}
	if !strings.Contains(payload["launch_output"], "launched") {
		t.Fatalf("ResultJson carries no launch output: %q", payload["launch_output"])
	}

	// A substrate missing either artefact must still be refused: the provider's guard is
	// part of the changed path, and a control that cannot fail is not a control.
	bad := `{"deploy_name":"fc-test","substrate":{"kernel":"vmlinux-6.1.155"}}`
	if _, err := (provider{}).Invoke(context.Background(), &pb.InvokeRequest{EnvJson: []byte(bad)}); err == nil {
		t.Fatal("Invoke accepted a substrate with no rootDrive")
	}
}
