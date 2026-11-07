package audible

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestReleaseDatePlausible: a placeholder release date (Audible sends
// 2200-01-01 for some titles it has had for years) is no date, so the title is
// not a pre-order; issue_date stands in when it is plausible; a real date
// months ahead is still a pre-order.
func TestReleaseDatePlausible(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	rights := &customerRights{IsConsumableOffline: true}
	cases := []struct {
		name         string
		it           libItem
		wantPreorder bool
		wantDate     string
	}{
		{"placeholder 2200, no issue date", libItem{ReleaseDate: "2200-01-01", CustomerRights: rights}, false, ""},
		{"placeholder 2200, issue date stands in", libItem{ReleaseDate: "2200-01-01", IssueDate: "2019-10-03", CustomerRights: rights}, false, "2019-10-03"},
		{"placeholder 2200, issue date upcoming", libItem{ReleaseDate: "2200-01-01", IssueDate: "2027-01-10"}, true, "2027-01-10"},
		{"both placeholders", libItem{ReleaseDate: "2200-01-01", IssueDate: "2200-01-01"}, false, ""},
		{"year 2100 exactly", libItem{ReleaseDate: "2100-06-01"}, false, ""},
		{"more than two years ahead", libItem{ReleaseDate: "2029-01-01"}, false, ""},
		{"just under two years ahead", libItem{ReleaseDate: "2028-10-01"}, true, "2028-10-01"},
		{"a real preorder", libItem{ReleaseDate: "2027-02-14"}, true, "2027-02-14"},
		{"released", libItem{ReleaseDate: "2016-08-01"}, false, "2016-08-01"},
		{"timestamp form", libItem{ReleaseDate: "2200-01-01T00:00:00Z", IssueDate: "2020-05-05"}, false, "2020-05-05"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.it.ASIN, tc.it.Title = "B0VICTORIA", "Victorian Secrets"
			rec := tc.it.toRecord(now)
			if rec.Preorder != tc.wantPreorder || (rec.UnavailableReason != "") != tc.wantPreorder {
				t.Fatalf("preorder = %v (reason %q), want %v", rec.Preorder, rec.UnavailableReason, tc.wantPreorder)
			}
			if rec.ReleaseDate != tc.wantDate {
				t.Fatalf("release date = %q, want %q", rec.ReleaseDate, tc.wantDate)
			}
			wantYear := ""
			if tc.wantDate != "" {
				wantYear = tc.wantDate[:4]
			}
			if rec.Year != wantYear {
				t.Fatalf("year = %q, want %q", rec.Year, wantYear)
			}
		})
	}
}

// TestUnavailableCheckOrder: a show or periodical is checked first, then a
// plausible release date ahead (a pre-order, whatever its rights say: Audible
// grants none to a title not out yet), then the rights.
func TestUnavailableCheckOrder(t *testing.T) {
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	soon := "2027-01-01"
	show := libItem{ReleaseDate: soon, ContentDeliveryType: "PodcastParent"}
	if r, pre := show.unavailableReason(now); !strings.Contains(r, "periodical") || pre {
		t.Fatalf("upcoming show = %q, preorder %v", r, pre)
	}
	if rec := show.toRecord(now); rec.Preorder {
		t.Fatal("an upcoming show's record is a preorder")
	}
	noRights := libItem{ReleaseDate: soon, CustomerRights: &customerRights{}}
	if r, pre := noRights.unavailableReason(now); r != PreorderReason || !pre {
		t.Fatalf("upcoming title without rights = %q, preorder %v, want a pre-order", r, pre)
	}
	upcoming := libItem{ReleaseDate: soon, CustomerRights: &customerRights{IsConsumableOffline: true}}
	if r, pre := upcoming.unavailableReason(now); r != PreorderReason || !pre {
		t.Fatalf("upcoming title = %q, preorder %v", r, pre)
	}
	// Released, no rights: unavailable for its rights, not a pre-order.
	out := libItem{ReleaseDate: "2026-01-01", CustomerRights: &customerRights{}}
	if r, pre := out.unavailableReason(now); !strings.Contains(r, "download rights") || pre {
		t.Fatalf("released title without rights = %q, preorder %v", r, pre)
	}
	// A placeholder date far ahead is no date: the rights decide.
	placeholder := libItem{ReleaseDate: "2200-01-01", CustomerRights: &customerRights{}}
	if r, pre := placeholder.unavailableReason(now); !strings.Contains(r, "download rights") || pre {
		t.Fatalf("placeholder-dated title without rights = %q, preorder %v", r, pre)
	}
}

// TestLibraryLogsOddDate: a title whose date was set aside logs its raw
// fields once, and the log carries no credential.
func TestLibraryLogsOddDate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[
			{"asin":"B0VICTORIA","title":"Victorian Secrets","release_date":"2200-01-01","issue_date":"2200-01-01",
			 "content_delivery_type":"SinglePartBook","customer_rights":{"is_consumable_offline":true}},
			{"asin":"B0NORMAL01","title":"Soulsmith","release_date":"2016-08-01"}
		]}`))
	}))
	defer ts.Close()
	var buf bytes.Buffer
	keyPEM, _ := pemKey(t, false)
	c, err := NewClient(Credentials{
		ADPToken: "ADP-SECRET", DevicePrivateKey: keyPEM, DeviceType: deviceType,
		DeviceSerial: "SER", CustomerID: "CUST-SECRET", Marketplace: "us",
	}, WithEndpoints(Endpoints{Amazon: ts.URL, AmazonAPI: ts.URL, API: ts.URL}), noSleep(),
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	if err != nil {
		t.Fatal(err)
	}
	recs, err := c.Library(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Preorder || recs[0].UnavailableReason != "" {
		t.Fatalf("records = %+v", recs)
	}
	out := buf.String()
	if n := strings.Count(out, "implausible"); n != 1 {
		t.Fatalf("logged %d odd titles, want 1:\n%s", n, out)
	}
	for _, want := range []string{"B0VICTORIA", "release_date=2200-01-01", "issue_date=2200-01-01",
		"content_delivery_type=SinglePartBook", "is_consumable_offline=true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"ADP-SECRET", "CUST-SECRET", "SER", "PRIVATE KEY"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log carries %q:\n%s", secret, out)
		}
	}
}
