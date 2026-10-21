package link

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

type PodcastPolicy struct {
	Mode string `json:"mode"`
	Keep int    `json:"keep"`
	Days int    `json:"days"`
}

func (p PodcastPolicy) Valid() bool {
	return (p.Mode == "keep" || p.Mode == "on_demand" || p.Mode == "archive") && p.Keep >= 1 && p.Keep <= 1000 && p.Days >= 0 && p.Days <= 365
}

type PodcastRetentionRequest struct {
	ShowID     string `json:"show_id"`
	Path       string `json:"path"`
	Generation int    `json:"generation"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
}
type PodcastRetentionCheck struct {
	LibraryID         int64         `json:"library_id"`
	ShowPath          string        `json:"show_path"`
	ShowID            string        `json:"show_id"`
	Path              string        `json:"path"`
	Generation        int           `json:"generation"`
	SHA256            string        `json:"sha256"`
	Bytes             int64         `json:"bytes"`
	CheckedAt         string        `json:"checked_at"`
	Policy            PodcastPolicy `json:"policy"`
	Followers         int           `json:"followers"`
	AllFinished       bool          `json:"all_finished"`
	AllFinishedAt     *string       `json:"all_finished_at"`
	Starred           bool          `json:"starred"`
	Queued            bool          `json:"queued"`
	ActiveStream      bool          `json:"active_stream"`
	FetchOpen         bool          `json:"fetch_open"`
	OutsideKeepWindow bool          `json:"outside_keep_window"`
	DeleteAfter       *string       `json:"delete_after"`
	Eligible          bool          `json:"eligible"`
}

func (c *Client) PodcastRetention(ctx context.Context, id int64, show, showID string) (*PodcastPolicy, error) {
	var out struct {
		Retention PodcastPolicy `json:"retention"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/bridge/libraries/"+strconv.FormatInt(id, 10)+"/podcasts/retention", url.Values{"path": {show}, "show_id": {showID}}, nil, &out)
	if err != nil {
		return nil, err
	}
	return &out.Retention, nil
}
func (c *Client) CheckPodcastRetention(ctx context.Context, id int64, in PodcastRetentionRequest) (*PodcastRetentionCheck, error) {
	var out PodcastRetentionCheck
	err := c.do(ctx, http.MethodPost, "/api/v1/bridge/libraries/"+strconv.FormatInt(id, 10)+"/podcasts/retention/check", nil, in, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UnmarshalJSON requires the complete deletion authority snapshot while allowing
// additive response fields. A partial older response must fail closed.
func (c *PodcastRetentionCheck) UnmarshalJSON(b []byte) error {
	type plain PodcastRetentionCheck
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	for _, key := range []string{"library_id", "show_path", "show_id", "path", "generation", "sha256", "bytes", "checked_at", "policy", "followers", "all_finished", "all_finished_at", "starred", "queued", "active_stream", "fetch_open", "outside_keep_window", "delete_after", "eligible"} {
		if value, ok := fields[key]; !ok || string(value) == "null" && key != "all_finished_at" && key != "delete_after" {
			return errors.New("incomplete retention snapshot")
		}
	}
	var value plain
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	*c = PodcastRetentionCheck(value)
	return nil
}
