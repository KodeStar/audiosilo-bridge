package podcast

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// Verify tells a file proven to hold other content (no error) from one it
// could not read to tell (the error): only the first is a changed file.
func TestVerifyTellsAChangedFileFromAnUnreadOne(t *testing.T) {
	root, j := journalFixture(t)
	temp := "Fixture Show/" + j.AudioTemp
	if ok, err := Verify(root, temp, j.Fetch.Bytes, j.Fetch.SHA256); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := Verify(root, temp, j.Fetch.Bytes, strings.Repeat("0", 64)); ok || err != nil {
		t.Fatal("another hash:", ok, err)
	}
	if ok, err := Verify(root, temp, j.Fetch.Bytes+1, j.Fetch.SHA256); ok || err != nil {
		t.Fatal("another size:", ok, err)
	}
	if ok, err := Verify(root, "Gone Show/"+j.AudioTemp, j.Fetch.Bytes, j.Fetch.SHA256); ok || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("not there:", ok, err)
	}
	if err := os.Symlink(j.AudioTemp, root.Name()+"/Fixture Show/linked"); err == nil {
		if ok, err := Verify(root, "Fixture Show/linked", j.Fetch.Bytes, j.Fetch.SHA256); ok || err == nil {
			t.Fatal("a link:", ok, err)
		}
	}
	if Holds(root, "Gone Show/"+j.AudioTemp, j.Fetch.Bytes, j.Fetch.SHA256) || !Holds(root, temp, j.Fetch.Bytes, j.Fetch.SHA256) {
		t.Fatal("Holds does not follow Verify")
	}
}
