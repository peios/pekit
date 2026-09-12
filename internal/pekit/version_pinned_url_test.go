package pekit

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestExactURLSourceWithoutListingNeedsNoEnumeration(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "directory listing forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	for _, constraint := range []string{"= 1.83.0", "=1.83.0", "1.83.0"} {
		cfg := URLSourceConfig{URL: server.URL + "/rust-{{version}}.tar.xz", Versions: constraint}
		versions, err := enumerateBaseURLVersions(cfg)
		if err != nil || !reflect.DeepEqual(versions, []string{"1.83.0"}) {
			t.Fatalf("constraint %q: got %v, %v", constraint, versions, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("an exact URL source attempted directory enumeration")
	}

	// A floor is not an exact pin and must still consult upstream.
	_, err := enumerateBaseURLVersions(URLSourceConfig{
		URL: server.URL + "/rust-{{version}}.tar.xz", Versions: ">= 1.83.0",
	})
	if diagCode(err) != "url_versions" || requests.Load() != 1 {
		t.Fatalf("range did not consult upstream: err=%v requests=%d", err, requests.Load())
	}
	// An explicit listing remains authoritative even with an exact selector.
	_, err = enumerateBaseURLVersions(URLSourceConfig{
		URL: server.URL + "/rust-{{version}}.tar.xz", Versions: "= 1.83.0", ListingURL: server.URL + "/index",
	})
	if diagCode(err) != "url_versions" || requests.Load() != 2 {
		t.Fatalf("explicit listing was bypassed: err=%v requests=%d", err, requests.Load())
	}
}
