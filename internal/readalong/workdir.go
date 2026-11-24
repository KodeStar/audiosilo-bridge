package readalong

// The read-along working files in an engine book's work dir (the
// readalong_aligning stage, internal/pipeline), named here so internal/scratch,
// which pipeline imports, reclaims heal/ with audio scratch and words.json at
// transcript expiry. The map stays available for a parked library write.
const (
	// WorkDir holds the stage's files.
	WorkDir = "readalong"
	// RequestPath is the job's request: the ebook and the server's files.
	RequestPath = WorkDir + "/request.json"
	// WordsPath is the words the map was aligned from, on the book's timeline.
	WordsPath = WorkDir + "/words.json"
	// HealPath holds the heal windows' clips (removed once transcribed) and raws.
	HealPath = WorkDir + "/heal"
	// MapPath is the map the job writes into the library.
	MapPath = WorkDir + "/map.json"
	// ReportPath is the stage's figures (coverage, heal, snap), no text.
	ReportPath = WorkDir + "/align_report.json"
)
