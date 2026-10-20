package secrets

import "strconv"

func PodcastJobName(id int64) string       { return "podcast_job_" + strconv.FormatInt(id, 10) }
func PodcastFeedName(showID string) string { return "podcast_feed_" + showID }
