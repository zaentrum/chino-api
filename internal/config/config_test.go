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
