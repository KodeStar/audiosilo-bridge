package generate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Sources: what the run reads. auto takes an .epub beside the audio when there
// is a readable one, else the audio.
const (
	SourceAuto  = "auto"
	SourceAudio = "audio"
	SourceEbook = "ebook"
)

// Contribution modes (decision 6): local keeps the sidecars on this server only;
// issue also files the community intake issue.
const (
	ContributeLocal = "local"
	ContributeIssue = "issue"
)

// maxWorkID bounds params.work_id (the server's own bound).
const maxWorkID = 200

// Params are sidecars.generate's params. The server checks them strictly and
// stores them normalized; the Bridge checks them again, but decodes leniently so
// a field a later server adds doesn't fail every job.
type Params struct {
	Source     string `json:"source"`
	Contribute string `json:"contribute"`
	WorkID     string `json:"work_id"`
}

// ParseParams decodes and checks a job's params, filling the defaults.
func ParseParams(raw json.RawMessage) (Params, error) {
	var p Params
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return Params{}, fmt.Errorf("bad params: %w", err)
		}
	}
	p.Source, p.Contribute, p.WorkID = strings.TrimSpace(p.Source), strings.TrimSpace(p.Contribute), strings.TrimSpace(p.WorkID)
	switch p.Source {
	case "":
		p.Source = SourceAuto
	case SourceAuto, SourceAudio, SourceEbook:
	default:
		return Params{}, fmt.Errorf("source %q must be auto, audio or ebook", p.Source)
	}
	switch p.Contribute {
	case "":
		p.Contribute = ContributeLocal
	case ContributeLocal, ContributeIssue:
	default:
		return Params{}, fmt.Errorf("contribute %q must be local or issue", p.Contribute)
	}
	if len(p.WorkID) > maxWorkID {
		return Params{}, fmt.Errorf("work_id is longer than %d bytes", maxWorkID)
	}
	if strings.ContainsFunc(p.WorkID, unicode.IsControl) {
		return Params{}, errors.New("work_id holds a control character")
	}
	return p, nil
}
