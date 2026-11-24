package transfer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Sweep finds nothing in a missing folder, shows inspect every regular file
// before the first removal, and joins the removals' errors.
func TestPrivateDirSweep(t *testing.T) {
	missing := PrivateDir(filepath.Join(t.TempDir(), "none"))
	if err := missing.Sweep(nil, func(string, fs.DirEntry) error {
		t.Error("swept an entry of a missing folder")
		return nil
	}); err != nil {
		t.Fatalf("a missing folder: %v", err)
	}

	dir := PrivateDir(filepath.Join(t.TempDir(), "private"))
	if err := dir.Create(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir.Base("1", "2")+".json", "{}")
	writeFile(t, dir.Base("acct", "B0X")+".aaxc.part", "x")
	if err := os.Mkdir(filepath.Join(string(dir), "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	var inspected, swept []string
	boom := errors.New("boom")
	err := dir.Sweep(func(file string, e fs.DirEntry) {
		if len(swept) != 0 {
			t.Errorf("inspected %s after a removal", e.Name())
		}
		inspected = append(inspected, e.Name())
	}, func(file string, e fs.DirEntry) error {
		if file != filepath.Join(string(dir), e.Name()) {
			t.Errorf("swept %s as %s", e.Name(), file)
		}
		swept = append(swept, e.Name())
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("Sweep = %v, want the removals' errors", err)
	}
	if want := []string{"1-2.json", "acct-B0X.aaxc.part"}; !slices.Equal(inspected, want) {
		t.Errorf("inspected %v, want the regular files %v", inspected, want)
	}
	if want := []string{"1-2.json", "acct-B0X.aaxc.part", "sub"}; !slices.Equal(swept, want) {
		t.Errorf("swept %v, want every entry %v", swept, want)
	}
}

// PrepareProjectDir makes the parent and the staging folder, uses one already
// there again, and refuses anything else at the staging path.
func TestPrepareProjectDir(t *testing.T) {
	mount := t.TempDir()
	root, err := os.OpenRoot(mount)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, stage := range []string{LibrivoxStageName(1), "An Author/" + LibrivoxStageName(2)} {
		for range 2 {
			if rel, err := PrepareProjectDir(root, stage); err != nil || rel != stage {
				t.Fatalf("PrepareProjectDir(%s) = %q, %v", stage, rel, err)
			}
		}
		if info, err := os.Lstat(filepath.Join(mount, filepath.FromSlash(stage))); err != nil || !info.IsDir() {
			t.Fatalf("%s isn't a folder: %v", stage, err)
		}
	}

	file := LibrivoxStageName(3)
	writeFile(t, filepath.Join(mount, file), "x")
	if rel, err := PrepareProjectDir(root, file); !errors.Is(err, ErrStagingDir) || rel != file {
		t.Errorf("a file at the staging path: %q, %v", rel, err)
	}
	link := LibrivoxStageName(4)
	if err := os.Symlink(filepath.Join(mount, "An Author"), filepath.Join(mount, link)); err != nil {
		t.Fatal(err)
	}
	if rel, err := PrepareProjectDir(root, link); !errors.Is(err, ErrStagingDir) || rel != link {
		t.Errorf("a link at the staging path: %q, %v", rel, err)
	}
	writeFile(t, filepath.Join(mount, "Taken"), "x")
	if rel, err := PrepareProjectDir(root, "Taken/"+LibrivoxStageName(5)); err == nil || errors.Is(err, ErrStagingDir) || rel != "Taken" {
		t.Errorf("a file at the parent: %q, %v", rel, err)
	}
}

// StagedProjects finds staging folders named exactly for a project in the
// library's top folder and its visible author folders, and follows no link.
func TestStagedProjects(t *testing.T) {
	mount := t.TempDir()
	for _, d := range []string{
		LibrivoxStageName(1),
		"An Author/" + LibrivoxStageName(2),
		"An Author/Book/" + LibrivoxStageName(3), // deeper than a staging folder goes
		".hidden/" + LibrivoxStageName(4),
		LibrivoxStagePrefix + "05", LibrivoxStagePrefix + "0", LibrivoxStagePrefix + "x",
	} {
		if err := os.MkdirAll(filepath.Join(mount, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(mount, LibrivoxStageName(6)), "a file")
	for name, to := range map[string]string{LibrivoxStageName(7): LibrivoxStageName(1), "Linked": "An Author"} {
		if err := os.Symlink(filepath.Join(mount, to), filepath.Join(mount, name)); err != nil {
			t.Fatal(err)
		}
	}
	got := StagedProjects(mount, time.Minute)
	slices.SortFunc(got, func(a, b StagedProject) int { return a.ID - b.ID })
	want := []StagedProject{{Path: LibrivoxStageName(1), ID: 1}, {Path: "An Author/" + LibrivoxStageName(2), ID: 2}}
	if !slices.Equal(got, want) {
		t.Errorf("StagedProjects = %v, want %v", got, want)
	}
	if got := StagedProjects(filepath.Join(mount, "none"), time.Minute); got != nil {
		t.Errorf("a missing library: %v", got)
	}
}
