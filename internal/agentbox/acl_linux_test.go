package agentbox

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// defaultACL is a POSIX default ACL (the kernel's xattr form) that gives uid
// read and search on everything made in the dir it is set on.
func defaultACL(uid uint32) []byte {
	const (
		userObj, user, groupObj, mask, other = 0x01, 0x02, 0x04, 0x10, 0x20
		undefined                            = 0xffffffff
	)
	b := binary.LittleEndian.AppendUint32(nil, 2) // the xattr's version
	for _, e := range []struct {
		tag, perm uint16
		id        uint32
	}{{userObj, 7, undefined}, {user, 5, uid}, {groupObj, 5, undefined}, {mask, 7, undefined}, {other, 5, undefined}} {
		b = binary.LittleEndian.AppendUint16(b, e.tag)
		b = binary.LittleEndian.AppendUint16(b, e.perm)
		b = binary.LittleEndian.AppendUint32(b, e.id)
	}
	return b
}

// hasACL reports whether path carries an access ACL.
func hasACL(t *testing.T, path string) bool {
	t.Helper()
	_, err := unix.Getxattr(path, "system.posix_acl_access", nil)
	if err != nil && !errors.Is(err, unix.ENODATA) {
		t.Fatal(err)
	}
	return err == nil
}

// A staging dir with a default ACL hands its named users and groups every
// file made under it, readable by them under the mode's mask: the credential
// handed to the Bridge's group drops what it inherited (denied: the uid the
// ACL names), and keeps its modes (allowed: the group still reads it).
func TestHandoffCredentialDropsInheritedACL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := unix.Setxattr(dir, "system.posix_acl_default", defaultACL(54321), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skip("no POSIX ACLs on this filesystem:", err)
		}
		t.Fatal(err)
	}
	control := filepath.Join(dir, "control")
	mustWrite(t, control, "x")
	if !hasACL(t, control) {
		t.Skip("this filesystem doesn't apply a default ACL")
	}
	job := filepath.Join(dir, "00000000000000aa")
	handed := &agentproto.Credential{CLI: agent.IDClaude, Kind: agentproto.CredentialOAuthToken, Value: "sk-ant-oat01-vault"}
	root, groupErr, err := stage(job, agent.Request{Dir: stagedDir(t), Prompt: "p"}, agent.IDClaude, 60, handed, os.Getegid(), false)
	if err != nil || groupErr != nil {
		t.Fatal(err, groupErr)
	}
	_ = root.Close()
	if !hasACL(t, filepath.Join(job, agentproto.PromptFile)) {
		t.Fatal("the job's other files didn't inherit the default ACL: the test proves nothing")
	}
	credDir := filepath.Join(job, agentproto.CredentialDir)
	credFile := filepath.Join(credDir, agentproto.CredentialFile)
	for _, p := range []string{credDir, credFile} {
		if hasACL(t, p) {
			t.Fatalf("%s kept the ACL it inherited", filepath.Base(p))
		}
	}
	d, derr := os.Stat(credDir)
	f, ferr := os.Stat(credFile)
	if derr != nil || ferr != nil || d.Mode() != fs.ModeDir|credentialGroupDirMode || f.Mode() != credentialGroupFileMode {
		t.Fatalf("modes = %v %v, %v %v", d, derr, f, ferr)
	}
}
