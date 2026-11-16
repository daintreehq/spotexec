package client

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
)

// cacheProvider is a regional provider offering the shared cache.
type cacheProvider struct{ provider.Provider }

func (cacheProvider) CacheURLs(context.Context, string, string) (string, string, error) {
	return "https://get", "https://put", nil
}

// plainProvider offers no shared cache (an SSH runner).
type plainProvider struct{ provider.Provider }

// placedProvider is a multi-region runner: For finds the instance's region.
type placedProvider struct {
	provider.Provider
	region provider.Provider
	err    error
}

func (p placedProvider) For(context.Context, string) (provider.Provider, error) {
	return p.region, p.err
}

// A multi-region fleet that can't find the VM's region runs without the
// shared cache and says why (#228); one that finds it uses that region's
// cache; a runner without a shared cache, or with it turned off, says
// nothing.
func TestDepsCacheForSaysWhyItHasNone(t *testing.T) {
	regional := cacheProvider{}
	for _, tc := range []struct {
		name   string
		bucket string
		p      provider.Provider
		cache  bool
		warned string
	}{
		{"region found", "", placedProvider{region: regional}, true, ""},
		{"region lookup fails", "", placedProvider{err: errors.New("can't find instance i-1: ap-south-1: timeout")}, false, "finding the region of i-1: can't find instance i-1: ap-south-1: timeout"},
		{"cache off", "off", placedProvider{err: errors.New("can't find instance i-1")}, false, ""},
		{"single region", "", regional, true, ""},
		{"no shared cache", "", plainProvider{}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := &recRep{}
			dc := depsCacheFor(context.Background(), Options{Rep: rep}, &config.Runner{CacheBucket: tc.bucket}, tc.p, "i-1")
			if (dc != nil) != tc.cache {
				t.Fatalf("cache = %v, want %v", dc, tc.cache)
			}
			switch {
			case tc.warned == "" && len(rep.warns) > 0:
				t.Fatalf("warnings %q, want none", rep.warns)
			case tc.warned != "" && (len(rep.warns) != 1 || !strings.HasPrefix(rep.warns[0], "DEPS_CACHE: ") || !strings.Contains(rep.warns[0], tc.warned)):
				t.Fatalf("warnings %q, want one DEPS_CACHE naming %q", rep.warns, tc.warned)
			}
		})
	}
}

// The VM failing to download the cache's archive is a warning, which the
// run's record keeps, once however often the helper reports it.
func TestDepsCacheErrorsWarnOnce(t *testing.T) {
	rep := &recRep{}
	on := depsCacheErrors(Options{Rep: rep})
	on(protocol.Event{Name: "deps_cache", Message: "checking shared cache"})
	on(protocol.Event{Name: "deps_cache_error", Message: "cache download: HTTP 404"})
	on(protocol.Event{Name: "deps_cache_error", Message: "cache download: HTTP 503"})
	if len(rep.warns) != 1 || rep.warns[0] != "DEPS_CACHE: the VM couldn't use the shared dependency cache: cache download: HTTP 404" {
		t.Fatalf("warnings %q", rep.warns)
	}
}
