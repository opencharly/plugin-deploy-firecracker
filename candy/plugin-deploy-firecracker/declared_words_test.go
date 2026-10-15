package firecracker

// TestDeclaredWordsMatchServedClasses — the repo-local check that fails when the candy's
// `plugin.providers:` and the capabilities this plugin ADVERTISES disagree.
//
// WHY IT EXISTS. A provider word in charly.yml is a claim: the kernel routes that class to
// this plugin at runtime, and a route to a class the binary cannot answer is worse than an
// absent word because the registry hands it out anyway (opencharly/charly#916). This repo
// exists precisely to stop the word `deploy:firecracker` being a claim nobody serves — so
// the check that keeps declaration and capability identical belongs here, where the claim
// is made.
//
// It is STRONGER than a copy-comparison: the served side is `providedCapabilities()`, the
// very value NewMeta hands to sdk.NewMeta, so there is no second list to drift. The candy
// is the claim; that function is the capability.
//
// FAILS WITHOUT the parity: add `- deploy:notreal` to charly.yml's providers: and this
// exits non-zero naming it, because no capability advertises that word.

import (
	"os"
	"sort"
	"strings"
	"testing"
)

func TestDeclaredWordsMatchServedClasses(t *testing.T) {
	// The CANDY's own manifest, which is where the `plugin.providers:` claim is made. The
	// repo-root charly.yml is the discovery file and declares no entities, so reading it
	// would compare the capability against an empty list and pass for the wrong reason —
	// which is what the first version of this test did until it failed loudly.
	manifest, err := os.ReadFile("charly.yml")
	if err != nil {
		t.Fatalf("read the candy manifest: %v", err)
	}

	declared := declaredWords(t, string(manifest))
	served := servedWords()
	sort.Strings(declared)
	sort.Strings(served)

	if strings.Join(declared, ",") != strings.Join(served, ",") {
		t.Fatalf("charly.yml and the advertised capabilities disagree:\n  declared: %v\n  served:   %v\nA word declared but not served is a route to a refusal (charly#916); a capability served but not declared is invisible to the registry.", declared, served)
	}
	if len(declared) == 0 {
		t.Fatal("no providers are declared at all — this plugin exists to serve exactly one word")
	}
}

// declaredWords pulls the `providers:` entries out of the manifest's `plugin:` block. It
// is deliberately a narrow reader and not a YAML dependency: the shape it reads is fixed
// by the plugin schema, and a manifest that stops matching it fails loudly here.
func declaredWords(t *testing.T, manifest string) []string {
	t.Helper()
	var out []string
	inProviders := false
	for _, line := range strings.Split(manifest, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "providers:") {
			inProviders = true
			continue
		}
		if !inProviders {
			continue
		}
		if !strings.HasPrefix(trimmed, "-") {
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				break
			}
			continue
		}
		word := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		word = strings.TrimSpace(strings.SplitN(word, "#", 2)[0])
		if word != "" {
			out = append(out, word)
		}
	}
	return out
}

// servedWords renders providedCapabilities() as the same `class:word` form the manifest
// uses, so the two sides are compared in the vocabulary the file is written in.
func servedWords() []string {
	var out []string
	for _, c := range providedCapabilities() {
		out = append(out, c.Class+":"+c.Word)
	}
	return out
}
