//go:build audiblefake

package audiblefake

import "time"

// Title is one book in the fake's library. The fixtures are synthetic (made-up
// titles, authors and ASINs), shaped from what the manager's client parses; the
// recorded-and-scrubbed set from an owner's real account replaces their shapes
// later, not these names.
type Title struct {
	ASIN      string   `json:"asin"`
	Title     string   `json:"title"`
	Subtitle  string   `json:"subtitle,omitempty"`
	Authors   []string `json:"authors"`
	Narrators []string `json:"narrators,omitempty"`
	Series    string   `json:"series,omitempty"`
	Sequence  string   `json:"sequence,omitempty"`
	// ReleaseIn is the release date relative to the fake's clock, in days: a
	// positive value is a preorder.
	ReleaseIn int `json:"release_in_days"`
	// RuntimeMin is the length in minutes (product_attrs).
	RuntimeMin int `json:"runtime_min,omitempty"`
	// DeliveryType is content_delivery_type ("SinglePartBook" when empty);
	// "Periodical" and "PodcastParent" are unavailable to download.
	DeliveryType string `json:"delivery_type,omitempty"`
	// NoRights clears customer_rights.is_consumable_offline: owned, but Audible
	// won't license a download.
	NoRights bool `json:"no_rights,omitempty"`
}

// State is the title's state as the Bridge reports it to the server:
// available, preorder or unavailable.
func (t Title) State() string {
	switch {
	case t.ReleaseIn > 0:
		return "preorder"
	case t.NoRights, t.DeliveryType == "Periodical", t.DeliveryType == "PodcastParent":
		return "unavailable"
	}
	return "available"
}

// Fixtures are the fake's titles: 30, 22 available, 4 preorders and 4
// unavailable (two shows, two without download rights). The library lists the
// 26 that are out; a preorder shows only in the orders (pending) and the
// catalog until Release puts it in the library (orders.go), as Audible's API
// does. Series books carry their
// sequence; a few are co-written or share a series across authors' credits, so
// matching and placement see the shapes real libraries have.
var Fixtures = []Title{
	{ASIN: "B0FAKE0001", Title: "The Glass Orchard", Authors: []string{"Mara Quill"}, Narrators: []string{"Iris Fenwick"}, Series: "Orchard Cycle", Sequence: "1", ReleaseIn: -2100, RuntimeMin: 712},
	{ASIN: "B0FAKE0002", Title: "The Copper Grove", Authors: []string{"Mara Quill"}, Narrators: []string{"Iris Fenwick"}, Series: "Orchard Cycle", Sequence: "2", ReleaseIn: -1800, RuntimeMin: 698},
	{ASIN: "B0FAKE0003", Title: "The Iron Thicket", Subtitle: "Orchard Cycle, Book 3", Authors: []string{"Mara Quill"}, Narrators: []string{"Iris Fenwick"}, Series: "Orchard Cycle", Sequence: "3", ReleaseIn: -1400, RuntimeMin: 745},
	{ASIN: "B0FAKE0004", Title: "Orchard Cycle 4: The Silver Wood", Authors: []string{"Mara Quill"}, Narrators: []string{"Iris Fenwick"}, Series: "Orchard Cycle", Sequence: "4", ReleaseIn: -900, RuntimeMin: 801},
	{ASIN: "B0FAKE0005", Title: "The Golden Canopy", Authors: []string{"Mara Quill"}, Narrators: []string{"Iris Fenwick"}, Series: "Orchard Cycle", Sequence: "5", ReleaseIn: 21, RuntimeMin: 0},
	{ASIN: "B0FAKE0006", Title: "Lanterns Over Hollow Bay", Authors: []string{"Theo Marchetti"}, Narrators: []string{"Daniel Okafor"}, ReleaseIn: -3000, RuntimeMin: 540},
	{ASIN: "B0FAKE0007", Title: "A Small Hour in Varrow", Authors: []string{"Theo Marchetti"}, Narrators: []string{"Daniel Okafor"}, ReleaseIn: -2500, RuntimeMin: 488},
	{ASIN: "B0FAKE0008", Title: "Signal of the Drowned Choir", Authors: []string{"Priya Halvorsen", "Jonah Reyes"}, Narrators: []string{"Celia Tran", "Marcus Bell"}, Series: "Choir Sequence", Sequence: "1", ReleaseIn: -1200, RuntimeMin: 902},
	{ASIN: "B0FAKE0009", Title: "Echo of the Drowned Choir", Authors: []string{"Priya Halvorsen", "Jonah Reyes"}, Narrators: []string{"Celia Tran", "Marcus Bell"}, Series: "Choir Sequence", Sequence: "2", ReleaseIn: -700, RuntimeMin: 935},
	{ASIN: "B0FAKE0010", Title: "Silence of the Drowned Choir", Authors: []string{"Priya Halvorsen", "Jonah Reyes"}, Narrators: []string{"Celia Tran", "Marcus Bell"}, Series: "Choir Sequence", Sequence: "3", ReleaseIn: 60, RuntimeMin: 0},
	{ASIN: "B0FAKE0011", Title: "The Cartographer's Apprentice", Authors: []string{"Wen Liang"}, Narrators: []string{"Sophie Adair"}, Series: "Mapmakers", Sequence: "1", ReleaseIn: -1600, RuntimeMin: 610},
	{ASIN: "B0FAKE0012", Title: "The Cartographer's Rival", Authors: []string{"Wen Liang"}, Narrators: []string{"Sophie Adair"}, Series: "Mapmakers", Sequence: "2", ReleaseIn: -1100, RuntimeMin: 633},
	{ASIN: "B0FAKE0013", Title: "Mapmakers 2.5: Margins", Authors: []string{"Wen Liang"}, Narrators: []string{"Sophie Adair"}, Series: "Mapmakers", Sequence: "2.5", ReleaseIn: -1000, RuntimeMin: 95},
	{ASIN: "B0FAKE0014", Title: "The Cartographer's Return", Authors: []string{"Wen Liang"}, Narrators: []string{"Sophie Adair"}, Series: "Mapmakers", Sequence: "3", ReleaseIn: -400, RuntimeMin: 655},
	{ASIN: "B0FAKE0015", Title: "Ninefold Gate", Authors: []string{"R. J. Okonkwo"}, Narrators: []string{"Bram Castillo"}, Series: "Ninefold", Sequence: "1", ReleaseIn: -2200, RuntimeMin: 1150},
	{ASIN: "B0FAKE0016", Title: "Ninefold Crown", Authors: []string{"R.J. Okonkwo"}, Narrators: []string{"Bram Castillo"}, Series: "The Ninefold", Sequence: "2", ReleaseIn: -1900, RuntimeMin: 1210},
	{ASIN: "B0FAKE0017", Title: "Ninefold Throne", Authors: []string{"R. J. Okonkwo"}, Narrators: []string{"Bram Castillo"}, Series: "Ninefold", Sequence: "3", ReleaseIn: 3, RuntimeMin: 0},
	{ASIN: "B0FAKE0018", Title: "The Quiet Engineer", Subtitle: "A Memoir", Authors: []string{"Hannah Voss"}, Narrators: []string{"Hannah Voss"}, ReleaseIn: -600, RuntimeMin: 420},
	{ASIN: "B0FAKE0019", Title: "Salt and Ember", Authors: []string{"Callum Dree"}, Narrators: []string{"Nia Brooks"}, ReleaseIn: -2800, RuntimeMin: 515},
	{ASIN: "B0FAKE0020", Title: "Under the Tin Moon", Authors: []string{"Callum Dree"}, Narrators: []string{"Nia Brooks"}, ReleaseIn: -2000, RuntimeMin: 560},
	{ASIN: "B0FAKE0021", Title: "Weatherglass", Authors: []string{"Ada Pennington"}, Narrators: []string{"Oliver Strand"}, Series: "Weatherglass", Sequence: "1", ReleaseIn: -1300, RuntimeMin: 690},
	{ASIN: "B0FAKE0022", Title: "Weatherglass: Squall", Authors: []string{"Ada Pennington"}, Narrators: []string{"Oliver Strand"}, Series: "Weatherglass", Sequence: "2", ReleaseIn: -800, RuntimeMin: 702},
	{ASIN: "B0FAKE0023", Title: "Weatherglass: Gale (Unabridged)", Authors: []string{"Ada Pennington"}, Narrators: []string{"Oliver Strand"}, Series: "Weatherglass", Sequence: "3", ReleaseIn: -200, RuntimeMin: 730},
	{ASIN: "B0FAKE0024", Title: "The Lighthouse Ledger", Authors: []string{"Imogen Sato"}, Narrators: []string{"Felix Moreau"}, ReleaseIn: -150, RuntimeMin: 380},
	{ASIN: "B0FAKE0025", Title: "Paper Kingdoms", Authors: []string{"Imogen Sato"}, Narrators: []string{"Felix Moreau"}, ReleaseIn: -90, RuntimeMin: 402},
	{ASIN: "B0FAKE0026", Title: "Starlight Ferry", Authors: []string{"Imogen Sato"}, Narrators: []string{"Felix Moreau"}, ReleaseIn: 120, RuntimeMin: 0},
	{ASIN: "B0FAKE0027", Title: "The Weekly Brass Hour", Authors: []string{"Brass Hour Media"}, DeliveryType: "Periodical", ReleaseIn: -30},
	{ASIN: "B0FAKE0028", Title: "Tales from the Long Table", Authors: []string{"Long Table Audio"}, DeliveryType: "PodcastParent", ReleaseIn: -400},
	{ASIN: "B0FAKE0029", Title: "The Borrowed Summer", Authors: []string{"Lena Albright"}, Narrators: []string{"Ruth Kimani"}, ReleaseIn: -1500, RuntimeMin: 455, NoRights: true},
	{ASIN: "B0FAKE0030", Title: "Clockwork Harbour", Authors: []string{"Lena Albright"}, Narrators: []string{"Ruth Kimani"}, ReleaseIn: -1700, RuntimeMin: 472, NoRights: true},
}

// position is a last-heard place in the fake's annotations.
type position struct {
	ms      int64
	updated time.Time
}

// fixturePositions are the places the fake starts with: a few books part way,
// one finished, the rest never played (DoesNotExist). Days are before the
// fake's clock.
var fixturePositions = map[string]struct {
	ms      int64
	daysAgo int
}{
	"B0FAKE0001": {ms: 42_720_000, daysAgo: 400}, // finished (712 min)
	"B0FAKE0002": {ms: 12_345_678, daysAgo: 30},  // part way
	"B0FAKE0008": {ms: 3_600_000, daysAgo: 2},    // an hour in
	"B0FAKE0011": {ms: 900_000, daysAgo: 1},      // fifteen minutes in
	"B0FAKE0021": {ms: 20_000_000, daysAgo: 12},  // part way
	"B0FAKE0024": {ms: 60_000, daysAgo: 0},       // just started
	"B0FAKE0029": {ms: 7_000_000, daysAgo: 200},  // a no-rights title still has a place
}
