package katalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Person is a cast/crew member. Credits is the number of titles they are
// credited on (filled by SearchPeople so the UI can show "· 12 titles").
// HasProfile says the catalog holds their portrait.
type Person struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Credits    int    `json:"credits,omitempty"`
	HasProfile bool   `json:"has_profile"`
	// ProfileURL is synthesised on the way out, as an item's poster_url is:
	// chino-api's portrait proxy, set only when there is a portrait.
	ProfileURL string `json:"profile_url,omitempty"`
}

// withProfileURL sets ProfileURL to the portrait proxy when the person has a
// portrait, and clears it otherwise.
func (p Person) withProfileURL() Person {
	p.ProfileURL = ""
	if p.HasProfile && p.ID != "" {
		p.ProfileURL = "/api/v1/people/" + url.PathEscape(p.ID) + "/profile"
	}
	return p
}

// PersonDetail is a person, what katalog-api knows about them (each field
// omitted when unknown) and their filmography. The fields pass through as
// katalog-api sends them. Items are full chino Items (poster/backdrop URLs
// synthesised), each with the person's roles on it, so clients render them
// with the existing poster-card components.
type PersonDetail struct {
	Person
	SortName    string   `json:"sort_name,omitempty"`
	AlsoKnownAs []string `json:"also_known_as,omitempty"`
	// BirthDate and DeathDate are YYYY-MM-DD.
	BirthDate          string `json:"birth_date,omitempty"`
	DeathDate          string `json:"death_date,omitempty"`
	Birthplace         string `json:"birthplace,omitempty"`
	KnownForDepartment string `json:"known_for_department,omitempty"`
	// Biography is one text, in the language BiographyLang names: the one
	// asked for (?lang= or Accept-Language, which GetPerson passes on), else
	// English, else any.
	Biography     string `json:"biography,omitempty"`
	BiographyLang string `json:"biography_lang,omitempty"`
	TMDBPersonID  string `json:"tmdb_person_id,omitempty"`
	IMDbID        string `json:"imdb_id,omitempty"`
	Items         []Item `json:"items"`
}

// upstreamPerson is katalog-api's person: PersonDetail's fields, with the
// filmography in the upstream item shape (its items field shadows the
// embedded one when decoding).
type upstreamPerson struct {
	PersonDetail
	Items []upstreamItem `json:"items"`
}

// SearchPeople proxies katalog-api's /people name search (accent- and
// case-insensitive). Empty q returns an empty slice without a round-trip.
func (c *Client) SearchPeople(ctx context.Context, bearer, q string, limit int) ([]Person, error) {
	if q == "" {
		return []Person{}, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	u := c.BaseURL + "/api/v1/people?q=" + url.QueryEscape(q) + "&limit=" + strconv.Itoa(limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog people: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("katalog people: %d %s", resp.StatusCode, string(body))
	}
	var raw struct {
		People []Person `json:"people"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if raw.People == nil {
		raw.People = []Person{}
	}
	for i := range raw.People {
		raw.People[i] = raw.People[i].withProfileURL()
	}
	return raw.People, nil
}

// GetPerson proxies katalog-api's /people/{id} — the person, their details
// and their filmography. lang (the client's ?lang=) and acceptLanguage (its
// Accept-Language header) are passed on, either may be empty: katalog-api
// picks the biography's language from them. Returns (nil, nil) on 404 so the
// handler answers 404. The filmography items are converted via toItem() so
// poster/backdrop URLs are present (watched_at is stamped by the chino-api
// handler).
func (c *Client) GetPerson(ctx context.Context, bearer, id string, limit int, lang, acceptLanguage string) (*PersonDetail, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if lang != "" {
		q.Set("lang", lang)
	}
	u := c.BaseURL + "/api/v1/people/" + url.PathEscape(id) + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if acceptLanguage != "" {
		req.Header.Set("Accept-Language", acceptLanguage)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog person: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("katalog person: %d %s", resp.StatusCode, string(body))
	}
	var raw upstreamPerson
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	pd := raw.PersonDetail
	pd.Person = pd.Person.withProfileURL()
	pd.Items = make([]Item, 0, len(raw.Items))
	for _, it := range raw.Items {
		pd.Items = append(pd.Items, it.toItem())
	}
	return &pd, nil
}
