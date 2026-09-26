package depupgrade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/config"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

func testResolver(t *testing.T, h http.HandlerFunc) *Resolver {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := config.Config{GoRegistryURL: srv.URL, NPMRegistryURL: srv.URL, CargoRegistryURL: srv.URL, PyPIRegistryURL: srv.URL}
	r, err := NewResolver(cfg, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func resolveTarget(t *testing.T, r *Resolver, ecosystem, name, fixed, policy, want string) {
	t.Helper()
	got, err := r.Resolve(context.Background(), reviewengine.PackageIdentity{Ecosystem: ecosystem, Name: name, FixedVersion: fixed}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetVersion != want || got.Status != StatusResolved || got.Source != "registry" || got.Degraded {
		t.Fatalf("resolve %s %s: got %+v, want registry target %s", ecosystem, policy, got, want)
	}
}

func TestGoProxyVersionsAndCaseEncoding(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.URL.Path, "example.com/!mixed/!mod/") {
			t.Errorf("Go proxy path not case-escaped: %s", req.URL.Path)
		}
		switch {
		case strings.HasSuffix(req.URL.Path, "/@v/list"):
			fmt.Fprint(w, "v1.2.3\nv1.2.4-rc.1\nv1.2.5+incompatible\nv1.2.6\nv1.2.7-rc.1\n")
		case strings.HasSuffix(req.URL.Path, "/@latest"):
			fmt.Fprint(w, `{"Version":"v1.2.7-rc.1","Time":"2026-01-01T00:00:00Z"}`)
		case strings.HasSuffix(req.URL.Path, "/@v/v1.2.7-rc.1.mod"):
			fmt.Fprint(w, "module example.com/Mixed/Mod\n")
		default:
			http.NotFound(w, req)
		}
	})
	resolveTarget(t, r, "go", "example.com/Mixed/Mod", "1.2.4", PolicyNextPatch, "v1.2.5+incompatible")
	resolveTarget(t, r, "go", "example.com/Mixed/Mod", "v1.2.4", PolicyLatest, "v1.2.6")
}

func TestGoPseudoVersionOrdersBelowRelease(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/@v/list"):
			fmt.Fprint(w, "v1.2.2\nv1.2.3-rc.1\nv1.2.3\nv1.2.4\n")
		case strings.HasSuffix(req.URL.Path, "/@latest"):
			fmt.Fprint(w, `{"Version":"v1.2.4"}`)
		case strings.HasSuffix(req.URL.Path, "/@v/v1.2.4.mod"):
			fmt.Fprint(w, "module example.com/mod\n")
		default:
			http.NotFound(w, req)
		}
	})
	resolveTarget(t, r, "go", "example.com/mod", "1.2.3-0.20260101010101-abcdefabcdef", PolicyNextPatch, "v1.2.3")
}

func TestGoRetractedVersionIsSkipped(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/@v/list"):
			fmt.Fprint(w, "v1.2.3\nv1.2.4\nv1.2.5\n")
		case strings.HasSuffix(req.URL.Path, "/@latest"):
			fmt.Fprint(w, `{"Version":"v1.2.5"}`)
		case strings.HasSuffix(req.URL.Path, "/@v/v1.2.5.mod"):
			fmt.Fprint(w, "module example.com/mod\nretract v1.2.4 // bad release\n")
		default:
			http.NotFound(w, req)
		}
	})
	resolveTarget(t, r, "go", "example.com/mod", "v1.2.4", PolicyNextPatch, "v1.2.5")
}

func TestNPMScopedPackumentAndDistTag(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.EscapedPath() != "/@scope%2Fname" {
			t.Errorf("scoped npm path = %q", req.URL.EscapedPath())
		}
		if req.Header.Get("Accept") != "application/vnd.npm.install-v1+json" {
			t.Error("expected abbreviated packument")
		}
		fmt.Fprint(w, `{"name":"@scope/name","dist-tags":{"latest":"3.0.0"},"versions":{"1.2.3":{},"1.2.4-beta.1":{},"1.2.4":{"deprecated":"bad"},"1.2.5":{},"3.0.0":{}}}`)
	})
	resolveTarget(t, r, "npm", "@scope/name", "1.2.4", PolicyNextPatch, "1.2.5")
	resolveTarget(t, r, "npm", "@scope/name", "1.2.4", PolicyLatest, "3.0.0")
}

func TestNPMLatestHonorsDistTagEvenWhenPrerelease(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"name":"pkg","dist-tags":{"latest":"2.0.0-rc.1"},"versions":{"1.2.4":{},"2.0.0-rc.1":{}}}`)
	})
	resolveTarget(t, r, "npm", "pkg", "1.2.4", PolicyLatest, "2.0.0-rc.1")
	resolveTarget(t, r, "npm", "pkg", "1.2.4", PolicyNextPatch, "1.2.4")
}

func TestCargoYankedAndStableOrdering(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/crates/serde" {
			t.Errorf("Cargo path = %q", req.URL.Path)
		}
		fmt.Fprint(w, `{"crate":{"name":"serde","max_stable_version":"2.0.0"},"versions":[{"num":"1.2.4","yanked":true},{"num":"1.2.5-beta.1","yanked":false},{"num":"1.2.6","yanked":false},{"num":"2.0.0","yanked":false}]}`)
	})
	resolveTarget(t, r, "cargo", "serde", "1.2.4", PolicyNextPatch, "1.2.6")
	resolveTarget(t, r, "cargo", "serde", "1.2.4", PolicyLatest, "2.0.0")
}

func TestPyPIPEP440AndYanks(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/pypi/my-package/json" {
			t.Errorf("PyPI normalized path = %q", req.URL.Path)
		}
		fmt.Fprint(w, `{"info":{"name":"My_Package","version":"2.0rc1"},"releases":{"1.0":[{"yanked":false}],"1.0.post1":[{"yanked":false}],"1.0.post2":[{"yanked":true}],"1.1.dev1":[{"yanked":false}],"1.1":[{"yanked":false}],"2.0rc1":[{"yanked":false}],"2.0":[{"yanked":false}],"1!0.1":[{"yanked":false}]}}`)
	})
	resolveTarget(t, r, "pip", "My_Package", "1.0.post1", PolicyNextPatch, "1.0.post1")
	resolveTarget(t, r, "pip", "My_Package", "1.0.post2", PolicyNextPatch, "1.1")
	resolveTarget(t, r, "pip", "My_Package", "2.0rc1", PolicyNextPatch, "2.0")
	resolveTarget(t, r, "pip", "My_Package", "2.0rc1", PolicyLatest, "1!0.1")
}

func TestNoFixNeverQueriesRegistry(t *testing.T) {
	var calls atomic.Int32
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) { calls.Add(1); t.Error("unexpected registry request") })
	got, err := r.Resolve(context.Background(), reviewengine.PackageIdentity{Ecosystem: "npm", Name: "name"}, PolicyLatest)
	if err != nil || got.Status != StatusNoFix || got.TargetVersion != "" || calls.Load() != 0 {
		t.Fatalf("no fix: %+v, err=%v, requests=%d", got, err, calls.Load())
	}
}

func TestUnavailableRegistryDegradesButNot404(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	got, err := r.Resolve(context.Background(), reviewengine.PackageIdentity{Ecosystem: "npm", Name: "name", FixedVersion: "1.2.3"}, PolicyNextPatch)
	if err != nil || got.TargetVersion != "1.2.3" || !got.Degraded || got.Source != "scanner_fallback" || got.Warning == "" {
		t.Fatalf("fallback: %+v, err=%v", got, err)
	}
	r = testResolver(t, func(w http.ResponseWriter, req *http.Request) { http.NotFound(w, req) })
	got, err = r.Resolve(context.Background(), reviewengine.PackageIdentity{Ecosystem: "npm", Name: "name", FixedVersion: "1.2.3"}, PolicyNextPatch)
	if err == nil || got.TargetVersion != "" {
		t.Fatalf("404 must not degrade: %+v, %v", got, err)
	}
}

func TestCargoIncompleteVersionListDoesNotChooseTarget(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"crate":{"name":"crate","num_versions":2},"versions":[{"num":"1.2.4","yanked":false}]}`)
	})
	got, err := r.Resolve(context.Background(), reviewengine.PackageIdentity{Ecosystem: "cargo", Name: "crate", FixedVersion: "1.2.3"}, PolicyNextPatch)
	if err == nil || got.TargetVersion != "" {
		t.Fatalf("incomplete list: %+v, %v", got, err)
	}
}

func TestNoPublishedStableTargetDoesNotGuess(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"crate":{"name":"crate"},"versions":[{"num":"1.2.3","yanked":true},{"num":"1.2.4-rc.1","yanked":false}]}`)
	})
	got, err := r.Resolve(context.Background(), reviewengine.PackageIdentity{Ecosystem: "cargo", Name: "crate", FixedVersion: "1.2.3"}, PolicyNextPatch)
	if !errors.Is(err, ErrNoPublishedTarget) || got.TargetVersion != "" {
		t.Fatalf("no target: %+v, %v", got, err)
	}
}

func TestRegistryRedirectIsNotFollowed(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, destination.URL, http.StatusFound)
	})
	got, err := r.Resolve(context.Background(), reviewengine.PackageIdentity{Ecosystem: "npm", Name: "name", FixedVersion: "1.2.3"}, PolicyNextPatch)
	if err == nil || got.TargetVersion != "" || redirected.Load() != 0 {
		t.Fatalf("redirect must not be followed or degraded: %+v, %v, requests=%d", got, err, redirected.Load())
	}
}

func TestProductionResolverRejectsPlaintext(t *testing.T) {
	cfg := config.Config{Production: true, GoRegistryURL: "http://example.com", NPMRegistryURL: "https://registry.npmjs.org", CargoRegistryURL: "https://crates.io", PyPIRegistryURL: "https://pypi.org"}
	if _, err := NewResolver(cfg, nil); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("expected production HTTPS rejection, got %v", err)
	}
}

func TestBoundedResponseAndTimeout(t *testing.T) {
	r := testResolver(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", maxRegistryResponseBytes+1))
	})
	_, err := r.Resolve(context.Background(), reviewengine.PackageIdentity{Ecosystem: "go", Name: "example.com/mod", FixedVersion: "v1.0.0"}, PolicyNextPatch)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded response: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { <-req.Context().Done() }))
	defer srv.Close()
	cfg := config.Config{GoRegistryURL: srv.URL, NPMRegistryURL: srv.URL, CargoRegistryURL: srv.URL, PyPIRegistryURL: srv.URL}
	r, err = NewResolver(cfg, &http.Client{Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve(context.Background(), reviewengine.PackageIdentity{Ecosystem: "go", Name: "example.com/mod", FixedVersion: "1.0.0"}, PolicyNextPatch)
	if err != nil || !got.Degraded || got.TargetVersion != "v1.0.0" {
		t.Fatalf("timeout fallback: %+v, %v", got, err)
	}
}
