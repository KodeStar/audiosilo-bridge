package podcast

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// withLink replaces the hard link for one test.
func withLink(t *testing.T, fn func(root *os.Root, oldname, newname string) error) {
	t.Helper()
	saved := linkFile
	linkFile = fn
	t.Cleanup(func() { linkFile = saved })
}

func TestInstallFallsBackToNoReplaceRenameWithoutHardLinks(t *testing.T) {
	for _, unsupported := range unsupportedLinkErrors {
		t.Run(unsupported.Error(), func(t *testing.T) {
			root, j := journalFixture(t)
			withLink(t, func(_ *os.Root, oldname, newname string) error {
				return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: unsupported}
			})
			if err := Publish(root, j, time.Now(), nil); err != nil {
				t.Fatal(err)
			}
			if !Matches(root, j.Path, &j.Fetch) {
				t.Fatal("renamed audio does not match its pin")
			}
			if st, err := root.Lstat(j.Path); err != nil || st.Mode().Perm() != 0o644 {
				t.Fatal("installed audio mode", st, err)
			}
			for _, gone := range []string{"Fixture Show/" + j.AudioTemp, AttachmentPath(j.Path, ".publish.json")} {
				if _, err := root.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("left behind", gone, err)
				}
			}
		})
	}
}

func TestInstallFallbackRefusesAnExistingTarget(t *testing.T) {
	root, j := journalFixture(t)
	foreign := []byte("an administrator's own file")
	// The target appears between the absence checks and the install, then the
	// filesystem reports it has no hard links.
	withLink(t, func(r *os.Root, oldname, newname string) error {
		if err := r.WriteFile(newname, foreign, 0o644); err != nil {
			t.Fatal(err)
		}
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: unsupportedLinkErrors[0]}
	})
	if err := Publish(root, j, time.Now(), nil); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("fallback replaced or failed oddly:", err)
	}
	if got, err := root.ReadFile(j.Path); err != nil || !bytes.Equal(got, foreign) {
		t.Fatal("foreign file overwritten", err)
	}
	// The candidate and the barrier stay for an administrator's repair; recovery
	// fails closed rather than adopting or replacing the foreign file.
	if !Matches(root, "Fixture Show/"+j.AudioTemp, &j.Fetch) {
		t.Fatal("staged candidate lost")
	}
	if _, err := root.Lstat(AttachmentPath(j.Path, ".publish.json")); err != nil {
		t.Fatal("journal removed", err)
	}
	if err := RecoverShow(root, "Fixture Show", time.Now(), nil); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("recovery adopted a foreign file:", err)
	}
	if got, _ := root.ReadFile(j.Path); !bytes.Equal(got, foreign) {
		t.Fatal("recovery overwrote the foreign file")
	}
}

func TestNoReplaceRenameRefusesExistingTarget(t *testing.T) {
	root, _ := journalFixture(t)
	for name, rename := range map[string]func(*os.Root, string, string) error{"platform": renameNoReplace, "portable": renameIfAbsent} {
		t.Run(name, func(t *testing.T) {
			if err := root.WriteFile("Fixture Show/a.mp3", []byte("staged"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := root.WriteFile("Fixture Show/b.mp3", []byte("existing"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := rename(root, "Fixture Show/a.mp3", "Fixture Show/b.mp3"); !errors.Is(err, ErrInvalidFile) {
				t.Fatal("replaced an existing target:", err)
			}
			if got, _ := root.ReadFile("Fixture Show/b.mp3"); string(got) != "existing" {
				t.Fatal("target overwritten")
			}
			if err := root.Remove("Fixture Show/b.mp3"); err != nil {
				t.Fatal(err)
			}
			if err := rename(root, "Fixture Show/a.mp3", "Fixture Show/b.mp3"); err != nil {
				t.Fatal("absent target refused:", err)
			}
			if got, _ := root.ReadFile("Fixture Show/b.mp3"); string(got) != "staged" {
				t.Fatal("rename lost bytes")
			}
			if _, err := root.Lstat("Fixture Show/a.mp3"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("source kept", err)
			}
			_ = root.Remove("Fixture Show/b.mp3")
		})
	}
}

func TestRecoveryAfterFallbackRenameBeforeFeedCommit(t *testing.T) {
	root, j := journalFixture(t)
	withLink(t, func(_ *os.Root, oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: unsupportedLinkErrors[0]}
	})
	crash := errors.New("interrupted after rename")
	if err := Publish(root, j, time.Now(), func(at string) error {
		if at == "linked" {
			return crash
		}
		return nil
	}); !errors.Is(err, crash) {
		t.Fatal(err)
	}
	if _, err := root.Lstat("Fixture Show/" + j.AudioTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("renamed temp still present", err)
	}
	if err := RecoverShow(root, "Fixture Show", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	f, err := Read(root, "Fixture Show")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.Episodes {
		if e.Path == j.Path && (e.Fetch == nil || *e.Fetch != j.Fetch) {
			t.Fatal("recovery did not commit the same generation", e.Fetch)
		}
	}
	if !Matches(root, j.Path, &j.Fetch) {
		t.Fatal("committed audio lost")
	}
	if _, err := root.Lstat(AttachmentPath(j.Path, ".publish.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("journal kept", err)
	}
}

func TestInstallDoesNotFallBackOnOtherLinkErrors(t *testing.T) {
	root, j := journalFixture(t)
	refused := &os.LinkError{Op: "link", Err: syscall.EEXIST}
	withLink(t, func(*os.Root, string, string) error { return refused })
	if err := Publish(root, j, time.Now(), nil); !errors.Is(err, syscall.EEXIST) {
		t.Fatal(err)
	}
	if _, err := root.Lstat(j.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("installed without a link", err)
	}
	if !Matches(root, "Fixture Show/"+j.AudioTemp, &j.Fetch) {
		t.Fatal("staged candidate lost")
	}
}
