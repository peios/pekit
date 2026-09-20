package pekit

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSourceBundleManifestEncoding(t *testing.T) {
	small := sourceBundleManifest{Schema: 3, Recipe: "workspace/example", SourceVersion: "1.0", Files: []sourceBundleFile{{Path: "source/a", Identity: "0644:" + strings.Repeat("a", 64)}}}
	t.Run("preserves_readable_encoding", func(t *testing.T) {
		got, err := encodeSourceBundleManifest(small)
		want, _ := json.MarshalIndent(small, "", "  ")
		if err != nil || string(got) != string(want) {
			t.Fatalf("small manifest changed: %v", err)
		}
	})
	t.Run("compact_fallback_preserves_every_identity", func(t *testing.T) {
		manifest := small
		manifest.Files = make([]sourceBundleFile, 400000)
		for i := range manifest.Files {
			manifest.Files[i] = sourceBundleFile{Path: fmt.Sprintf("source/%044d", i), Identity: "0644:" + strings.Repeat("a", 64)}
		}
		// Escaped path/link bytes must also survive the fallback without changes.
		manifest.Files[0] = sourceBundleFile{Path: "source/<escaped>&\nname", Identity: "link:target\\name"}
		pretty, _ := json.MarshalIndent(manifest, "", "  ")
		if len(pretty) < 64<<20 {
			t.Fatalf("fixture does not exercise fallback: %d", len(pretty))
		}
		got, err := encodeSourceBundleManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if len(got)+1 > 64<<20 || strings.Contains(string(got), "\n") {
			t.Fatalf("not compact within unchanged consumer limit: %d", len(got))
		}
		var decoded sourceBundleManifest
		if err := json.Unmarshal(append(got, '\n'), &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, manifest) {
			t.Fatal("identity content or metadata changed")
		}
	})
	t.Run("oversized_compact_still_rejected", func(t *testing.T) {
		manifest := small
		manifest.Recipe = strings.Repeat("x", 64<<20)
		if _, err := encodeSourceBundleManifest(manifest); err == nil || !strings.Contains(err.Error(), "source_bundle_limit") {
			t.Fatalf("oversized manifest accepted: %v", err)
		}
	})
}
