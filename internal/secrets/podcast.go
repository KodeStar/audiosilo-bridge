package secrets

import "strconv"

// PodcastJobName is one claim's sealed feed URL: a later attempt of the job
// seals its own, so the end of an earlier one never deletes it.
func PodcastJobName(id int64, attempt int) string {
	return "podcast_job_" + strconv.FormatInt(id, 10) + "_" + strconv.Itoa(attempt)
}
func PodcastFeedName(showID string) string { return "podcast_feed_" + showID }
