package config

import "testing"

// katalog-manager is where KATALOG_MANAGER_URL says; unset (or blank), where
// ANALYZER_BASE_URL, its former name, says; else its Service in the namespace.
func TestKatalogManagerURL(t *testing.T) {
	for _, tc := range []struct{ manager, analyzer, want string }{
		{"", "", "http://katalog-manager-api"},
		{"", "http://katalog-manager-api.zaentrum.svc.cluster.local", "http://katalog-manager-api.zaentrum.svc.cluster.local"},
		{"http://km:8080", "http://katalog-manager-api.zaentrum.svc.cluster.local", "http://km:8080"},
		{"  ", "http://from-the-old-name", "http://from-the-old-name"},
		{"http://km:8080", "", "http://km:8080"},
	} {
		t.Setenv("KATALOG_MANAGER_URL", tc.manager)
		t.Setenv("ANALYZER_BASE_URL", tc.analyzer)
		if got := Load().KatalogManagerURL; got != tc.want {
			t.Errorf("KATALOG_MANAGER_URL %q, ANALYZER_BASE_URL %q: %q, want %q", tc.manager, tc.analyzer, got, tc.want)
		}
	}
}

// Downloads are on unless DOWNLOADS_ENABLED turns them off; a value that is
// neither on nor off leaves them on.
func TestDownloadsEnabled(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", true},
		{"  ", true},
		{"true", true},
		{"1", true},
		{"ON", true},
		{"false", false},
		{"False", false},
		{"0", false},
		{"no", false},
		{" off ", false},
		{"maybe", true},
	} {
		t.Setenv("DOWNLOADS_ENABLED", tc.value)
		if got := Load().DownloadsEnabled; got != tc.want {
			t.Errorf("DOWNLOADS_ENABLED %q: %v, want %v", tc.value, got, tc.want)
		}
	}
}

// An upstream's base address goes without the slashes it ends in, so that a
// route joined to it has one: katalog-api's, chino-stream's, the artwork's,
// katalog-manager's under either name, portal-api's and OpenProject's.
func TestUpstreamBaseAddressesLoseTheirTrailingSlashes(t *testing.T) {
	for _, base := range []string{"http://up.example/", "http://up.example//", " http://up.example/ "} {
		for _, key := range []string{"KATALOG_BASE_URL", "STREAM_BASE_URL", "ARTWORK_BASE_URL", "KATALOG_MANAGER_URL",
			"PORTAL_BASE_URL", "OPENPROJECT_URL"} {
			t.Setenv(key, base)
		}
		t.Setenv("ANALYZER_BASE_URL", "")
		c := Load()
		for key, got := range map[string]string{"KATALOG_BASE_URL": c.KatalogBaseURL, "STREAM_BASE_URL": c.StreamBaseURL,
			"ARTWORK_BASE_URL": c.ArtworkBaseURL, "KATALOG_MANAGER_URL": c.KatalogManagerURL,
			"PORTAL_BASE_URL": c.PortalBaseURL, "OPENPROJECT_URL": c.OpenProjectURL} {
			if got != "http://up.example" {
				t.Errorf("%s %q: %q, want http://up.example", key, base, got)
			}
		}
	}
	t.Setenv("KATALOG_MANAGER_URL", "")
	t.Setenv("ANALYZER_BASE_URL", "http://up.example/km/")
	if got := Load().KatalogManagerURL; got != "http://up.example/km" {
		t.Errorf("ANALYZER_BASE_URL http://up.example/km/: %q, want http://up.example/km", got)
	}
}
