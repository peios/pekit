package pekit

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestExactURLSourceWithoutListingNeedsNoEnumeration(t *testing.T) {
	requests := 0
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: io.NopCloser(strings.NewReader("directory listing forbidden")), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })
	const baseURL = "https://example.test"
	for _, constraint := range []string{"= 1.83.0", "=1.83.0", "1.83.0"} {
		cfg := URLSourceConfig{URL: baseURL + "/rust-{{version}}.tar.xz", Versions: constraint}
		versions, err := enumerateBaseURLVersions(cfg)
		if err != nil || !reflect.DeepEqual(versions, []string{"1.83.0"}) {
			t.Fatalf("constraint %q: got %v, %v", constraint, versions, err)
		}
	}
	if requests != 0 {
		t.Fatal("an exact URL source attempted directory enumeration")
	}

	// A floor is not an exact pin and must still consult upstream.
	_, err := enumerateBaseURLVersions(URLSourceConfig{
		URL: baseURL + "/rust-{{version}}.tar.xz", Versions: ">= 1.83.0",
	})
	if diagCode(err) != "url_versions" || requests != 1 {
		t.Fatalf("range did not consult upstream: err=%v requests=%d", err, requests)
	}
	// An explicit listing remains authoritative even with an exact selector.
	_, err = enumerateBaseURLVersions(URLSourceConfig{
		URL: baseURL + "/rust-{{version}}.tar.xz", Versions: "= 1.83.0", ListingURL: baseURL + "/index",
	})
	if diagCode(err) != "url_versions" || requests != 2 {
		t.Fatalf("explicit listing was bypassed: err=%v requests=%d", err, requests)
	}
}
