package http

// sampleItems returns hardcoded items shaped for the product. Replaced once
// the shared `katalog` backend is online.
func sampleItems() []map[string]any {
	switch "chino" {
	case "chino":
		return []map[string]any{
			{"id": "m-001", "kind": "movie", "title": "The Last Journey", "year": 2024},
			{"id": "s-002", "kind": "series", "title": "Mountain Echoes", "year": 2023},
		}
	case "fernseh":
		return []map[string]any{
			{"id": "ch-srf1", "kind": "channel", "name": "SRF 1", "live": true},
			{"id": "ch-zdf",  "kind": "channel", "name": "ZDF",   "live": true},
		}
	case "musig":
		return []map[string]any{
			{"id": "alb-001", "kind": "album", "title": "Beta Tracks", "artist": "Various"},
		}
	}
	return []map[string]any{}
}
