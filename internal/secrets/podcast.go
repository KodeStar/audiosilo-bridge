package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// PodcastJobName is one claim's sealed feed URL, keyed by the server that
// handed the job out (its id, hashed into the name's alphabet) as well as the
// job's id and attempt: job ids are each server's own, so the end of another
// server's job of the same id and attempt never deletes it, and a later
// attempt of the job seals its own, so the end of an earlier one never does
// either. serverID "" is PodcastLegacyJobName.
func PodcastJobName(serverID string, id int64, attempt int) string {
	if serverID == "" {
		return PodcastLegacyJobName(id, attempt)
	}
	sum := sha256.Sum256([]byte(serverID))
	return "podcast_job_" + hex.EncodeToString(sum[:8]) + "_" + strconv.FormatInt(id, 10) + "_" + strconv.Itoa(attempt)
}

// PodcastLegacyJobName is the name an earlier Bridge sealed a claim's
// feed URL under: the job's id and attempt only, whichever server's job it
// was. A job reads or deletes it only when the URL sealed there is its own
// (podcastjob's legacyJobSecret).
func PodcastLegacyJobName(id int64, attempt int) string {
	return "podcast_job_" + strconv.FormatInt(id, 10) + "_" + strconv.Itoa(attempt)
}
func PodcastFeedName(showID string) string { return "podcast_feed_" + showID }

// PodcastETagPrefix starts each sealed resume validator's name (an episode
// checkpoint's strong ETag); only names with it are ever deleted as one.
const PodcastETagPrefix = "podcast_etag_"
