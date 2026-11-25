package jobs

import (
	"fmt"
	"regexp"
	"strings"
)

// A progress sentence the Bridge sends from a fixed set has a code (server#224):
// the console words the code in the admin's language (its locale key
// bridge.progress.<code>) and fills in the params, so the English message is
// only what an older console, or a code it doesn't know, shows. The codes are
// wire: never rename or reuse one; a new sentence gets a new code, and an
// English template changes only in wording (the console's en.json mirrors
// these templates).
//
// Placeholders are i18next's ({{name}}), each filled from Progress.Params.
const (
	SayASRTurnWait           = "asr_turn_wait"
	SayStarting              = "starting"
	SayFinished              = "finished"
	SayEngineStarting        = "engine_starting"
	SayLibraryFolderWait     = "library_folder_wait"
	SayBookFolderWait        = "book_folder_wait"
	SayFolderWait            = "folder_wait"
	SayBookLookup            = "book_lookup"
	SayBookFilesLookup       = "book_files_lookup"
	SayMappingWait           = "mapping_wait"
	SayEarlierJobWait        = "earlier_job_wait"
	SayEngineReleaseWait     = "engine_release_wait"
	SayChaptersJobWait       = "chapters_job_wait"
	SayDownloadWait          = "download_wait"
	SaySavingSidecars        = "saving_sidecars"
	SaySavingChapters        = "saving_chapters"
	SaySavingReadalong       = "saving_readalong"
	SaySavingDescription     = "saving_description"
	SayRescanThem            = "rescan_them"
	SayRescanIt              = "rescan_it"
	SayDescribing            = "describing"
	SayDescribingParked      = "describing_parked"
	SaySharing               = "sharing"
	SaySharingParked         = "sharing_parked"
	SayShared                = "shared"
	SayFindingLibrary        = "finding_library"
	SayComparingFiles        = "comparing_files"
	SayComparedFiles         = "compared_files"
	SayCheckingWritable      = "checking_writable"
	SayHolding               = "holding"
	SayReadingCommunity      = "reading_community_record"
	SayFilingCorrections     = "filing_corrections"
	SayFiled                 = "filed"
	SayReadingAudiblePlaces  = "reading_audible_places"
	SaySendingPlaces         = "sending_places"
	SayListingAudibleLibrary = "listing_audible_library"
	SayListingPreorders      = "listing_audible_preorders"
	SayReadingLibraryBooks   = "reading_library_books"
	SayMatchingTitles        = "matching_titles"
	SayReportingTitles       = "reporting_titles"
	SayAddingASINs           = "adding_asins"
	SayAddingASIN            = "adding_asin"
	SayServerSlowDown        = "server_slow_down"
	SayCopyingBook           = "copying_book"
	SayReadingAudibleTitle   = "reading_audible_title"
	SayDownloadingAudible    = "downloading_audible"
	SayRemovingDRM           = "removing_drm"
	SayIndexingBook          = "indexing_book"
	SayAccountBackupWait     = "account_backup_wait"
	SayRenewingLicense       = "renewing_license"
	SayLibraryNotWritable    = "library_not_writable"
	SayCheckingRetained      = "checking_retained"
	SayFetchingFeed          = "fetching_feed"
	SayReadingFeed           = "reading_feed"
	SayCheckingFeed          = "checking_feed"
	SaySavingShow            = "saving_show"
	SaySavingShowChanges     = "saving_show_changes"
	SayIndexingShow          = "indexing_show"
	SayResolvingEpisode      = "resolving_episode"
	SayCheckingEpisodeAudio  = "checking_episode_audio"
	SayFetchingEpisode       = "fetching_episode"
	SaySavingEpisode         = "saving_episode"
	SayIndexingEpisode       = "indexing_episode"
	SayDownloadingSection    = "downloading_section"
	SayDownloadedArchive     = "downloaded_archive"
	SayDownloadingCover      = "downloading_cover"
	SayEarlierFetchWait      = "earlier_fetch_wait"
	SayReadingRecording      = "reading_recording"
	SayMetadataRetrying      = "metadata_retrying"
	SayPlacingBook           = "placing_book"
)

// phrases are the codes' English sentences, which Progress.Text fills in.
var phrases = map[string]string{
	SayASRTurnWait:           "Waiting for a turn to use speech recognition",
	SayStarting:              "Starting",
	SayFinished:              "Finished",
	SayEngineStarting:        "Waiting for the engine to start",
	SayLibraryFolderWait:     "Waiting for the library's folder to answer",
	SayBookFolderWait:        "Waiting for the book's folder to answer",
	SayFolderWait:            "Waiting for the folder",
	SayBookLookup:            "Asking the server for the book",
	SayBookFilesLookup:       "Asking the server for the book's files",
	SayMappingWait:           "Waiting for the library's mapping check",
	SayEarlierJobWait:        "Waiting for the book's earlier job to end",
	SayEngineReleaseWait:     "Waiting for the engine to let go of the book's earlier run",
	SayChaptersJobWait:       "Waiting for the book's chapters job to give way",
	SayDownloadWait:          "Waiting for the Bridge's other download to finish",
	SaySavingSidecars:        "Saving the characters and recaps with the book",
	SaySavingChapters:        "Saving the chapters with the book",
	SaySavingReadalong:       "Saving the read-along with the book",
	SaySavingDescription:     "Saving the description with the book",
	SayRescanThem:            "Asking the server to read them",
	SayRescanIt:              "Asking the server to read it",
	SayDescribing:            "Writing a description",
	SayDescribingParked:      "Needs attention: Writing a description",
	SaySharing:               "Sharing with the community",
	SaySharingParked:         "Needs attention: Sharing with the community",
	SayShared:                "Shared",
	SayFindingLibrary:        "Finding the library",
	SayComparingFiles:        "Comparing the files",
	SayComparedFiles:         "Compared the files",
	SayCheckingWritable:      "Checking the book's folder can be written",
	SayHolding:               "Holding, as asked",
	SayReadingCommunity:      "Reading the book's community record",
	SayFilingCorrections:     "Filing the corrections",
	SayFiled:                 "Filed",
	SayReadingAudiblePlaces:  "Reading the places on Audible",
	SaySendingPlaces:         "Sending the places to the server",
	SayListingAudibleLibrary: "Listing the Audible library",
	SayListingPreorders:      "Listing the Audible preorders",
	SayReadingLibraryBooks:   "Reading the library's books on the server",
	SayMatchingTitles:        "Matching the titles to the library",
	SayReportingTitles:       "Reporting the titles to the server",
	SayAddingASINs:           "Adding the ASINs of the matched books",
	SayAddingASIN:            "Adding the title's ASIN to the book",
	SayServerSlowDown:        "The server asked the Bridge to slow down: waiting",
	SayCopyingBook:           "Copying the book into the library",
	SayReadingAudibleTitle:   "Reading the title from Audible",
	SayDownloadingAudible:    "Downloading from Audible",
	SayRemovingDRM:           "Removing the DRM (a lossless copy)",
	SayIndexingBook:          "Asking the server to index the book",
	SayAccountBackupWait:     "Waiting for this account's other backup to finish",
	SayRenewingLicense:       "The download link expired: asking Audible for a new license",
	SayLibraryNotWritable:    "Needs attention: Library {{library}} must be mapped and writable on the Bridge: check its mapping on the Bridge's Status tab.",
	SayCheckingRetained:      "Checking retained episodes",
	SayFetchingFeed:          "Fetching the podcast feed",
	SayReadingFeed:           "Reading the podcast feed",
	SayCheckingFeed:          "Checking the podcast feed",
	SaySavingShow:            "Saving the podcast show",
	SaySavingShowChanges:     "Saving changed podcast details",
	SayIndexingShow:          "Indexing the podcast show",
	SayResolvingEpisode:      "Resolving the episode",
	SayCheckingEpisodeAudio:  "Checking episode audio",
	SayFetchingEpisode:       "Fetching episode",
	SaySavingEpisode:         "Saving the episode",
	SayIndexingEpisode:       "Indexing the episode",
	SayDownloadingSection:    "Downloading section {{section}} of {{sections}} from archive.org",
	SayDownloadedArchive:     "Downloaded from archive.org",
	SayDownloadingCover:      "Downloading the cover from archive.org",
	SayEarlierFetchWait:      "Waiting for the Bridge's earlier run of this fetch to end",
	SayReadingRecording:      "Reading the recording from the metadata database",
	SayMetadataRetrying:      "The metadata database isn't answering: trying again",
	SayPlacingBook:           "Placing the book in the library",
}

// placeholder is one {{name}} in a phrase.
var placeholder = regexp.MustCompile(`\{\{(\w+)\}\}`)

// Phrase is code's English sentence with params filled in, "" for a code with
// none. A placeholder params lacks stays as written.
func Phrase(code string, params map[string]any) string {
	tmpl, ok := phrases[code]
	if !ok {
		return ""
	}
	if !strings.Contains(tmpl, "{{") {
		return tmpl
	}
	return placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		if v, ok := params[m[2:len(m)-2]]; ok {
			return fmt.Sprint(v)
		}
		return m
	})
}

// needsAttention leads a parked progress's phrase; the park's own reason is
// the sentence without it.
const needsAttention = "Needs attention: "

// LibraryNotWritable is SayLibraryNotWritable's params for the library named
// name, and the reason a job parks (or fails) on with it: the phrase without
// its "Needs attention: " lead.
func LibraryNotWritable(name string) (params map[string]any, reason string) {
	params = map[string]any{"library": name}
	return params, strings.TrimPrefix(Phrase(SayLibraryNotWritable, params), needsAttention)
}
