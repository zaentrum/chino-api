package katalog

import (
	"context"
	"testing"
)

// adaUpstream is katalog-api's GET /people/p1 for a person it knows a lot
// about.
const adaUpstream = `{"id":"p1","name":"Ada Example","has_profile":true,"sort_name":"Example, Ada",
	"also_known_as":["A. Example"],"birth_date":"1950-03-01","death_date":"2020-11-30",
	"birthplace":"Bern, Switzerland","known_for_department":"Acting",
	"biography":"Ada Example ist Schauspielerin.","biography_lang":"de",
	"tmdb_person_id":"12345","imdb_id":"nm0000123","items":[
	{"id":"s1","type":"series","title":"A Show","sort_title":"Show, A","year":2010,"rating":8,"roles":["actor"]},
	{"id":"m1","type":"movie","title":"A Film","sort_title":"Film, A","year":2001,"rating":7.5,"roles":["actor","director"]}]}`

// A person's details pass through as katalog-api sends them, with the
// portrait proxy's profile_url, and each filmography card keeps the person's
// roles on it; the language asked for goes on to katalog-api.
func TestGetPersonPassesTheDetailsThrough(t *testing.T) {
	up := newUpstream(t, map[string]string{"/api/v1/people/p1": adaUpstream})
	kc := New(up.URL)

	pd, err := kc.GetPerson(context.Background(), "tok", "p1", 0, "de-CH", "fr-CH, de;q=0.9")
	if err != nil || pd == nil {
		t.Fatalf("get person: %v, %v", pd, err)
	}
	sameJSON(t, "person", pd, `{"id":"p1","name":"Ada Example","has_profile":true,
		"profile_url":"/api/v1/people/p1/profile","sort_name":"Example, Ada",
		"also_known_as":["A. Example"],"birth_date":"1950-03-01","death_date":"2020-11-30",
		"birthplace":"Bern, Switzerland","known_for_department":"Acting",
		"biography":"Ada Example ist Schauspielerin.","biography_lang":"de",
		"tmdb_person_id":"12345","imdb_id":"nm0000123","items":[
		{"id":"s1","type":"series","title":"A Show","year":2010,"rating":8,
		 "poster_url":"/api/v1/items/s1/poster","backdrop_url":"/api/v1/items/s1/backdrop","roles":["actor"]},
		{"id":"m1","type":"movie","title":"A Film","year":2001,"rating":7.5,
		 "poster_url":"/api/v1/items/m1/poster","backdrop_url":"/api/v1/items/m1/backdrop","roles":["actor","director"]}]}`)

	r := up.last(t)
	if q := r.URL.Query(); q.Get("lang") != "de-CH" || q.Get("limit") != "100" {
		t.Errorf("upstream query %q, want lang=de-CH and the default limit", r.URL.RawQuery)
	}
	if h := r.Header.Get("Accept-Language"); h != "fr-CH, de;q=0.9" {
		t.Errorf("upstream Accept-Language %q", h)
	}
	if h := r.Header.Get("Authorization"); h != "Bearer tok" {
		t.Errorf("upstream Authorization %q", h)
	}
}

// Without a portrait — or from a katalog-api that does not say — there is no
// profile_url; nothing asked, nothing passed on.
func TestGetPersonWithoutAPortrait(t *testing.T) {
	up := newUpstream(t, map[string]string{
		"/api/v1/people/p2": `{"id":"p2","name":"Bo Writer","has_profile":false,"items":[]}`,
		"/api/v1/people/p3": `{"id":"p3","name":"Cy Older","items":[]}`,
	})
	kc := New(up.URL)
	for id, want := range map[string]string{
		"p2": `{"id":"p2","name":"Bo Writer","has_profile":false,"items":[]}`,
		"p3": `{"id":"p3","name":"Cy Older","has_profile":false,"items":[]}`,
	} {
		pd, err := kc.GetPerson(context.Background(), "", id, 0, "", "")
		if err != nil || pd == nil {
			t.Fatalf("get %s: %v, %v", id, pd, err)
		}
		sameJSON(t, id, pd, want)
		r := up.last(t)
		if r.URL.Query().Has("lang") || r.Header.Get("Accept-Language") != "" {
			t.Errorf("%s: upstream got lang %q, Accept-Language %q; want neither", id, r.URL.Query().Get("lang"), r.Header.Get("Accept-Language"))
		}
	}
	if pd, err := kc.GetPerson(context.Background(), "", "nobody", 0, "", ""); pd != nil || err != nil {
		t.Errorf("unknown person: %v, %v; want nil, nil", pd, err)
	}
}

func TestSearchPeopleSetsProfileURLs(t *testing.T) {
	up := newUpstream(t, map[string]string{"/api/v1/people": `{"people":[
		{"id":"p1","name":"Ada Example","credits":2,"has_profile":true},
		{"id":"p4","name":"Ada Other","credits":1,"has_profile":false}],"total":2}`})
	people, err := New(up.URL).SearchPeople(context.Background(), "tok", "ada", 0)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, "people", people, `[
		{"id":"p1","name":"Ada Example","credits":2,"has_profile":true,"profile_url":"/api/v1/people/p1/profile"},
		{"id":"p4","name":"Ada Other","credits":1,"has_profile":false}]`)
}
