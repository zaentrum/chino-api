package http

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/store"
)

// One file can hold several episodes (a double-length finale listed as two):
// the episodes of one file share a viewer's position and watched state. A
// write of either, for any of them, is written for each (fileOf); a read is
// the episode's own row, as it always was.

// fileAskTimeout bounds how long a write waits for katalog-api to say which
// episodes the file holds; past it the write goes on without.
const fileAskTimeout = 3 * time.Second

// fileOf is the items a write of userID's playback for the item id goes to:
// every episode of the file id plays, as katalog-api names them
// (katalog.Client.FileEpisodes), else id alone. Best effort: when katalog-api
// fails, or does not answer in fileAskTimeout, the write is id's alone, as
// it was before a file held several episodes. Nothing is asked when nothing
// is written (no database, no viewer).
func fileOf(r *http.Request, st *store.Store, kc *katalog.Client, userID, id string) []string {
	if st == nil || userID == "" {
		return []string{id}
	}
	ctx, cancel := context.WithTimeout(r.Context(), fileAskTimeout)
	defer cancel()
	ids, err := kc.FileEpisodes(ctx, bearerFrom(r), id)
	if err != nil {
		log.Printf("chino-api: the episodes of the file of %s could not be asked, it is written alone: %v", id, err)
		return []string{id}
	}
	return ids
}
